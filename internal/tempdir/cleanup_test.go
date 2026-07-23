package tempdir

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCleanOldFiles verifies that files older than maxAge are removed,
// recent files are kept, directories are skipped, and non-matching
// files are unaffected.
func TestCleanOldFiles(t *testing.T) {
	dir := t.TempDir()

	// Create an old file (backdate modification time).
	oldFile := filepath.Join(dir, "spawn-result-grep-001.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour) // 10 days ago
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}

	// Create a recent file.
	recentFile := filepath.Join(dir, "spawn-result-grep-002.txt")
	if err := os.WriteFile(recentFile, []byte("recent"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create a non-matching file that should not be touched.
	otherFile := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(otherFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(otherFile, old, old); err != nil {
		t.Fatal(err)
	}

	// Create a directory matching the glob — should be skipped.
	dirMatch := filepath.Join(dir, "spawn-result-dir-003.txt")
	if err := os.Mkdir(dirMatch, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dirMatch, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := CleanOldFiles(dir, "spawn-result-*.txt", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanOldFiles: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 removed, got %d", n)
	}

	// Old file should be gone.
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old file should have been removed")
	}
	// Recent file should still exist.
	if _, err := os.Stat(recentFile); err != nil {
		t.Errorf("recent file should still exist: %v", err)
	}
	// Non-matching file should still exist.
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("non-matching file should still exist: %v", err)
	}
	// Directory should still exist.
	if _, err := os.Stat(dirMatch); err != nil {
		t.Errorf("directory should still exist: %v", err)
	}
}

// TestCleanOldFilesEmptyDir confirms that running against an empty
// directory succeeds with zero removals.
func TestCleanOldFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()
	n, err := CleanOldFiles(dir, "spawn-result-*.txt", time.Hour)
	if err != nil {
		t.Fatalf("CleanOldFiles: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 removed, got %d", n)
	}
}

// TestCleanStaleRoot_WipesOrphansPreservesAppBlobs verifies the core wipe:
// every category of orphaned top-level state gets removed (regardless of
// name pattern — CleanStale doesn't glob-match, it wipes anything not
// explicitly excluded), while app-blobs/ and its contents survive untouched,
// and spawn/'s children are wiped but spawn/ itself is kept.
func TestCleanStaleRoot_WipesOrphansPreservesAppBlobs(t *testing.T) {
	root := t.TempDir()

	// A grab-bag of orphan categories named after the real ones observed —
	// none of these names are special-cased in the implementation, they're
	// wiped simply for not being "app-blobs" or "spawn".
	mustWriteFile(t, filepath.Join(root, "exec-123-funcs.sh"), "stale bridge funcs")
	mustWriteFile(t, filepath.Join(root, "exec-123.sock"), "x") // not a real socket, just a stand-in file
	mustMkdirWithFile(t, filepath.Join(root, "foci-spill-abc"), "big.json", "spilled tool result")
	mustMkdirWithFile(t, filepath.Join(root, "fgwcheck-1"), "cc-stub", "escaped integration test binary")
	mustMkdirWithFile(t, filepath.Join(root, "waldiag-1"), "cc-stub", "another escaped test dir")

	// app-blobs/ must survive completely untouched.
	blobFile := filepath.Join(root, "app-blobs", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	mustWriteFile(t, blobFile, "blob bytes")

	// tool-results/ must survive too: guard.go puts these paths into the
	// conversation, and a session outlives a gateway restart, so the model can
	// be asked to re-read one after a deploy.
	toolResultFile := filepath.Join(root, "tool-results", "tool-result-Bash-deadbeef.txt")
	mustWriteFile(t, toolResultFile, "spilled output referenced from history")

	// spawn/'s children get wiped, but spawn/ itself must remain (its
	// out-of-band owner/group/mode must not be disturbed).
	mustMkdirWithFile(t, filepath.Join(root, "spawn", "foci-spawn-1"), "cmd.txt", "sandboxed")
	mustMkdirWithFile(t, filepath.Join(root, "spawn", "foci-spawn-2"), "cmd.txt", "sandboxed")

	result := cleanStaleRoot(root)

	if result.Failed != 0 {
		t.Errorf("expected 0 failures, got %d", result.Failed)
	}
	if result.Removed == 0 {
		t.Error("expected at least one entry removed")
	}
	if result.Bytes == 0 {
		t.Error("expected non-zero bytes freed")
	}

	// Orphans gone.
	for _, name := range []string{"exec-123-funcs.sh", "exec-123.sock", "foci-spill-abc", "fgwcheck-1", "waldiag-1"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat err = %v", name, err)
		}
	}

	// app-blobs/ and its contents untouched.
	if _, err := os.Stat(blobFile); err != nil {
		t.Errorf("app-blobs content should survive: %v", err)
	}

	// tool-results/ and its contents untouched.
	if _, err := os.Stat(toolResultFile); err != nil {
		t.Errorf("tool-results content should survive: %v", err)
	}

	// spawn/ itself survives; its children do not.
	if info, err := os.Stat(filepath.Join(root, "spawn")); err != nil || !info.IsDir() {
		t.Errorf("spawn/ directory itself should survive: %v", err)
	}
	for _, name := range []string{"foci-spawn-1", "foci-spawn-2"} {
		if _, err := os.Stat(filepath.Join(root, "spawn", name)); !os.IsNotExist(err) {
			t.Errorf("spawn/%s should have been removed, stat err = %v", name, err)
		}
	}
}

// TestCleanStaleRoot_UnremovableEntryCountsFailedNotFatal verifies that an
// entry this process can't remove (e.g. a directory it can't read into,
// standing in for another rich-readers-group uid's mode-2700 sandbox) is
// counted as a failure and skipped — not treated as fatal, and doesn't
// prevent removal of other, removable entries.
func TestCleanStaleRoot_UnremovableEntryCountsFailedNotFatal(t *testing.T) {
	root := t.TempDir()

	unreadable := filepath.Join(root, "foci-spawn-other-uid")
	mustMkdirWithFile(t, unreadable, "secret.txt", "not mine")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0755) }) // let t.TempDir() clean up

	removable := filepath.Join(root, "foci-spill-ok")
	mustMkdirWithFile(t, removable, "data.json", "fine")

	result := cleanStaleRoot(root)

	if result.Failed != 1 {
		t.Errorf("expected 1 failure, got %d", result.Failed)
	}
	if _, err := os.Stat(unreadable); err != nil {
		t.Errorf("unremovable entry should still be present: %v", err)
	}
	if _, err := os.Stat(removable); !os.IsNotExist(err) {
		t.Errorf("removable sibling should have been removed, stat err = %v", err)
	}
}

