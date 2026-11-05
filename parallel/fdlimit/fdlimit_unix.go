//go:build !windows
// +build !windows

package fdlimit

import (
	"syscall"
)

const (
	// minOpenFilesLimit is the lowest soft limit worth setting. The default
	// of 256 workers keeps up to 256 local files open at once.
	minOpenFilesLimit = 1024

	// maxOpenFilesLimit caps the soft limit that is requested. Every worker
	// can hold a local file plus several connections for multipart
	// transfers, so 1024 is too low for the default settings.
	maxOpenFilesLimit = 65536

	// darwinOpenMax is what macOS accepts when the hard limit is reported
	// as unlimited.
	darwinOpenMax = 10240
)

// Raise tries to increase the soft limit of open files towards the hard limit.
// Raising is best effort: the existing limit is left alone if it cannot be
// changed.
func Raise() error {
	var rLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return err
	}

	target := uint64(maxOpenFilesLimit)
	if rLimit.Max < target {
		target = rLimit.Max
	}

	if rLimit.Cur >= target || target < minOpenFilesLimit {
		return nil
	}

	next := rLimit
	next.Cur = target
	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &next)
	if err == nil {
		return nil
	}

	// Some systems (macOS) refuse a limit above OPEN_MAX even if the hard
	// limit is higher.
	if darwinOpenMax > rLimit.Cur && darwinOpenMax < target {
		next.Cur = darwinOpenMax
		return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &next)
	}

	return err
}
