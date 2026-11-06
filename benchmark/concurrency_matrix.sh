#!/usr/bin/env bash
#
# Runs s5cmd against an S3 compatible endpoint (for example MinIO) over a
# matrix of --numworkers and --concurrency values and prints throughput and
# peak memory for each combination.
#
# Scenarios:
#   small  SMALL_COUNT files of SMALL_SIZE_KIB KiB   (default 2000 x 64 KiB)
#   big    BIG_COUNT files of BIG_SIZE_MIB MiB       (default 4 x 1024 MiB)
#
# Each cell is run for upload and download. Output is a TSV table on stdout,
# progress goes to stderr.
#
# Environment:
#   S3_ENDPOINT_URL        required, e.g. http://127.0.0.1:9000
#   AWS_ACCESS_KEY_ID      required
#   AWS_SECRET_ACCESS_KEY  required
#   BENCH_BUCKET           required, must already exist
#   S5CMD_BIN              s5cmd binary to test (default: s5cmd in PATH)
#   TIME_BIN               GNU time (default: /usr/bin/time)
#   BENCH_WORKDIR          scratch directory (default: a mktemp dir)
#   BENCH_PREFIX           key prefix (default: s5cmd-conc-bench)
#   WORKERS                numworkers values (default: "16 64 256")
#   CONCURRENCY            --concurrency values (default: "5 16")
#   SCENARIOS              "small big" by default
#   SMALL_COUNT, SMALL_SIZE_KIB, BIG_COUNT, BIG_SIZE_MIB
#
# Credentials are read from the environment only and are never printed.

set -euo pipefail

S5CMD_BIN=${S5CMD_BIN:-s5cmd}
TIME_BIN=${TIME_BIN:-/usr/bin/time}
BENCH_PREFIX=${BENCH_PREFIX:-s5cmd-conc-bench}
WORKERS=${WORKERS:-"16 64 256"}
CONCURRENCY=${CONCURRENCY:-"5 16"}
SCENARIOS=${SCENARIOS:-"small big"}
SMALL_COUNT=${SMALL_COUNT:-2000}
SMALL_SIZE_KIB=${SMALL_SIZE_KIB:-64}
BIG_COUNT=${BIG_COUNT:-4}
BIG_SIZE_MIB=${BIG_SIZE_MIB:-1024}

die() {
	echo "error: $*" >&2
	exit 1
}

for v in S3_ENDPOINT_URL AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY BENCH_BUCKET; do
	[[ -n ${!v:-} ]] || die "$v is not set"
done
command -v "$S5CMD_BIN" >/dev/null 2>&1 || die "s5cmd binary not found: $S5CMD_BIN"
"$TIME_BIN" -f '%e %M' -o /dev/null true >/dev/null 2>&1 ||
	die "$TIME_BIN does not support -f/-o, install GNU time or set TIME_BIN"

if [[ -n ${BENCH_WORKDIR:-} ]]; then
	workdir=$BENCH_WORKDIR
	mkdir -p "$workdir"
else
	workdir=$(mktemp -d)
fi
cleanup_workdir=${BENCH_WORKDIR:+no}
cleanup() {
	"$S5CMD_BIN" --log error rm "s3://$BENCH_BUCKET/${BENCH_PREFIX:?}/*" >/dev/null 2>&1 || true
	if [[ -z $cleanup_workdir ]]; then
		rm -rf "$workdir"
	else
		rm -rf "${workdir:?}/src" "${workdir:?}/dst"
	fi
}
trap cleanup EXIT

src="$workdir/src"
dst="$workdir/dst"
timing="$workdir/timing"
mkdir -p "$src" "$dst"

# make_files DIR COUNT SIZE_KIB: random content, one distinct file per name.
make_files() {
	local dir=$1 count=$2 size_kib=$3 i
	rm -rf "${dir:?}" && mkdir -p "$dir"
	head -c "$((size_kib * 1024))" /dev/urandom >"$dir/file-0"
	for ((i = 1; i < count; i++)); do
		cp "$dir/file-0" "$dir/file-$i"
	done
}

remote_clear() {
	"$S5CMD_BIN" --log error rm "s3://$BENCH_BUCKET/${BENCH_PREFIX:?}/$1/*" >/dev/null 2>&1 || true
}

# measure SCENARIO OP COUNT TOTAL_KIB WORKERS CONCURRENCY -- s5cmd args...
measure() {
	local scenario=$1 op=$2 count=$3 total_kib=$4 workers=$5 conc=$6
	shift 7
	local secs rss_kib
	"$TIME_BIN" -f '%e %M' -o "$timing" \
		"$S5CMD_BIN" --log error --numworkers "$workers" "$@" >/dev/null ||
		die "s5cmd failed: scenario=$scenario op=$op workers=$workers concurrency=$conc"
	read -r secs rss_kib <"$timing"
	awk -v s="$scenario" -v o="$op" -v w="$workers" -v c="$conc" -v t="$secs" \
		-v n="$count" -v m="$total_kib" -v r="$rss_kib" 'BEGIN {
			if (t <= 0) t = 0.001
			printf "%s\t%s\t%d\t%d\t%.2f\t%.1f\t%.1f\t%.0f\n", s, o, w, c, t, m / 1024 / t, n / t, r / 1024
		}'
}

printf 'scenario\top\tnumworkers\tconcurrency\tseconds\tMiB_per_s\tobjects_per_s\tpeak_rss_MiB\n'

for scenario in $SCENARIOS; do
	case $scenario in
	small)
		count=$SMALL_COUNT
		size_kib=$SMALL_SIZE_KIB
		;;
	big)
		count=$BIG_COUNT
		size_kib=$((BIG_SIZE_MIB * 1024))
		;;
	*) die "unknown scenario: $scenario" ;;
	esac
	total_kib=$((count * size_kib))

	echo "creating $count files of $size_kib KiB for scenario $scenario" >&2
	make_files "$src" "$count" "$size_kib"

	for w in $WORKERS; do
		for c in $CONCURRENCY; do
			echo "scenario=$scenario numworkers=$w concurrency=$c" >&2
			remote="${BENCH_PREFIX:?}/$scenario"

			remote_clear "$scenario"
			measure "$scenario" upload "$count" "$total_kib" "$w" "$c" -- \
				cp --concurrency "$c" "$src/*" "s3://$BENCH_BUCKET/$remote/"

			rm -rf "${dst:?}" && mkdir -p "$dst"
			measure "$scenario" download "$count" "$total_kib" "$w" "$c" -- \
				cp --concurrency "$c" "s3://$BENCH_BUCKET/$remote/*" "$dst/"
		done
	done
	rm -rf "${src:?}" "${dst:?}" && mkdir -p "$src" "$dst"
done
