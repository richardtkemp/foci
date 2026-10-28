package main

import (
	"foci/internal/display"
	"foci/internal/tempdir"
)

// logTempCleanup reports the startup temp-root wipe (tempdir.CleanStale):
//   - skipped: one line saying so and why. Info when another living gateway
//     uses the root (a normal outcome); Warn when the lock could not be
//     checked, which is the fail-safe arm and worth an operator's look.
//   - ran and touched something: the "temp cleanup: reclaimed ..." line.
//   - ran with nothing to do: silent.
//
// After a wipe that ran, a lock error means the post-wipe downgrade to a
// shared lock failed; that gets its own Warn.
func logTempCleanup(r tempdir.CleanStaleResult) {
	if r.SkipReason != "" {
		if r.LockErr != nil {
			startupLog.Warnf("temp cleanup skipped: %s", r.SkipReason)
		} else {
			startupLog.Infof("temp cleanup skipped: %s", r.SkipReason)
		}
		return
	}
	if r.Removed > 0 || r.Failed > 0 {
		startupLog.Infof("temp cleanup: reclaimed %d entries (%s), %d failed", r.Removed, display.FormatBytes(r.Bytes), r.Failed)
	}
	if r.LockErr != nil {
		startupLog.Warnf("temp cleanup: %v; other gateways starting on this temp root will skip their own wipe after a wait", r.LockErr)
	}
}
