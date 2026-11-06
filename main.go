package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/peak/s5cmd/v2/command"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	// received records which signal asked us to stop, so that we can report
	// it to the parent process once everything has been cleaned up. The
	// cancellation itself is done through ctx, which command.Main already
	// honors.
	received := make(chan os.Signal, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			received <- sig
			// Stop catching signals: the first one starts a graceful
			// shutdown, a second one gets the default action and quits
			// right away.
			signal.Stop(sigCh)
			cancel()
		case <-ctx.Done():
			signal.Stop(sigCh)
		}
	}()

	err := command.Main(ctx, os.Args)
	cancel()

	select {
	case sig := <-received:
		// Does not return if it could terminate the process.
		exitBySignal(sig)
	default:
	}

	if err != nil {
		os.Exit(1)
	}
}
