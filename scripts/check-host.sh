#!/bin/sh
#
# check-host.sh: check whether this machine's limits and sysctls can carry an
# s5cmd run at the given settings. It only reads /proc, /sys, ulimit and
# sysctl; it never changes anything. Fix commands are printed, not run.
#
# Usage:
#   scripts/check-host.sh [--numworkers N] [--concurrency N] [--part-size MiB]
#                         [--bandwidth Gbit] [--rtt ms] [--streams N]
#
#   --numworkers N    global --numworkers of s5cmd (default 256)
#   --concurrency N   --concurrency of cp/mv/sync, parts per file (default 5)
#   --part-size MiB   --part-size of cp/mv/sync (default 50)
#   --bandwidth Gbit  link speed to the object store in Gbit/s (optional)
#   --rtt ms          round trip time to the object store in ms (optional)
#   --streams N       uploads read from stdin at the same time (default 1);
#                     only these buffer whole parts in memory
#
# --bandwidth and --rtt must be given together; without them the socket
# buffer check is skipped.
#
# Exit status: 0 when nothing FAILed (WARN is fine), 1 when something FAILed,
# 2 on a usage error.
#
# Where the defaults and formulas come from (s5cmd code of this repository):
#   numworkers 256         command/app.go, defaultWorkerCount
#   concurrency 5          command/cp.go, defaultCopyConcurrency
#   part size 50 MiB       command/cp.go, defaultPartSize
#   connections            command/app.go, connPoolSize: one idle connection per
#                          worker times the multipart concurrency, at most 2048
#                          idle. Active connections are not capped
#                          (storage/s3.go sets MaxIdleConns and
#                          MaxIdleConnsPerHost only), so the upper bound for
#                          open sockets is numworkers x concurrency.
#   open file limit        On Linux the Go runtime raises the soft limit to the
#                          hard limit at startup. parallel/fdlimit/
#                          fdlimit_unix.go, Raise, is the fallback (macOS): the
#                          soft limit goes to min(hard, 65536), called from
#                          parallel.Init.
#
# Output: one line per check, "LEVEL name: details", and for WARN and FAIL an
# indented "fix:" line. Levels are PASS, WARN, FAIL, INFO and SKIP.

# Defaults, see the table above.
NUMWORKERS=256
CONCURRENCY=5
PART_MIB=50
STREAMS=1
BANDWIDTH_GBIT=
RTT_MS=

# s5cmd raises the soft limit at most to this value (fdlimit_unix.go,
# maxOpenFilesLimit) and never below this one (minOpenFilesLimit).
S5CMD_FD_CAP=65536
S5CMD_FD_MIN=1024

# macOS accepts at most OPEN_MAX when the hard limit is "unlimited"
# (fdlimit_unix.go, darwinOpenMax). s5cmd falls back to it when it cannot set
# the capped value.
DARWIN_OPEN_MAX=10240

# Extra descriptors on top of connections and local files: stdin, stdout,
# stderr, the credentials and CA files, DNS sockets, the listing connection of
# sync/ls/du, the log. This is a rule of thumb, not a value from the code.
FD_MARGIN=128

# Memory s5cmd needs besides part buffers: BASE_MEM_MIB plus
# numworkers / WORKERS_PER_MIB. Measured peak RSS (VmHWM) of s5cmd v2.6.0 for
# 20,000 files of 64 KiB, upload and download: 30 to 42 MiB with 4 workers,
# 34 to 45 with 16, 38 to 50 with 64, 59 to 76 with 256 and 115 to 147 with
# 1024. A 4 GiB multipart upload from a file at 5 parts used 34 MiB. This
# formula (32 + numworkers / 6 MiB) is a little above all of them.
BASE_MEM_MIB=32
WORKERS_PER_MIB=6

# A closed TCP connection keeps its local port in TIME_WAIT for 60 seconds
# (TCP_TIMEWAIT_LEN in include/net/tcp.h of the Linux kernel).
TIME_WAIT_S=60

usage() {
	sed -n '3,24p' "$0" | sed 's/^# \{0,1\}//'
}

die_usage() {
	printf 'check-host.sh: %s\n' "$1" >&2
	printf 'Try: %s --help\n' "$0" >&2
	exit 2
}

