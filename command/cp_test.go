package command

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-multierror"
	"gotest.tools/v3/assert"

	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/storage"
)

func TestDstStorageOpts(t *testing.T) {
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
		want      storage.Options
	}{
		{
			name: "no_regions",
			want: base,
		},
		{
			name:      "only_source_region_is_kept",
			srcRegion: "eu-west-1",
			want:      withRegion(base, "eu-west-1"),
		},
		{
			name:      "destination_region_overrides_source_region",
			srcRegion: "eu-west-1",
			dstRegion: "us-west-2",
			want:      withRegion(base, "us-west-2"),
		},
		{
			name:      "destination_region_only",
			dstRegion: "us-west-2",
			want:      withRegion(base, "us-west-2"),
		},
	}

	for _, tc := range testcases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := base
			if tc.srcRegion != "" {
				opts.SetRegion(tc.srcRegion)
			}
			c := Copy{storageOpts: opts, dstRegion: tc.dstRegion}

			if got := c.dstStorageOpts(); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			// the common options must stay untouched, they are also used
			// for the source.
			if c.storageOpts != opts {
				t.Errorf("storageOpts was modified: got %+v, want %+v", c.storageOpts, opts)
			}
		})
	}
}

func TestGuessContentType(t *testing.T) {
	t.Parallel()

	testcases := []struct {
		filename string
		content  string

		expectedContentType string
	}{
		{
			filename:            "*.pdf",
			expectedContentType: "application/pdf",
		},
		{
			filename:            "*.css",
			expectedContentType: "text/css; charset=utf-8",
		},
		{
			filename: "index",
			content: `
					<!DOCTYPE html>
					<html>
						<head>
							<title>Hello World</title>
						</head>
						<body>
							<p>Hello, World! I am s5cmd :)</p>
						</body>
					</html>
					`,
			expectedContentType: "text/html; charset=utf-8",
		},
		// check file extension first without checking the content
		{
			filename: "index*.txt",
			content: `
					<!DOCTYPE html>
					<html>
						<head>
							<title>Hello World</title>
						</head>
						<body>
							<p>Hello, World! I am s5cmd :)</p>
						</body>
					</html>
					`,
			expectedContentType: "text/plain; charset=utf-8",
		},
	}

	for _, tc := range testcases {
		tc := tc

		f, err := os.CreateTemp("", tc.filename)
		if err != nil {
			t.Error(err)
		}

		if tc.content != "" {
			f.WriteString(tc.content)
			f.Seek(0, io.SeekStart)
		}

		assert.Equal(t, tc.expectedContentType, guessContentType(f))

		f.Close()
		os.Remove(f.Name())
	}
}

func TestIsSourceNewer(t *testing.T) {
	t.Parallel()

	now := time.Now()
	earlier := now.Add(-time.Minute)

	testcases := []struct {
		name     string
		src      *time.Time
		dst      *time.Time
		expected bool
	}{
		{
			name:     "source newer than destination",
			src:      &now,
			dst:      &earlier,
			expected: true,
		},
		{
			name:     "source older than destination",
			src:      &earlier,
			dst:      &now,
			expected: false,
		},
		{
			name:     "same modification time",
			src:      &now,
			dst:      &now,
			expected: false,
		},
		{
			name:     "source modification time unavailable",
			src:      nil,
			dst:      &now,
			expected: false,
		},
		{
			name:     "destination modification time unavailable",
			src:      &now,
			dst:      nil,
			expected: false,
		},
		{
			name:     "both modification times unavailable",
			src:      nil,
			dst:      nil,
			expected: false,
		},
	}

	for _, tc := range testcases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := &storage.Object{ModTime: tc.src}
			dst := &storage.Object{ModTime: tc.dst}

			got := isSourceNewer(src, dst)
			assert.Equal(t, got, tc.expected)
		})
	}
}

func TestCollectCopyErrorsFileLimit(t *testing.T) {
	// A task hitting the open file limit must cancel the copy and return the
	// error instead of terminating the process.
	log.Init("error", false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 3)
	errCh <- errors.New("open /tmp/a: too many open files")
	errCh <- errors.New("open /tmp/b: too many open files")
	errCh <- errors.New("some other error")
	close(errCh)

	err := collectCopyErrors(func() string { return "" }, "cp", errCh, cancel)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if ctx.Err() == nil {
		t.Error("expected the context to be cancelled")
	}

	merr, ok := err.(*multierror.Error)
	if !ok || len(merr.Errors) != 3 {
		t.Errorf("expected 3 aggregated errors, got %v", err)
	}
}
