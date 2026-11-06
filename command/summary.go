package command

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/urfave/cli/v2"

	errorpkg "github.com/peak/s5cmd/v2/error"
)

// maxSummaryErrors is how many error messages the summary keeps. The count of
// all errors is always exact.
const maxSummaryErrors = 100

// tally counts objects and their bytes.
type tally struct {
	count atomic.Int64
	bytes atomic.Int64
}

func (t *tally) add(size int64) {
	t.count.Add(1)
	if size > 0 {
		t.bytes.Add(size)
	}
}

// jobSummary collects what a command did, for --summary-json. The counters are
// atomic, so the workers update it without a lock; only the list of error
// messages is guarded by a mutex.
type jobSummary struct {
	command   string
	startedAt time.Time

	copied, skipped, failed, deleted tally

	// sizeHints holds the sizes of destination objects that sync found and
	// will ask rm to delete by name, keyed by deleteKey. rm does not list
	// objects it is given by name, so it would not know their size.
	sizeHints sync.Map
	// hintSizes is set by sync --delete. Without it hints would pile up for
	// objects that are never deleted.
	hintSizes atomic.Bool

	mu     sync.Mutex
	errors []string
}

// activeSummary is the summary that is being collected, if any. There is at
// most one per process: the command that was started with --summary-json.
var activeSummary atomic.Pointer[jobSummary]

func summaryCopied(size int64) {
	if s := activeSummary.Load(); s != nil {
		s.copied.add(size)
	}
}

func summarySkipped(size int64) {
	if s := activeSummary.Load(); s != nil {
		s.skipped.add(size)
	}
}

func summaryDeleted(size int64) {
	if s := activeSummary.Load(); s != nil {
		s.deleted.add(size)
	}
}

// summaryFailed records a failure. Cancelations are not failures: they are
// the result of an earlier failure or of the user interrupting.
func summaryFailed(size int64, err error) {
	s := activeSummary.Load()
	if s == nil || err == nil || errorpkg.IsCancelation(err) {
		return
	}
	s.failed.add(size)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errors) < maxSummaryErrors {
		s.errors = append(s.errors, summaryErrorText(err))
	}
}

// summaryErrorText is the text of an error as it is printed: with the command
// that failed, when it is known.
func summaryErrorText(err error) string {
	var cerr *errorpkg.Error
	if errors.As(err, &cerr) {
		return fmt.Sprintf("%q: %s", cerr.FullCommand(), cleanupError(cerr.Err))
	}
	return cleanupError(err)
}

// summarySizeHint remembers the size of an object that is going to be deleted
// by name, see jobSummary.sizeHints.
func summarySizeHint(key string, size int64) {
	if s := activeSummary.Load(); s != nil && s.hintSizes.Load() {
		s.sizeHints.Store(key, size)
	}
}

// summaryExpectDeletes tells the summary that names of objects to delete come
// with size hints.
func summaryExpectDeletes() {
	if s := activeSummary.Load(); s != nil {
		s.hintSizes.Store(true)
	}
}

// summaryTakeSizeHint returns the size set by summarySizeHint, once.
func summaryTakeSizeHint(key string) int64 {
	if s := activeSummary.Load(); s != nil {
		if v, ok := s.sizeHints.LoadAndDelete(key); ok {
			return v.(int64)
		}
	}
	return 0
}

// summaryTask wraps a task of size bytes so that its failure is recorded.
func summaryTask(task func() error, size int64) func() error {
	return func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				summaryFailed(size, fmt.Errorf("panic: %v", r))
				panic(r)
			}
		}()
		err = task()
		summaryFailed(size, err)
		return err
	}
}

type summaryCounts struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

func (t *tally) counts() summaryCounts {
	return summaryCounts{Count: t.count.Load(), Bytes: t.bytes.Load()}
}