is_uint() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$1" -gt 0 ] 2>/dev/null
}

is_decimal() {
	# positive decimal such as 10, 0.5 or 12.25
	case $1 in
	'' | *[!0-9.]* | *.*.* | .) return 1 ;;
	esac
	awk -v v="$1" 'BEGIN { exit !(v + 0 > 0) }'
}

while [ $# -gt 0 ]; do
	case $1 in
	--numworkers | --concurrency | --part-size | --bandwidth | --rtt | --streams)
		[ $# -ge 2 ] || die_usage "$1 needs a value"
		opt=$1
		val=$2
		shift 2
		case $opt in
		--numworkers)
			is_uint "$val" || die_usage "--numworkers must be a positive integer"
			NUMWORKERS=$val
			;;
		--concurrency)
			is_uint "$val" || die_usage "--concurrency must be a positive integer"
			CONCURRENCY=$val
			;;
		--part-size)
			is_uint "$val" || die_usage "--part-size must be a positive integer (MiB)"
			PART_MIB=$val
			;;
		--streams)
			is_uint "$val" || die_usage "--streams must be a positive integer"
			STREAMS=$val
			;;
		--bandwidth)
			is_decimal "$val" || die_usage "--bandwidth must be a positive number (Gbit/s)"
			BANDWIDTH_GBIT=$val
			;;
		--rtt)
			is_decimal "$val" || die_usage "--rtt must be a positive number (ms)"
			RTT_MS=$val
			;;
		esac
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die_usage "unknown option: $1"
		;;
	esac
done

if { [ -n "$BANDWIDTH_GBIT" ] && [ -z "$RTT_MS" ]; } || { [ -z "$BANDWIDTH_GBIT" ] && [ -n "$RTT_MS" ]; }; then
	die_usage "--bandwidth and --rtt must be given together"
fi

OS=$(uname -s 2>/dev/null || echo unknown)
FAILS=0
WARNS=0
# Set by check_open_files, used for the suggested command at the end.
EFF_FD=

# emit LEVEL NAME MESSAGE [FIX]
emit() {
	printf '%-4s %s: %s\n' "$1" "$2" "$3"
	if [ -n "${4:-}" ]; then
		printf '     fix: %s\n' "$4"
	fi
	case $1 in
	FAIL) FAILS=$((FAILS + 1)) ;;
	WARN) WARNS=$((WARNS + 1)) ;;
	esac
}

# human BYTES -> "12.3 MiB"
human() {
	awk -v b="$1" 'BEGIN {
		if (b >= 1073741824) printf "%.1f GiB", b / 1073741824
		else if (b >= 1048576) printf "%.1f MiB", b / 1048576
		else if (b >= 1024) printf "%.1f KiB", b / 1024
		else printf "%d B", b
	}'
}

# proc_read FILE -> contents, empty when unreadable
proc_read() {
	cat "$1" 2>/dev/null
}

# Concurrent sockets to the object store. Upper bound, see the header.
CONNS=$((NUMWORKERS * CONCURRENCY))
LOCAL_FILES=$NUMWORKERS
FD_NEED=$((CONNS + LOCAL_FILES + FD_MARGIN))

printf 'Settings: numworkers=%s concurrency=%s part-size=%s MiB streams=%s' \
	"$NUMWORKERS" "$CONCURRENCY" "$PART_MIB" "$STREAMS"
if [ -n "$BANDWIDTH_GBIT" ]; then
	printf ' bandwidth=%s Gbit/s rtt=%s ms' "$BANDWIDTH_GBIT" "$RTT_MS"
fi
printf '\nUpper bound of open sockets: %s (numworkers x concurrency). Descriptors needed: %s (sockets + %s local files + %s margin).\n\n' \
	"$CONNS" "$FD_NEED" "$LOCAL_FILES" "$FD_MARGIN"

