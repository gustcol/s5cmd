//go:build !windows

package command

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"gotest.tools/v3/assert"
)

func TestWriteSummaryRespectsUmask(t *testing.T) {
	for _, mask := range []int{0o022, 0o077} {
		old := syscall.Umask(mask)
		path := filepath.Join(t.TempDir(), "s.json")
		err := writeSummary(path, summaryReport{Command: "cp"})
		syscall.Umask(old)
		assert.NilError(t, err)
		st, err := os.Stat(path)
		assert.NilError(t, err)
		assert.Equal(t, st.Mode().Perm(), os.FileMode(0o666&^mask))
		entries, _ := os.ReadDir(filepath.Dir(path))
		assert.Equal(t, len(entries), 1)
	}
}
