package command

import (
	"testing"

	"github.com/peak/s5cmd/v2/storage"
)

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