# ---------------------------------------------------------------------------
# 1. Open files.
#
# getrlimit(2), RLIMIT_NOFILE: the soft limit is what the kernel enforces, the
# hard limit is the ceiling an unprivileged process can raise the soft limit
# to. s5cmd raises the soft limit itself (see header), so the hard limit is
# the number that matters: the soft limit after startup is
#   max(soft, min(hard, 65536))
# fs.nr_open (proc(5), also kernel documentation fs.rst) is the highest value
# a limit can be set to; fs.file-max is the system-wide number of open file
# handles. Both are only checked on Linux.
# ---------------------------------------------------------------------------
check_open_files() {
	# ulimit -S and -H are not in POSIX but dash, bash, busybox and the macOS
	# sh all have them.
	# shellcheck disable=SC3045
	soft=$(ulimit -Sn 2>/dev/null)
	# shellcheck disable=SC3045
	hard=$(ulimit -Hn 2>/dev/null)
	if [ -z "$soft" ] || [ -z "$hard" ]; then
		emit SKIP open-files "ulimit -n is not available in this shell"
		return
	fi

	# "unlimited" means no ceiling; use a number that is above any need.
	soft_n=$soft
	hard_n=$hard
	[ "$soft" = unlimited ] && soft_n=1073741824
	[ "$hard" = unlimited ] && hard_n=1073741824

	# What happens at startup. On Linux the Go runtime (os package, since
	# Go 1.19) already raises the soft limit to the hard limit before main
	# runs. We measured s5cmd v2.6.0 in containers: with a hard limit of
	# 512, 4096, 100000 and 1048576 the soft limit ended at 511, 4096, 99999
	# and 1048575. s5cmd's own Raise (parallel/fdlimit/fdlimit_unix.go) is the
	# fallback for systems where the runtime does not do it, such as macOS.
	# It raises the soft limit to min(hard, 65536), but only when that is at
	# least 1024.
	if [ "$OS" = Linux ]; then
		eff=$hard_n
	else
		target=$S5CMD_FD_CAP
		[ "$hard_n" -lt "$target" ] && target=$hard_n
		if [ "$OS" = Darwin ]; then
			# Raise falls back to darwinOpenMax when the capped value is
			# refused. kern.maxfilesperproc is the per-process ceiling.
			perproc=$(sysctl -n kern.maxfilesperproc 2>/dev/null)
			if is_uint "$perproc" && [ "$perproc" -lt "$target" ]; then
				target=$DARWIN_OPEN_MAX
				[ "$hard_n" -lt "$target" ] && target=$hard_n
			fi
		fi
		eff=$soft_n
		if [ "$target" -ge "$S5CMD_FD_MIN" ] && [ "$soft_n" -lt "$target" ]; then
			eff=$target
		fi
	fi

	EFF_FD=$eff
	desc="soft $soft, hard $hard, effective after startup: $eff, needed: $FD_NEED"
	fix_sys="ulimit -n $FD_NEED (shell), LimitNOFILE=$FD_NEED (systemd unit), --ulimit nofile=$FD_NEED:$FD_NEED (docker run), or a 'nofile' line in /etc/security/limits.conf"
	if [ "$eff" -ge "$FD_NEED" ]; then
		if [ "$soft_n" -ge "$FD_NEED" ]; then
			emit PASS open-files "$desc"
		else
			emit PASS open-files "$desc (the soft limit is raised at startup)"
		fi
	elif [ "$hard_n" -ge "$FD_NEED" ]; then
		emit WARN open-files "$desc" \
			"s5cmd raises the soft limit to at most $S5CMD_FD_CAP here; run 'ulimit -n $FD_NEED' before s5cmd, or lower --numworkers x --concurrency"
	else
		emit FAIL open-files "$desc" \
			"raise the hard limit: $fix_sys; or lower --numworkers x --concurrency"
	fi

	if [ "$OS" = Linux ]; then
		nr_open=$(proc_read /proc/sys/fs/nr_open)
		if is_uint "$nr_open"; then
			if [ "$nr_open" -ge "$FD_NEED" ]; then
				emit PASS fs.nr_open "current $nr_open, needed $FD_NEED"
			else
				emit FAIL fs.nr_open "current $nr_open, needed $FD_NEED" \
					"sysctl -w fs.nr_open=$FD_NEED (a limit cannot be set above this value)"
			fi
		fi
		file_max=$(proc_read /proc/sys/fs/file-max)
		if is_uint "$file_max"; then
			if [ "$file_max" -ge "$FD_NEED" ]; then
				emit PASS fs.file-max "current $file_max, needed $FD_NEED (system-wide, shared with every process)"
			else
				emit FAIL fs.file-max "current $file_max, needed $FD_NEED (system-wide)" \
					"sysctl -w fs.file-max=$((FD_NEED * 4))"
			fi
		fi
	elif [ "$OS" = Darwin ]; then
		maxfiles=$(sysctl -n kern.maxfiles 2>/dev/null)
		if is_uint "$maxfiles"; then
			if [ "$maxfiles" -ge "$FD_NEED" ]; then
				emit PASS kern.maxfiles "current $maxfiles, needed $FD_NEED (system-wide)"
			else
				emit FAIL kern.maxfiles "current $maxfiles, needed $FD_NEED (system-wide)" \
					"sudo sysctl -w kern.maxfiles=$((FD_NEED * 4))"
			fi
		fi
		if is_uint "$perproc"; then
			if [ "$perproc" -ge "$FD_NEED" ]; then
				emit PASS kern.maxfilesperproc "current $perproc, needed $FD_NEED"
			else
				emit FAIL kern.maxfilesperproc "current $perproc, needed $FD_NEED" \
					"sudo sysctl -w kern.maxfilesperproc=$FD_NEED"
			fi
		fi
	fi
}

