package command

import (
	"errors"
	"strings"
	"testing"

	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/storage"
)

func TestSyncPrintSortErrorsSkipsNil(t *testing.T) {
	log.Init("error", false)

	// extsort sends a nil error when the context is cancelled mid-merge.
	errCh := make(chan error, 2)
	errCh <- nil
	errCh <- errors.New("sort failed")
	close(errCh)

	s := Sync{op: "sync", listErrs: &syncErrors{}}
	s.printSortErrors(errCh)

	if err := s.listErrs.err(); err == nil || !strings.Contains(err.Error(), "sort failed") {
		t.Errorf("expected the sort error to be recorded, got %v", err)
	}
}

func TestSyncStorageOpts(t *testing.T) {
	t.Parallel()

	withRegion := func(opts storage.Options, region string) storage.Options {
		opts.SetRegion(region)
		return opts
	}

	base := storage.Options{MaxRetries: 7, Endpoint: "http://localhost:9000"}

	testcases := []struct {
		name      string
		srcRegion string
		dstRegion string
		wantSrc   storage.Options
		wantDst   storage.Options
	}{
		{
			name:    "no_regions",
			wantSrc: base,
			wantDst: base,
		},
		{
			name:      "only_source_region",
			srcRegion: "eu-west-1",
			wantSrc:   withRegion(base, "eu-west-1"),
			wantDst:   withRegion(base, "eu-west-1"),
		},
		{
			name:      "destination_region_only",
			dstRegion: "us-west-2",
			wantSrc:   base,
			wantDst:   withRegion(base, "us-west-2"),
		},
		{
			name:      "both_regions",
			srcRegion: "eu-west-1",
			dstRegion: "us-west-2",
			wantSrc:   withRegion(base, "eu-west-1"),
			wantDst:   withRegion(base, "us-west-2"),
		},
	}

	for _, tc := range testcases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := Sync{storageOpts: base, srcRegion: tc.srcRegion, dstRegion: tc.dstRegion}

			if got := s.srcStorageOpts(); got != tc.wantSrc {
				t.Errorf("source: got %+v, want %+v", got, tc.wantSrc)
			}
			if got := s.dstStorageOpts(); got != tc.wantDst {
				t.Errorf("destination: got %+v, want %+v", got, tc.wantDst)
			}
			if s.storageOpts != base {
				t.Errorf("storageOpts was modified: got %+v, want %+v", s.storageOpts, base)
			}
		})
	}
}
