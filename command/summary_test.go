package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfave/cli/v2"
	"gotest.tools/v3/assert"

	"github.com/peak/s5cmd/v2/log"
)

func resetSummary(t *testing.T) *jobSummary {
	t.Helper()
	s := &jobSummary{command: "cp", startedAt: time.Now().Add(-2 * time.Second)}
	assert.Assert(t, activeSummary.CompareAndSwap(nil, s))
	t.Cleanup(func() { activeSummary.Store(nil) })
	return s
}

func TestSummaryWithoutActiveSummaryDoesNothing(t *testing.T) {
	// none of these may panic.
	summaryCopied(10)
	summarySkipped(10)
	summaryDeleted(10)
	summaryFailed(10, errors.New("x"))
	assert.Assert(t, summaryTask(func() error { return errors.New("x") }, 1)() != nil)
}

func TestSummaryCounts(t *testing.T) {
	s := resetSummary(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			summaryCopied(10)
			summarySkipped(1)
			summaryDeleted(5)
			summaryFailed(2, errors.New("boom"))
		}()
	}
	wg.Wait()

	r := s.report(time.Now(), errors.New("boom"))
	assert.DeepEqual(t, r.Copied, summaryCounts{100, 1000})
	assert.DeepEqual(t, r.Skipped, summaryCounts{100, 100})
	assert.DeepEqual(t, r.Deleted, summaryCounts{100, 500})
	assert.DeepEqual(t, r.Failed, summaryCounts{100, 200})
	assert.Equal(t, len(r.Errors), maxSummaryErrors)
	assert.Assert(t, !r.Success)
	assert.Assert(t, r.DurationSeconds >= 2)
	assert.Assert(t, r.ThroughputBytesPerSecond > 0 && r.ThroughputBytesPerSecond <= 500)
}

func TestSummaryIgnoresCancelation(t *testing.T) {
	s := resetSummary(t)
	summaryFailed(1, errors.New("context canceled"))
	summaryFailed(1, nil)
	// context.Canceled is what a canceled task reports.
	summaryFailed(1, ctxCanceled())
	assert.Equal(t, s.failed.count.Load(), int64(1))
}

func TestSummaryKeepsCommandErrorWhenNothingWasCounted(t *testing.T) {
	s := resetSummary(t)
	r := s.report(time.Now(), errors.New("no such bucket"))
	assert.Equal(t, r.Failed.Count, int64(1))
	assert.DeepEqual(t, r.Errors, []string{"no such bucket"})
	assert.Assert(t, !r.Success)

	r = s.report(time.Now(), nil)
	assert.Assert(t, r.Success)
	assert.DeepEqual(t, r.Errors, []string{})
}

func TestSummaryTaskRecordsFailureAndPanic(t *testing.T) {
	s := resetSummary(t)

	assert.NilError(t, summaryTask(func() error { return nil }, 5)())
	assert.Equal(t, s.failed.count.Load(), int64(0))

	err := summaryTask(func() error { return errors.New("bad") }, 5)()
	assert.ErrorContains(t, err, "bad")
	assert.Equal(t, s.failed.count.Load(), int64(1))
	assert.Equal(t, s.failed.bytes.Load(), int64(5))

	func() {
		defer func() { assert.Assert(t, recover() != nil) }()
		summaryTask(func() error { panic("oops") }, 7)()
	}()
	assert.Equal(t, s.failed.count.Load(), int64(2))
	assert.Assert(t, strings.Contains(s.errors[1], "oops"))
}

func newSummaryContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	app := &cli.App{}
	cmd := &cli.Command{Name: "cp", Flags: []cli.Flag{&cli.StringFlag{Name: "summary-json"}}}
	set := flagSetFor(t, cmd, args)
	ctx := cli.NewContext(app, set, nil)
	ctx.Command = cmd
	return ctx
}

func TestStartSummaryWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	var err error
	finish := startSummary(newSummaryContext(t, "--summary-json", path), &err)
	summaryCopied(100)
	summarySkipped(7)
	summaryFailed(3, errors.New("one"))
	err = errors.New("one")
	finish()

	data, rerr := os.ReadFile(path)
	assert.NilError(t, rerr)
	var got map[string]any
	assert.NilError(t, json.Unmarshal(data, &got))
	for _, key := range []string{"command", "started_at", "finished_at", "duration_seconds", "success",
		"copied", "skipped", "failed", "deleted", "throughput_bytes_per_second", "errors"} {
		_, ok := got[key]
		assert.Assert(t, ok, "missing %q in %s", key, data)
	}
	assert.Equal(t, got["command"], "cp")
	assert.Equal(t, got["success"], false)
	assert.DeepEqual(t, got["copied"], map[string]any{"count": 1.0, "bytes": 100.0})
	assert.DeepEqual(t, got["failed"], map[string]any{"count": 1.0, "bytes": 3.0})
	assert.DeepEqual(t, got["errors"], []any{"one"})

	// the summary is not active any more, and no temporary file is left.
	assert.Assert(t, activeSummary.Load() == nil)
	entries, _ := os.ReadDir(filepath.Dir(path))
	assert.Equal(t, len(entries), 1)
}

func TestStartSummaryWithoutFlagIsNoop(t *testing.T) {
	var err error
	startSummary(newSummaryContext(t), &err)()
	assert.Assert(t, activeSummary.Load() == nil)
}

func TestStartSummaryNestedDoesNotReplace(t *testing.T) {
	outer := resetSummary(t)
	path := filepath.Join(t.TempDir(), "inner.json")
	var err error
	var warned []string
	old := summaryDropped
	summaryDropped = func(_, _, p string) { warned = append(warned, p) }
	defer func() { summaryDropped = old }()
	startSummary(newSummaryContext(t, "--summary-json", path), &err)()
	assert.DeepEqual(t, warned, []string{path})
	// without the flag there is nothing to warn about.
	startSummary(newSummaryContext(t), &err)()
	assert.Equal(t, len(warned), 1)
	assert.Assert(t, activeSummary.Load() == outer)
	_, serr := os.Stat(path)
	assert.Assert(t, os.IsNotExist(serr))
}

func TestStartSummaryWriteFailureIsReported(t *testing.T) {
	log.Init("error", false)

	path := filepath.Join(t.TempDir(), "missing-dir", "summary.json")
	var err error
	startSummary(newSummaryContext(t, "--summary-json", path), &err)()
	assert.ErrorContains(t, err, "writing summary")
}

func ctxCanceled() error { return context.Canceled }

func flagSetFor(t *testing.T, cmd *cli.Command, args []string) *flag.FlagSet {
	t.Helper()
	set := flag.NewFlagSet("cp", flag.ContinueOnError)
	for _, f := range cmd.Flags {
		assert.NilError(t, f.Apply(set))
	}
	assert.NilError(t, set.Parse(args))
	return set
}

func TestValidateSummaryPath(t *testing.T) {
	dir := t.TempDir()

	assert.NilError(t, validateSummaryPath(newSummaryContext(t)))
	assert.NilError(t, validateSummaryPath(newSummaryContext(t, "--summary-json", filepath.Join(dir, "ok.json"))))
	assert.ErrorContains(t, validateSummaryPath(newSummaryContext(t, "--summary-json", filepath.Join(dir, "missing", "s.json"))), "cannot create a file")
	assert.ErrorContains(t, validateSummaryPath(newSummaryContext(t, "--summary-json", dir)), "is a directory")

	// nothing is left behind
	entries, _ := os.ReadDir(dir)
	assert.Equal(t, len(entries), 0)
}

func TestSummaryReportMarksCanceled(t *testing.T) {
	s := &jobSummary{command: "cp", startedAt: time.Now()}
	r := s.report(time.Now(), ctxCanceled())
	assert.Assert(t, r.Canceled)
	assert.Assert(t, !r.Success)
	assert.Equal(t, r.Failed.Count, int64(0))

	r = s.report(time.Now(), nil)
	assert.Assert(t, !r.Canceled && r.Success)
}

func TestWriteSummaryReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	assert.NilError(t, os.WriteFile(path, []byte("old"), 0o644))
	assert.NilError(t, writeSummary(path, summaryReport{Command: "cp"}))
	data, _ := os.ReadFile(path)
	assert.Assert(t, strings.Contains(string(data), `"command": "cp"`))
}
