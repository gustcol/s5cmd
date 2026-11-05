//go:build !windows
// +build !windows

package fdlimit

import (
	"syscall"
	"testing"
)

func TestRaiseDoesNotLowerLimit(t *testing.T) {
	var before, after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &before); err != nil {
		t.Skip(err)
	}

	_ = Raise()

	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &after); err != nil {
		t.Fatal(err)
	}
	if after.Cur < before.Cur {
		t.Fatalf("soft limit went from %d to %d", before.Cur, after.Cur)
	}
	if after.Cur > before.Max {
		t.Fatalf("soft limit %d above hard limit %d", after.Cur, before.Max)
	}
}