# ---------------------------------------------------------------------------
# 2. Ephemeral ports (Linux).
#
# Every TCP connection to one destination address and port needs its own local
# port. Linux picks it from net.ipv4.ip_local_port_range (ip-sysctl.rst); the
# default 32768 60999 gives 28232 ports. A range smaller than the number of
# connections makes connect() fail with EADDRNOTAVAIL. The range is per
# destination (address, port), so an endpoint behind several IPs needs less,
# but one IP is the worst case and what MinIO or RustFS on one host looks like.
#
# Headroom: we ask for twice the connections because closed connections keep
# their port in TIME_WAIT for 60 s (TCP_TIMEWAIT_LEN). That factor is a rule
# of thumb, not a kernel value.
#
# net.ipv4.tcp_tw_reuse (ip-sysctl.rst): 0 = off, 1 = reuse TIME_WAIT sockets
# for new outgoing connections when the protocol allows it, 2 = only for
# loopback (the default since Linux 4.19 or so). It matters when the server
# closes connections often (for example an idle timeout shorter than the
# gaps between requests, or Connection: close) and s5cmd opens many new ones:
# without reuse the sustainable rate is span / 60 s new connections per second.
# ---------------------------------------------------------------------------
check_ports() {
	range=$(proc_read /proc/sys/net/ipv4/ip_local_port_range)
	if [ -z "$range" ]; then
		emit SKIP ephemeral-ports "net.ipv4.ip_local_port_range is not readable"
		return
	fi
	# shellcheck disable=SC2086
	set -- $range
	lo=$1
	hi=$2
	span=$((hi - lo + 1))
	desc="net.ipv4.ip_local_port_range $lo $hi = $span ports, needed: $CONNS (x2 headroom: $((CONNS * 2)))"
	# Widest useful range: 1024 to 65535 (below 1024 are privileged ports).
	newrange_lo=$((65536 - CONNS * 2))
	if [ "$newrange_lo" -lt 1024 ]; then
		fix="one server address can take at most about 64000 connections: lower --numworkers x --concurrency, or spread the load over several endpoint addresses; sysctl -w net.ipv4.ip_local_port_range=\"1024 65535\" is the widest range"
	else
		fix="sysctl -w net.ipv4.ip_local_port_range=\"$newrange_lo 65535\" (or lower --numworkers x --concurrency)"
	fi
	if [ "$span" -lt "$CONNS" ]; then
		emit FAIL ephemeral-ports "$desc" "$fix"
	elif [ "$span" -lt $((CONNS * 2)) ]; then
		emit WARN ephemeral-ports "$desc" "$fix"
	else
		emit PASS ephemeral-ports "$desc"
	fi

	tw=$(proc_read /proc/sys/net/ipv4/tcp_tw_reuse)
	if [ -n "$tw" ]; then
		rate=$((span / TIME_WAIT_S))
		case $tw in
		1)
			emit PASS tcp_tw_reuse "current 1 (reuse TIME_WAIT ports for new outgoing connections)"
			;;
		2)
			emit INFO tcp_tw_reuse "current 2 (loopback only). Without reuse the range sustains about $rate new connections/s to one server. s5cmd keeps connections alive, so this only matters if the server closes them often"
			;;
		*)
			emit WARN tcp_tw_reuse "current $tw (off). The range sustains about $rate new connections/s to one server; many short connections can run out of ports" \
				"sysctl -w net.ipv4.tcp_tw_reuse=1"
			;;
		esac
	fi
}

