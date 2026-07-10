package agent

import (
	"context"

	"foci/internal/log"
	"foci/internal/timeutil"
)

// CleanupEphemeralSessions deletes the backend transcript files of ephemeral
// (non-root) sessions whose last activity — or creation — predates
// retentionDays. FILES ONLY: the session_index rows and cc_resume_history are
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
		log.Warnf("ephemeral-cleanup", "[%s] query: %v", a.AgentID, err)
		return 0
	}

	deleted := 0
	for _, key := range keys {
		// A session may have produced several transcripts over its life (each
		// post-compaction JSONL is a distinct UUID). Delete them all.
		for _, id := range a.SessionIndex.AllCCResumes(key) {
			if err := a.DelegatedManager.CleanupBackendSession(ctx, id); err != nil {
				log.Warnf("ephemeral-cleanup", "[%s] delete %s (%s): %v", a.AgentID, key, id, err)
				continue
			}
			deleted++
		}
	}
	if deleted > 0 {
		log.Infof("ephemeral-cleanup", "[%s] deleted %d ephemeral transcript(s) older than %dd", a.AgentID, deleted, retentionDays)
	}
	return deleted
}
