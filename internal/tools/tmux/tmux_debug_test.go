package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/tempdir"
)

// TestDebugLogWritesUnderTempRoot guards #1094: the send debug log used to go
// to a cwd-relative logs/tmux.debug, which under `go test` is the package dir
// inside the worktree — leaving a 0600 file owned by whichever uid ran the
// tests, so another uid could no longer `git worktree remove` it.
func TestDebugLogWritesUnderTempRoot(t *testing.T) {
	marker := fmt.Sprintf("debug-log-marker-%d", time.Now().UnixNano())
	logDebug("send", marker)

	path := filepath.Join(tempdir.Dir(), "tmux.debug")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("debug log not found under temp root: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Fatalf("debug log %s does not contain marker %q", path, marker)
	}
}
