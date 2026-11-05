package command

import (
	"errors"
	"testing"

	"github.com/peak/s5cmd/v2/log"
)

func TestSyncPrintSortErrorsSkipsNil(t *testing.T) {
	log.Init("error", false)

	// extsort sends a nil error when the context is cancelled mid-merge.
	errCh := make(chan error, 2)
	errCh <- nil
	errCh <- errors.New("sort failed")
	close(errCh)

	Sync{op: "sync"}.printSortErrors(errCh)
}
