package e2e

import (
	encjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/icmd"
)

type summaryFile struct {
	Command         string  `json:"command"`
	StartedAt       string  `json:"started_at"`
	FinishedAt      string  `json:"finished_at"`
	DurationSeconds float64 `json:"duration_seconds"`
	Success         bool    `json:"success"`
	Copied          counts  `json:"copied"`
	Skipped         counts  `json:"skipped"`
	Failed          counts  `json:"failed"`
	Deleted         counts  `json:"deleted"`
	Throughput      float64 `json:"throughput_bytes_per_second"`
	Errors          []string
}

type counts struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

func readSummary(t *testing.T, path string) summaryFile {
	t.Helper()
	data, err := os.ReadFile(path)
	assert.NilError(t, err)
	var s summaryFile
	assert.NilError(t, encjson.Unmarshal(data, &s), string(data))
	assert.Assert(t, s.StartedAt != "" && s.FinishedAt != "")
	return s
}

func TestSummaryJSONUploadAndDownload(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "12345"), fs.WithFile("b.txt", "1234567890"), fs.WithDir("out"))
	defer workdir.Remove()
	summary := filepath.Join(t.TempDir(), "up.json")

	result := icmd.RunCmd(s5cmd("cp", "--summary-json", summary, filepath.ToSlash(workdir.Path())+"/*.txt", fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Success)
	// the normal output is the same as without the flag.
	assert.Equal(t, strings.Count(result.Stdout(), "cp "), 2, result.Stdout())

	s := readSummary(t, summary)
	assert.Equal(t, s.Command, "cp")
	assert.Assert(t, s.Success)
	assert.Equal(t, s.Copied, counts{2, 15})
	assert.Equal(t, s.Skipped, counts{})
	assert.Equal(t, s.Failed, counts{})
	assert.Equal(t, s.Deleted, counts{})
	assert.Assert(t, s.DurationSeconds > 0)
	assert.Assert(t, s.Throughput > 0)
	assert.Equal(t, len(s.Errors), 0)

	// download with --no-clobber: one exists, so one is skipped.
	assert.NilError(t, os.WriteFile(workdir.Join("out", "a.txt"), []byte("12345"), 0o644))
	summary = filepath.Join(t.TempDir(), "down.json")
	icmd.RunCmd(s5cmd("cp", "-n", "--summary-json", summary, fmt.Sprintf("s3://%v/*", bucket), filepath.ToSlash(workdir.Join("out"))+"/")).Assert(t, icmd.Success)
	s = readSummary(t, summary)
	assert.Equal(t, s.Copied, counts{1, 10})
	assert.Equal(t, s.Skipped, counts{1, 5})
}

func TestSummaryJSONFailures(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "ok.txt", "ok")

	workdir := fs.NewDir(t, bucket)
	defer workdir.Remove()
	summary := filepath.Join(t.TempDir(), "fail.json")

	result := icmd.RunCmd(s5cmd("cp", "--summary-json", summary, fmt.Sprintf("s3://%v/missing.txt", bucket), filepath.ToSlash(workdir.Path())+"/"))
	result.Assert(t, icmd.Expected{ExitCode: 1})

	s := readSummary(t, summary)
	assert.Assert(t, !s.Success)
	assert.Equal(t, s.Failed.Count, int64(1))
	assert.Equal(t, len(s.Errors), 1)
	assert.Assert(t, strings.Contains(s.Errors[0], "missing.txt"), s.Errors[0])
	assert.Equal(t, s.Copied, counts{})
}

func TestSummaryJSONIsWrittenWhenTheCommandFailsEarly(t *testing.T) {
	t.Parallel()

	_, s5cmd := setup(t)
	workdir := fs.NewDir(t, "summary-early")
	defer workdir.Remove()
	summary := filepath.Join(t.TempDir(), "s.json")
	result := icmd.RunCmd(s5cmd("cp", "--summary-json", summary, "s3://nosuchbucket/*", filepath.ToSlash(workdir.Path())+"/"))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	s := readSummary(t, summary)
	assert.Assert(t, !s.Success)
	assert.Assert(t, s.Failed.Count >= 1)
	assert.Assert(t, len(s.Errors) >= 1)
}