# ---------------------------------------------------------------------------
# 3. Socket buffers (Linux, only with --bandwidth and --rtt).
#
# To keep a link full a TCP stream needs a window of at least the
# bandwidth-delay product, BDP = bandwidth x RTT. s5cmd is written in Go,
# which does not call setsockopt(SO_RCVBUF/SO_SNDBUF), so Linux auto-tunes
# the buffers up to the third value of net.ipv4.tcp_rmem and tcp_wmem
# (ip-sysctl.rst). net.core.rmem_max and wmem_max only cap applications that
# set the buffer size themselves, so they are reported as INFO.
#
# One large file is transferred by --concurrency parts in parallel, so each
# stream needs BDP / concurrency. A receive buffer also holds packet overhead,
# so only part of it is usable as window (tcp_adv_win_scale in ip-sysctl.rst);
# we ask for 2 x that share on the receive side. The factor 2 is a rule of
# thumb, not a kernel constant.
# ---------------------------------------------------------------------------
check_buffers() {
	if [ -z "$BANDWIDTH_GBIT" ]; then
		emit SKIP socket-buffers "give --bandwidth and --rtt to check them"
		return
	fi
	bdp=$(awk -v bw="$BANDWIDTH_GBIT" -v rtt="$RTT_MS" 'BEGIN { printf "%.0f", bw * 1e9 / 8 * rtt / 1000 }')
	share=$((bdp / CONCURRENCY))
	[ "$share" -lt 1 ] && share=1
	need_rcv=$((share * 2))
	need_snd=$share
	printf '     BDP = %s Gbit/s x %s ms = %s; per stream with %s parallel parts: %s\n' \
		"$BANDWIDTH_GBIT" "$RTT_MS" "$(human "$bdp")" "$CONCURRENCY" "$(human "$share")"

	rmem=$(proc_read /proc/sys/net/ipv4/tcp_rmem)
	wmem=$(proc_read /proc/sys/net/ipv4/tcp_wmem)
	if [ -n "$rmem" ]; then
		# shellcheck disable=SC2086
		set -- $rmem
		rmax=$3
		if [ "$rmax" -ge "$need_rcv" ]; then
			emit PASS tcp_rmem "max $(human "$rmax"), needed $(human "$need_rcv")"
		else
			emit WARN tcp_rmem "max $(human "$rmax"), needed $(human "$need_rcv")" \
				"sysctl -w net.ipv4.tcp_rmem=\"4096 131072 $need_rcv\""
		fi
	fi
	if [ -n "$wmem" ]; then
		# shellcheck disable=SC2086
		set -- $wmem
		wmax=$3
		if [ "$wmax" -ge "$need_snd" ]; then
			emit PASS tcp_wmem "max $(human "$wmax"), needed $(human "$need_snd")"
		else
			emit WARN tcp_wmem "max $(human "$wmax"), needed $(human "$need_snd")" \
				"sysctl -w net.ipv4.tcp_wmem=\"4096 16384 $need_snd\""
		fi
	fi
	rcore=$(proc_read /proc/sys/net/core/rmem_max)
	wcore=$(proc_read /proc/sys/net/core/wmem_max)
	if is_uint "$rcore" && is_uint "$wcore"; then
		emit INFO net.core.rmem_max/wmem_max "current $(human "$rcore") / $(human "$wcore"). Go does not set SO_RCVBUF/SO_SNDBUF, so s5cmd over TCP is limited by tcp_rmem/tcp_wmem, not by these"
	fi
}

