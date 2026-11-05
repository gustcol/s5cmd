//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// exitBySignal terminates the process with the signal that interrupted it,
// instead of a plain exit code. Callers such as shells can tell the two
// apart: a shell running a loop stops on Ctrl-C only if the child died from
// SIGINT, not if it merely exited with a non-zero status.
//
// It restores the default handler for sig and sends sig to the current
// process, so it must be called after all cleanup is done.
func exitBySignal(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		os.Exit(1)
	}

	signal.Reset(s)
	if err := syscall.Kill(syscall.Getpid(), s); err != nil {
		os.Exit(128 + int(s))
	}

	// The signal is delivered asynchronously. It terminates the process, but
	// fall back to the conventional exit status if it somehow does not.
	time.Sleep(time.Second)
	os.Exit(128 + int(s))
}