// TestCleanStaleRoot_MissingRootIsNotFatal verifies that a nonexistent root
// (e.g. resolveRoot degraded and the dir was never created) returns a
// zero-value result rather than erroring or panicking — a wipe failure must
// never block startup.
func TestCleanStaleRoot_MissingRootIsNotFatal(t *testing.T) {
	result := cleanStaleRoot(filepath.Join(t.TempDir(), "does-not-exist"))
	if result.Removed != 0 || result.Failed != 0 || result.Bytes != 0 {
		t.Errorf("expected zero-value result for a missing root, got %+v", result)
	}
}

// TestCleanStale_HonorsResolvedRootNotHardcodedPath proves CleanStale()
// operates on the process's resolved root (tempdir.Dir(), which honors
// FOCI_TMPDIR) rather than a hardcoded "/tmp/foci" — the exact failure class
// flagged as a hard constraint: a hardcoded path would make a test/second
// instance wipe a live install's state.
//
// resolve()'s root is memoized once per process (sync.Once), so this test
// forces re-resolution against a scratch override, runs CleanStale() against
// it, and restores the original memoized state afterward so it can't leak
// into other tests or (worse) ever touch a real /tmp/foci.
func TestCleanStale_HonorsResolvedRootNotHardcodedPath(t *testing.T) {
	origOverride, hadOverride := os.LookupEnv(EnvOverride)
	t.Cleanup(func() {
		if hadOverride {
			_ = os.Setenv(EnvOverride, origOverride)
		} else {
			_ = os.Unsetenv(EnvOverride)
		}
		// Reset so the NEXT resolve() (this test's or any later test's)
		// recomputes against the restored env rather than staying pinned
		// to this test's scratch override.
		resetResolveForTest()
	})

	scratch := t.TempDir()
	if err := os.Setenv(EnvOverride, scratch); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	resetResolveForTest() // force resolve() to re-read the override

	if got := Dir(); got != scratch {
		t.Fatalf("Dir() = %q after forcing FOCI_TMPDIR=%q, want the override honored", got, scratch)
	}

	mustWriteFile(t, filepath.Join(scratch, "exec-999-funcs.sh"), "stale")
	blobFile := filepath.Join(scratch, "app-blobs", "some-blob-id")
	mustWriteFile(t, blobFile, "keep me")

	result := CleanStale()

	if result.Removed != 1 {
		t.Errorf("expected 1 orphan removed under the override root, got %d", result.Removed)
	}
	if _, err := os.Stat(filepath.Join(scratch, "exec-999-funcs.sh")); !os.IsNotExist(err) {
		t.Error("orphan under the override root should have been removed")
	}
	if _, err := os.Stat(blobFile); err != nil {
		t.Errorf("app-blobs under the override root should survive: %v", err)
	}
	// Never touched: had CleanStale used a hardcoded "/tmp/foci" instead of
	// the resolved override, this scratch dir would be untouched and the
	// assertions above would fail (nothing to remove).
}

// resetResolveForTest forces the next resolve() call to recompute the root
// from the current environment instead of returning the process-memoized
// value. Test-only: production code never needs to re-resolve mid-process.
func resetResolveForTest() {
	resolveOnce = sync.Once{}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirWithFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	mustWriteFile(t, filepath.Join(dir, filename), content)
}
