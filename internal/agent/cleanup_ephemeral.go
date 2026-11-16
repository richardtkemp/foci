package agent

import (
	"context"

	"foci/internal/timeutil"
)

// CleanupEphemeralSessions deletes the backend transcript files of ephemeral
// (non-root) sessions whose last activity — or creation — predates
// retentionDays. FILES ONLY: the session_index rows and backend_resume_history
// rows are left intact as a historical record; a swept transcript is recorded
// by stamping swept_at on its resume row, so it is never re-attempted (#1801). This reclaims the disk used by cloned
// fork transcripts (reflection/keepalive/background/branch) without disturbing
// any live root session's files.
//
// retentionDays <= 0 disables cleanup. Returns the number of transcript files
// deleted. Safe to call on a schedule; a missing file is not an error.
func (a *Agent) CleanupEphemeralSessions(ctx context.Context, retentionDays int) int {
	if retentionDays <= 0 || a.SessionIndex == nil || a.DelegatedManager == nil {
		return 0
	}
	cutoff := timeutil.Now().AddDate(0, 0, -retentionDays)
	keys, err := a.SessionIndex.EphemeralSessionsOlderThan(a.AgentID, cutoff)
	if err != nil {
		a.taggedLog("ephemeral-cleanup").Warnf("query: %v", err)
		return 0
	}

	if len(keys) == 0 {
		return 0
	}

	// #3134: a session bound to an open app chat keeps its transcript,
	// however long idle (Dick, 2026-11-16: "preserve transcripts for any open
	// chat"). Checked before the scope opens, so a sweep whose every
	// candidate is held starts no backend server. An unreadable open set
	// skips the sweep: a delete cannot be undone, so it never runs on a guess.
	open, err := a.SessionIndex.OpenChatSessionKeys()
	if err != nil {
		a.taggedLog("ephemeral-cleanup").Warnf("open chats: %v; skipping this sweep", err)
		return 0
	}
	kept := keys[:0:0]
	held := 0
	for _, key := range keys {
		if open[key] {
			held++
			continue
		}
		kept = append(kept, key)
	}
	if held > 0 {
		a.taggedLog("ephemeral-cleanup").Infof("kept %d transcript(s) of open chats", held)
	}
	keys = kept
	if len(keys) == 0 {
		return 0
	}

	// One scope for the whole sweep. Backends that delete from disk get a
	// no-op; opencode acquires a single server here rather than once per
	// session — and without it an idle agent's sessions could never be
	// collected at all, because its server is down precisely because it is
	// idle (#1707). A failure is not fatal: the per-session deletes below then
	// fail individually and are logged as before.
	release, err := a.DelegatedManager.OpenBackendCleanupScope(ctx)
	if err != nil {
		a.taggedLog("ephemeral-cleanup").Warnf("open cleanup scope: %v", err)
	}
	defer release()

	deleted := 0
	for _, key := range keys {
		// A session may have produced several transcripts over its life (each
		// post-compaction JSONL is a distinct UUID). Delete them all.
		for _, id := range a.SessionIndex.UnsweptResumes(key) {
			// Another session still uses this transcript (a branch that
			// recorded its parent's id): release this session's claim and
			// leave the file to its owner (#3134).
			if a.SessionIndex.ResumeHeldElsewhere(key, id) {
				a.taggedLog("ephemeral-cleanup").Infof("kept %s (%s): another session still uses it", key, id)
				a.SessionIndex.MarkResumeSwept(key, id)
				continue
			}
			if err := a.DelegatedManager.CleanupBackendSession(ctx, id); err != nil {
				a.taggedLog("ephemeral-cleanup").Warnf("delete %s (%s): %v", key, id, err)
				continue
			}
			// Only after a successful delete: a failed one must stay eligible,
			// or a transient backend error exempts a real transcript forever.
			a.SessionIndex.MarkResumeSwept(key, id)
			deleted++
		}
	}
	if deleted > 0 {
		a.taggedLog("ephemeral-cleanup").Infof("deleted %d ephemeral transcript(s) older than %dd", deleted, retentionDays)
	}
	return deleted
}
