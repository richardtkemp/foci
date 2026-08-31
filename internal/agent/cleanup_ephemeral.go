package agent

import (
	"context"

	"foci/internal/timeutil"
)

// CleanupEphemeralSessions deletes the backend transcript files of ephemeral
// (non-root) sessions whose last activity — or creation — predates
// retentionDays. FILES ONLY: the session_index rows and backend_resume_history are
// left intact as a historical record. This reclaims the disk used by cloned
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
		for _, id := range a.SessionIndex.AllBackendResumes(key) {
			if err := a.DelegatedManager.CleanupBackendSession(ctx, id); err != nil {
				a.taggedLog("ephemeral-cleanup").Warnf("delete %s (%s): %v", key, id, err)
				continue
			}
			deleted++
		}
	}
	if deleted > 0 {
		a.taggedLog("ephemeral-cleanup").Infof("deleted %d ephemeral transcript(s) older than %dd", deleted, retentionDays)
	}
	return deleted
}