# ---------------------------------------------------------------------------
# 4. Congestion control (Linux), information only.
#
# net.ipv4.tcp_congestion_control is the default for new sockets,
# tcp_available_congestion_control lists what is loaded (ip-sysctl.rst). BBR
# often does better than CUBIC on long fat links with some loss; it is not
# automatically better on a clean LAN, so nothing is flagged.
# ---------------------------------------------------------------------------
check_cc() {
	cc=$(proc_read /proc/sys/net/ipv4/tcp_congestion_control)
	avail=$(proc_read /proc/sys/net/ipv4/tcp_available_congestion_control)
	if [ -z "$cc" ]; then
		emit SKIP congestion-control "net.ipv4.tcp_congestion_control is not readable"
		return
	fi
	case " $avail " in
	*" bbr "*) bbr="bbr is available" ;;
	*) bbr="bbr is not loaded (modprobe tcp_bbr, if the kernel has it)" ;;
	esac
	emit INFO congestion-control "current $cc; available: $avail; $bbr"
}

# ---------------------------------------------------------------------------
# 5. Memory.
#
# What s5cmd holds in memory (vendor/github.com/aws/aws-sdk-go/service/s3/
# s3manager/upload.go, nextReader):
#   * uploads from a file: parts are sections of the open file (io.SectionReader
#     over ReadAt), so no part buffer is allocated. Measured: 34 MiB RSS for a
#     4 GiB upload at concurrency 5 and 50 MiB parts.
#   * uploads from stdin (s5cmd pipe, cp - ...): parts are read into buffers
#     from a pool of --concurrency + 1 slices of --part-size each
#     (upload.go, init: poolCap := Concurrency + 1). Measured: 334 MiB RSS at
#     concurrency 5 and 50 MiB parts, which is 6 x 50 MiB plus 34 MiB.
#   * downloads: each part is copied from the HTTP body into the file with a
#     small copy buffer, no part-size buffer.
# So the estimate is base + streams x part-size x (concurrency + 1). In a
# container, the cgroup limit counts instead of the machine memory.
# ---------------------------------------------------------------------------
check_memory() {
	base_mib=$((BASE_MEM_MIB + NUMWORKERS / WORKERS_PER_MIB))
	est_mib=$((base_mib + STREAMS * PART_MIB * (CONCURRENCY + 1)))
	avail_kib=
	if [ "$OS" = Linux ]; then
		avail_kib=$(awk '/^MemAvailable:/ { print $2 }' /proc/meminfo 2>/dev/null)
		if [ -r /sys/fs/cgroup/memory.max ]; then
			lim=$(proc_read /sys/fs/cgroup/memory.max)
			cur=$(proc_read /sys/fs/cgroup/memory.current)
			if is_uint "$lim" && is_uint "$cur" && [ "$lim" -gt "$cur" ]; then
				cg_kib=$(((lim - cur) / 1024))
				if [ -z "$avail_kib" ] || [ "$cg_kib" -lt "$avail_kib" ]; then
					avail_kib=$cg_kib
				fi
			fi
		fi
	elif [ "$OS" = Darwin ]; then
		total=$(sysctl -n hw.memsize 2>/dev/null)
		if is_uint "$total"; then
			avail_kib=$((total / 1024))
			note=" (total RAM, free memory is not read on macOS)"
		fi
	fi
	if ! is_uint "$avail_kib"; then
		emit SKIP memory "available memory is not readable"
		return
	fi
	avail_mib=$((avail_kib / 1024))
	desc="available ${avail_mib} MiB, estimated need $est_mib MiB ($base_mib base + $STREAMS stdin upload(s) x $PART_MIB MiB x $((CONCURRENCY + 1)) buffers)${note:-}"
	if [ "$avail_mib" -lt "$est_mib" ]; then
		emit FAIL memory "$desc" "lower --part-size or --concurrency, or run fewer stdin uploads at once"
	elif [ "$avail_mib" -lt $((est_mib * 2)) ]; then
		emit WARN memory "$desc" "lower --part-size or --concurrency, or free memory"
	else
		emit PASS memory "$desc"
	fi
}

