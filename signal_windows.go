//go:build windows

package main

import (
	"os"
)

// exitBySignal exits with the conventional status for an interrupt. Windows
// cannot terminate a process "by signal", and it does not deliver SIGTERM, so
// only os.Interrupt is handled here; for anything else it returns and main
// exits as it would without a signal.
func exitBySignal(sig os.Signal) {
	if sig == os.Interrupt {
		os.Exit(130)
	}
}