// summaryReport is the content of the --summary-json file.
type summaryReport struct {
	Command         string    `json:"command"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	Success         bool      `json:"success"`
	// Canceled is true when the command was interrupted or its context was
	// canceled. Success is false then, and Failed does not count it.
	Canceled bool          `json:"canceled"`
	Copied   summaryCounts `json:"copied"`
	Skipped  summaryCounts `json:"skipped"`
	Failed   summaryCounts `json:"failed"`
	Deleted  summaryCounts `json:"deleted"`
	// ThroughputBytesPerSecond is the copied bytes over the duration of the
	// command.
	ThroughputBytesPerSecond float64 `json:"throughput_bytes_per_second"`
	// Errors holds the first errors. Failed.Count is the number of all errors.
	Errors []string `json:"errors"`
}

func (s *jobSummary) report(finished time.Time, cmdErr error) summaryReport {
	s.mu.Lock()
	errs := append([]string{}, s.errors...)
	s.mu.Unlock()

	// A command can fail before it counts anything, for example when the
	// source cannot be listed. The returned error must not be lost.
	failed := s.failed.counts()
	if cmdErr != nil && !errorpkg.IsCancelation(cmdErr) && failed.Count == 0 {
		failed.Count = 1
		errs = append(errs, cleanupError(cmdErr))
	}

	duration := finished.Sub(s.startedAt)
	copied := s.copied.counts()
	var throughput float64
	if secs := duration.Seconds(); secs > 0 {
		throughput = float64(copied.Bytes) / secs
	}

	return summaryReport{
		Command:                  s.command,
		StartedAt:                s.startedAt.UTC(),
		FinishedAt:               finished.UTC(),
		DurationSeconds:          duration.Seconds(),
		Success:                  cmdErr == nil && failed.Count == 0,
		Canceled:                 cmdErr != nil && errorpkg.IsCancelation(cmdErr),
		Copied:                   copied,
		Skipped:                  s.skipped.counts(),
		Failed:                   failed,
		Deleted:                  s.deleted.counts(),
		ThroughputBytesPerSecond: throughput,
		Errors:                   errs,
	}
}

// startSummary starts collecting a summary when --summary-json is set. It
// returns the function to call, with the error of the command, when the command
// is done: that writes the file. If a summary is already being collected (a
// command started by sync, or another command of a run file) it does nothing,
// so the counts always belong to the command that started the summary.
func startSummary(c *cli.Context, err *error) func() {
	path := c.String("summary-json")
	if path == "" {
		return func() {}
	}

	s := &jobSummary{command: c.Command.Name, startedAt: time.Now()}
	if !activeSummary.CompareAndSwap(nil, s) {
		return func() {}
	}

	return func() {
		activeSummary.CompareAndSwap(s, nil)

		var cmdErr error
		if err != nil {
			cmdErr = *err
		}
		if werr := writeSummary(path, s.report(time.Now(), cmdErr)); werr != nil {
			printError(commandFromContext(c), c.Command.Name, werr)
			if err != nil {
				*err = multierror.Append(*err, werr).ErrorOrNil()
			}
		}
	}
}

// validateSummaryPath checks, before the command starts, that the summary
// file can be created: a transfer that runs for hours should not find out at
// the end that the directory is missing. The file itself is only touched when
// the command ends.
func validateSummaryPath(c *cli.Context) error {
	path := c.String("summary-json")
	if path == "" {
		return nil
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return fmt.Errorf("--summary-json: %q is a directory", path)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("--summary-json: cannot create a file in %q: %w", dir, errors.Unwrap(err))
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

// createSummaryTemp creates a new file next to path with mode 0666, so that
// the umask decides the permissions, like it does for downloaded files.
// os.CreateTemp always uses 0600.
func createSummaryTemp(path string) (*os.File, error) {
	var lastErr error
	for i := 0; i < 100; i++ {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+hex.EncodeToString(b[:]))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// writeSummary writes the report to path. The file is written to a temporary
// file in the same directory and renamed, so a reader never sees half of it.
func writeSummary(path string, r summaryReport) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := createSummaryTemp(path)
	if err != nil {
		return fmt.Errorf("writing summary: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing summary: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing summary: %w", err)
	}
	return nil
}