# ---------------------------------------------------------------------------
# 6. CPU count, information only.
#
# Workers are goroutines that mostly wait for the network, so numworkers above
# the core count is normal. The CPU is the limit when the SDK hashes request
# bodies (MD5 and SHA-256 per part) and for TLS; see --unsigned-payload and
# --no-content-md5 in the README. Nothing is flagged.
# ---------------------------------------------------------------------------
check_cpu() {
	cpus=
	case $OS in
	Linux) cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null) ;;
	Darwin) cpus=$(sysctl -n hw.ncpu 2>/dev/null) ;;
	esac
	if ! is_uint "$cpus"; then
		emit SKIP cpu "CPU count is not readable"
		return
	fi
	per=$(awk -v w="$NUMWORKERS" -v c="$cpus" 'BEGIN { printf "%.1f", w / c }')
	emit INFO cpu "$cpus CPUs online, $NUMWORKERS workers = $per workers per CPU"
}

check_open_files
case $OS in
Linux)
	check_ports
	check_buffers
	check_cc
	;;
Darwin)
	emit SKIP ephemeral-ports "Linux only"
	if [ -n "$BANDWIDTH_GBIT" ]; then
		sockbuf=$(sysctl -n kern.ipc.maxsockbuf 2>/dev/null)
		bdp=$(awk -v bw="$BANDWIDTH_GBIT" -v rtt="$RTT_MS" 'BEGIN { printf "%.0f", bw * 1e9 / 8 * rtt / 1000 }')
		share=$((bdp / CONCURRENCY))
		# kern.ipc.maxsockbuf caps the size of one socket buffer on macOS.
		# macOS auto-tunes buffers up to it (net.inet.tcp.autorcvbufmax and
		# autosndbufmax apply as well), so we only compare this one value.
		if is_uint "$sockbuf"; then
			if [ "$sockbuf" -ge "$((share * 2))" ]; then
				emit PASS kern.ipc.maxsockbuf "current $(human "$sockbuf"), needed $(human $((share * 2))) per stream (BDP $(human "$bdp") / $CONCURRENCY parts, x2)"
			else
				emit WARN kern.ipc.maxsockbuf "current $(human "$sockbuf"), needed $(human $((share * 2))) per stream (BDP $(human "$bdp") / $CONCURRENCY parts, x2)" \
					"sudo sysctl -w kern.ipc.maxsockbuf=$((share * 2))"
			fi
		fi
	else
		sockbuf=$(sysctl -n kern.ipc.maxsockbuf 2>/dev/null)
		emit SKIP socket-buffers "give --bandwidth and --rtt to check them (kern.ipc.maxsockbuf is ${sockbuf:-unknown})"
	fi
	emit SKIP congestion-control "Linux only"
	;;
*)
	emit SKIP ephemeral-ports "not checked on $OS"
	emit SKIP socket-buffers "not checked on $OS"
	emit SKIP congestion-control "not checked on $OS"
	;;
esac
check_memory
check_cpu

# ---------------------------------------------------------------------------
# Suggested command.
#
# Only the open file limit maps to an s5cmd flag. Each worker can hold one
# local file and up to --concurrency connections, so the limit allows
#   fit = (effective limit - FD_MARGIN) / (concurrency + 1)
# workers (same formula as FD_NEED above). The other limits have a fix that
# changes the machine instead: ports and socket buffers are sysctls, and
# memory only depends on stdin uploads, so none of them is turned into a
# different flag value here. Nothing is changed by this script.
# ---------------------------------------------------------------------------
suggest() {
	nw=$NUMWORKERS
	printf 'Suggested command for these settings:\n'
	if is_uint "$EFF_FD"; then
		fit=$(((EFF_FD - FD_MARGIN) / (CONCURRENCY + 1)))
		if [ "$fit" -lt 2 ]; then
			printf '  the open file limit (%s) is too low for any useful setting, raise it first (see the fix line)\n' "$EFF_FD"
			return
		fi
		if [ "$fit" -lt "$nw" ]; then
			printf '  (--numworkers lowered from %s to %s to fit the open file limit)\n' "$nw" "$fit"
			nw=$fit
		fi
	fi
	printf '  s5cmd --numworkers %s cp --concurrency %s --part-size %s SOURCE DESTINATION\n' "$nw" "$CONCURRENCY" "$PART_MIB"
}

printf '\n'
suggest
printf '\n%s FAIL, %s WARN\n' "$FAILS" "$WARNS"
[ "$FAILS" -eq 0 ]
