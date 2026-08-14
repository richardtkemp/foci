package log

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Our OWN rotation must not trip the stale-inode detector. rotateFile finishes
// by renaming a trimmed temp file over the live log, which installs a new inode
// at the path; with the rename done bare, any goroutine logging between it and
// the reopen took l.mu first, found its fd pointing at the replaced inode, and
// emitted the "replaced underneath the open writer" WARN — once per rotation,
// every rotation (observed at 19:53:19 daily on the live box). The swap now
// happens under the writer lock, so writers never observe the stale state.
//
// The pair below is the point: suppressing the warning outright would pass the
// first test and silently undo #1479, so the second asserts the detector still
// fires for an EXTERNAL replacement.

// captureStaleWarns counts stale-inode warnings via the warn hook. Deliberately
// NOT setOutput: reopening an event file rebuilds its stderr multiwriter, which
// would discard a substituted buffer on exactly the code path under test.
func captureStaleWarns(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	SetWarnHook(func(_ Level, _ string, msg string) {
		if strings.Contains(msg, "was replaced underneath the open writer") {
			mu.Lock()
			n++
			mu.Unlock()
		}
	})
	t.Cleanup(func() { SetWarnHook(nil) })
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// oldLine is a log line old enough that rotation will archive it, forcing the
// rename path rather than the "entire file is recent" fast return.
func oldLine() string {
	return time.Now().Add(-72*time.Hour).Format(time.RFC3339) + " INFO  [test] ancient\n"
}

func TestRotation_DoesNotWarnAboutItsOwnSwap(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	if err := os.WriteFile(eventPath, []byte(oldLine()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init(Config{Level: "INFO", EventFile: eventPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	staleWarns := captureStaleWarns(t)

	rotateAll(RotationConfig{
		Period:      time.Hour,
		Retention:   48 * time.Hour,
		MaxLineSize: 1024 * 1024,
		ArchiveDir:  filepath.Join(dir, "archive"),
		Files:       []string{eventPath},
	})
	// A write after the swap: this is the one that used to observe the stale fd.
	Infof("test", "after rotation")

	if n := staleWarns(); n != 0 {
		t.Errorf("our own rotation emitted %d stale-inode warning(s); the swap should be "+
			"invisible to writers (it is held under the writer lock)", n)
	}

	// The reopen must still be real: the line written after rotation has to land
	// in the file that is now at the path, not in an orphaned inode.
	got, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "after rotation") {
		t.Errorf("post-rotation write did not reach the live file — it went to the replaced "+
			"inode, which is the data loss #1479 exists to prevent. File:\n%s", got)
	}
}

func TestRotation_StillWarnsOnExternalReplace(t *testing.T) {
	// The control. A "fix" that just suppressed the warning, or disabled the
	// detector, passes the test above and quietly reverts #1479 — where an
	// integration test truncated the live api/payload logs out from under
	// foci-gw's fds for ~2 months, several MB/hour vanishing into unlinked
	// inodes with nothing visible to show for it.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	if err := Init(Config{Level: "INFO", EventFile: eventPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	staleWarns := captureStaleWarns(t)

	replaceFileExternally(t, eventPath)
	Infof("test", "after external replace")

	if staleWarns() == 0 {
		t.Error("an EXTERNAL replacement produced no stale-inode warning — the detector has " +
			"been defeated, not narrowed")
	}
}
