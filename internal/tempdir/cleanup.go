package tempdir

import (
	"os"
	"path/filepath"
	"time"
)

// CleanOldFiles removes regular files matching the given glob pattern in dir
// whose modification time is older than maxAge. Returns the count of removed files.
func CleanOldFiles(dir, pattern string, maxAge time.Duration) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, path := range matches {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// appBlobsDir names the app blob store's private subdirectory (see
// internal/app/blob.go's newBlobStore) under the temp root. It is the blob
// store's own restart-durable state and must never be touched by CleanStale,
// under any circumstance.
const appBlobsDir = "app-blobs"

// spawnDirName names the spawn-sandbox subdirectory (see SpawnDir). Its
// individual children are wiped like any other orphaned state, but the
// directory itself is left in place — on the live host it's set up
// out-of-band with a specific owner/group and mode (observed: rich:rich-readers,
// 2775) that CleanStale must not disturb by removing and letting SpawnDir()
// recreate it (which would mint it with whatever default mode MkdirAll asks
// for, not the one actually in place).
const spawnDirName = "spawn"

// toolResultsDir names the tool-result overflow subdirectory (config
// tools.temp_dir, default <root>/tool-results). The guard in
// internal/agent/guard.go spills an oversized tool result to a file here and
// puts that PATH into the conversation — and foci sessions deliberately
// outlive a gateway restart, so the model can legitimately be asked to re-read
// one of these files in a later turn, on the far side of a deploy. Wiping them
// at startup would turn that into a silent "no such file". Excluded here and
// left to age out via the age-based sweep instead (see CleanOldFiles).
const toolResultsDir = "tool-results"

// CleanStaleResult summarizes one CleanStale run: how many entries were
// reclaimed, how many bytes that freed, and how many entries were left in
// place because removal failed.
type CleanStaleResult struct {
	Removed int   // entries successfully removed
	Bytes   int64 // total size of removed entries (best-effort)
	Failed  int   // entries that could not be removed
}

// CleanStale performs a best-effort wipe of orphaned top-level temp state
// under the resolved root. It is meant to run exactly once, at foci-gw
// startup, AFTER the root is resolved (see resolve/resolveRoot) and BEFORE
// any per-process bridge, blob, or spawn-sandbox dir is created.
//
// Startup is the one moment "everything already on disk is orphaned" is
// provable rather than inferred: no session exists yet, so no exec bridge,
// spill file, pairing scratch, or browser scratch can possibly be referenced
// by a live process. That's strictly better than an age heuristic — a
// session idle >24h holds a stale-but-LIVE BASH_ENV funcs.sh that an
// age-based reaper would delete, breaking every foci_* tool in that session
// on next wake.
//
// app-blobs/ is EXCLUDED entirely: it is the app blob store's own
// restart-durable directory (observed mode 2700, owned by the daemon's own
// uid) and is owned by that store, not this wipe — see appBlobsDir.
//
// CleanStale never panics out to the caller and never treats a removal
// failure as fatal. The root is NOT sticky and NOT world-writable — it's
// owned by the daemon's uid with a `rich-readers` group (POSIX ACL default
// grants the group rwx, so any member can create/unlink entries here), and
// individual entries (e.g. a spawn/ sandbox dir mode 2700, owned by a
// different `rich-readers` member) can still be unreadable to this process,
// making removal fail even though it's technically "our" group. Those
// failures are counted, never aborted on. A cleanup failure here must never
// block gateway startup.
func CleanStale() CleanStaleResult {
	return cleanStaleRoot(Dir())
}

// cleanStaleRoot is the testable core of CleanStale: it operates on an
// explicit root rather than the process-wide resolved Dir(), so tests can
// exercise it against a scratch directory without ever touching a live
// install's /tmp/foci.
func cleanStaleRoot(root string) (result CleanStaleResult) {
	defer func() {
		// Best-effort: a panic during the wipe must never block startup.
		_ = recover()
	}()

	entries, err := os.ReadDir(root)
	if err != nil {
		return result
	}

	for _, e := range entries {
		if e.Name() == appBlobsDir {
			continue // owned by the blob store; never wiped here
		}
		if e.Name() == toolResultsDir {
			continue // path is referenced from conversation history — see toolResultsDir
		}
		path := filepath.Join(root, e.Name())
		if e.Name() == spawnDirName && e.IsDir() {
			// Clean spawn/'s children but keep spawn/ itself — see spawnDirName.
			r := cleanDirContents(path)
			result.Removed += r.Removed
			result.Bytes += r.Bytes
			result.Failed += r.Failed
			continue
		}
		if size, ok := removeEntry(path, e); ok {
			result.Removed++
			result.Bytes += size
		} else {
			result.Failed++
		}
	}
	return result
}

// cleanDirContents removes every child of dir, leaving dir itself in place.
// Used for spawn/, whose individual sandbox subdirectories are mode 2700 and
// each owned by one of the two uids that create them — this process can
// fail to remove the other's, and each such failure is counted, never
// fatal.
func cleanDirContents(dir string) (result CleanStaleResult) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if size, ok := removeEntry(path, e); ok {
			result.Removed++
			result.Bytes += size
		} else {
			result.Failed++
		}
	}
	return result
}

// removeEntry removes one directory entry (file, symlink, or directory
// tree), returning its size in bytes and whether removal succeeded. Group
// write on the parent directory lets this process unlink a plain file it
// doesn't own, but a directory it can't read into (e.g. another uid's
// mode-2700 spawn sandbox) still fails to remove — a failure is reported to
// the caller and skipped, never treated as fatal.
func removeEntry(path string, e os.DirEntry) (int64, bool) {
	info, statErr := e.Info()
	isDir := statErr == nil && info.IsDir()

	var size int64
	switch {
	case statErr != nil:
		size = 0 // unknown; best-effort byte accounting only
	case isDir:
		size = dirSize(path)
	default:
		size = info.Size()
	}

	var rmErr error
	if isDir {
		rmErr = os.RemoveAll(path)
	} else {
		rmErr = os.Remove(path)
	}
	if rmErr != nil {
		return 0, false
	}
	return size, true
}

// dirSize best-effort sums the size of every regular file under path. Errors
// (permission, a dangling entry, a concurrent removal) are skipped rather
// than propagated — this only feeds the "bytes freed" log line, never gates
// removal.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
