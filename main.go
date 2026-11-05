package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/peak/s5cmd/v2/command"
)

func main() {
	// sigCh only records which signal asked us to stop, so that we can report
	// it to the parent process once everything has been cleaned up. The
	// cancellation itself is done through ctx, which command.Main already
	// honors.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := command.Main(ctx, os.Args)
	cancel()

	select {
	case sig := <-sigCh:
		// Does not return if it could terminate the process.
		exitBySignal(sig)
	default:
	}

	if err != nil {
		os.Exit(1)
	}
}
