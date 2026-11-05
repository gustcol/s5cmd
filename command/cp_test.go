package command

import (
	"io"
	"os"
	"testing"

	"gotest.tools/v3/assert"

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
