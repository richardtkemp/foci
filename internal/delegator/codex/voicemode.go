package codex

import "context"

// EnterVoiceMode queues "low" as the effort override for this session's next
// turn, saving whatever override was pending (or "" if none, meaning the
// model default / cold-launch --effort was in effect) so ExitVoiceMode can
// restore it. Implements delegator.VoiceModer (#1445). Codex has no
// mid-session control protocol — pendingEffort is applied by
// applyPendingControls at the next beginTurn — so this is applied the same
// way any other effort override is.
func (b *Backend) EnterVoiceMode(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceModeActive {
		// Already active — shouldn't happen (one Enter per begin-turn
		// dispatch); don't clobber the saved value with "low".
		return nil
	}
	b.voiceModeSavedEffort = b.pendingEffort
	b.pendingEffort = "low"
	b.voiceModeActive = true
	return nil
}

// ExitVoiceMode restores the effort override EnterVoiceMode saved, once the
// voice-originated turn has completed.
func (b *Backend) ExitVoiceMode(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.voiceModeActive {
		return nil
	}
	b.pendingEffort = b.voiceModeSavedEffort
	b.voiceModeSavedEffort = ""
	b.voiceModeActive = false
	return nil
}