func TestSummaryJSONSyncWithDelete(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "same.txt", "same")
	putFile(t, s3client, bucket, "gone1.txt", "gone-1")
	putFile(t, s3client, bucket, "gone2.txt", "gone-22")
	putFile(t, s3client, bucket, "special *.txt", "star")

	workdir := fs.NewDir(t, bucket, fs.WithFile("same.txt", "same"), fs.WithFile("new.txt", "brand new"))
	defer workdir.Remove()
	// the file is older than the object, so same.txt is skipped.
	assert.NilError(t, os.Chtimes(workdir.Join("same.txt"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	summary := filepath.Join(t.TempDir(), "sync.json")

	result := icmd.RunCmd(s5cmd("sync", "--delete", "--summary-json", summary, filepath.ToSlash(workdir.Path())+"/", fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Success)

	s := readSummary(t, summary)
	assert.Equal(t, s.Command, "sync")
	assert.Assert(t, s.Success)
	assert.Equal(t, s.Copied, counts{1, 9})
	assert.Equal(t, s.Skipped, counts{1, 4})
	assert.Equal(t, s.Deleted, counts{3, int64(len("gone-1") + len("gone-22") + len("star"))})
	assert.Equal(t, s.Failed, counts{})
	// the generated commands do not write summaries of their own
	entries, _ := os.ReadDir(filepath.Dir(summary))
	assert.Equal(t, len(entries), 1)
}

func TestSummaryJSONRemove(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "a/1.txt", "11")
	putFile(t, s3client, bucket, "a/2.txt", "222")

	summary := filepath.Join(t.TempDir(), "rm.json")
	icmd.RunCmd(s5cmd("rm", "--summary-json", summary, fmt.Sprintf("s3://%v/a/*", bucket))).Assert(t, icmd.Success)
	s := readSummary(t, summary)
	assert.Equal(t, s.Command, "rm")
	assert.Equal(t, s.Deleted, counts{2, 5})
	assert.Assert(t, s.Success)
}

// --json keeps printing one JSON object per line, and the summary file is
// separate.
func TestSummaryJSONDoesNotChangeJSONOutput(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "12345"))
	defer workdir.Remove()

	without := icmd.RunCmd(s5cmd("--json", "cp", filepath.ToSlash(workdir.Join("a.txt")), fmt.Sprintf("s3://%v/one.txt", bucket)))
	without.Assert(t, icmd.Success)
	summary := filepath.Join(t.TempDir(), "s.json")
	with := icmd.RunCmd(s5cmd("--json", "cp", "--summary-json", summary, filepath.ToSlash(workdir.Join("a.txt")), fmt.Sprintf("s3://%v/one.txt", bucket)))
	with.Assert(t, icmd.Success)

	assert.Equal(t, with.Stdout(), without.Stdout())
	assert.Equal(t, readSummary(t, summary).Copied, counts{1, 5})
}

func TestSummaryJSONMove(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "12345"))
	defer workdir.Remove()
	summary := filepath.Join(t.TempDir(), "mv.json")

	icmd.RunCmd(s5cmd("mv", "--summary-json", summary, filepath.ToSlash(workdir.Join("a.txt")), fmt.Sprintf("s3://%v/", bucket))).Assert(t, icmd.Success)
	s := readSummary(t, summary)
	assert.Equal(t, s.Command, "mv")
	assert.Assert(t, s.Success)
	assert.Equal(t, s.Copied, counts{1, 5})
}

func TestSummaryJSONUnwritablePathFailsBeforeTransfer(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "12345"))
	defer workdir.Remove()
	summary := filepath.Join(t.TempDir(), "missing", "s.json")

	result := icmd.RunCmd(s5cmd("cp", "--summary-json", summary, filepath.ToSlash(workdir.Join("a.txt")), fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "--summary-json"), result.Stderr())
	// nothing was uploaded.
	icmd.RunCmd(s5cmd("ls", fmt.Sprintf("s3://%v/", bucket))).Assert(t, icmd.Expected{ExitCode: 0, Out: ""})
}
