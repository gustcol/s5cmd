package command

import (
	"flag"
	"testing"

	"github.com/urfave/cli/v2"
)

func TestValidateRMCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sources        []string
		expectedErrStr string
	}{
		{
			name: "error_if_local_and_remote_mixed",
			sources: []string{
				"s3://bucket/key",
				"filename.txt",
			},
			expectedErrStr: "arguments cannot have both local and remote sources",
		},
		{
			name: "error_if_sources_have_bucket",
			sources: []string{
				"s3://bucket/key",
				"s3://bucket",
			},
			expectedErrStr: "s3 bucket/prefix cannot be used for delete operations (forgot wildcard character?)",
		},
		{
			name: "error_if_sources_have_s3_prefix",
			sources: []string{
				"s3://bucket/prefix/",
			},
			expectedErrStr: "s3 bucket/prefix cannot be used for delete operations (forgot wildcard character?)",
		},
		{
			name: "raw_allows_key_ending_with_slash",
			sources: []string{
				"--raw",
				"s3://bucket/dirobj/",
			},
		},
		{
			name: "raw_does_not_allow_bucket",
			sources: []string{
				"--raw",
				"s3://bucket",
			},
			expectedErrStr: "s3 bucket/prefix cannot be used for delete operations (forgot wildcard character?)",
		},
		{
			name: "error_if_raw_and_all_versions_with_trailing_slash",
			sources: []string{
				"--raw",
				"--all-versions",
				"s3://bucket/dirobj/",
			},
			expectedErrStr: "--raw with --all-versions cannot be used with a key ending in \"/\", it would match every key under that prefix",
		},
		{
			name: "raw_and_all_versions_without_trailing_slash",
			sources: []string{
				"--raw",
				"--all-versions",
				"s3://bucket/key",
			},
		},
		{
			name: "success",
			sources: []string{
				"s3://bucket/prefix/filename.txt",
				"s3://bucket/wildcard/*.txt",
			},
		},
		{
			name: "error_if_different_buckets",
			sources: []string{
				"s3://bucket/object",
				"s3://someotherbucket/object",
			},
			expectedErrStr: "removal of objects with different buckets in a single command is not allowed",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			flagset := flag.NewFlagSet("rm", flag.ExitOnError)
			flagset.Bool("raw", false, "")
			flagset.Bool("all-versions", false, "")
			if err := flagset.Parse(tc.sources); err != nil {
				t.Error(err)
			}
			ctx := cli.NewContext(app, flagset, nil)

			err := validateRMCommand(ctx)
			if (err != nil && err.Error() != tc.expectedErrStr) ||
				(err == nil && tc.expectedErrStr != "") {
				t.Errorf("expected_got = %v, error_expected = %v", err, tc.expectedErrStr)
			}
		})
	}
}
