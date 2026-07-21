package ccstream

import (
	"context"

	"foci/internal/delegator"
)

// EnterVoiceMode switches this CC session to low effort for the duration of
// a voice-originated turn, saving whatever effort level was in effect so
// ExitVoiceMode can restore it afterwards. Implements delegator.VoiceModer
// (#1445). Fire-and-forget over the wire, like every other apply_flag_settings
// push — CC replies with a control_response nobody waits on.
func (b *Backend) EnterVoiceMode(ctx context.Context) error {
	b.mu.Lock()
	if b.voiceModeActive {
		// Already active — RunInference scopes one Enter per begin-turn
		// dispatch, so this shouldn't happen. Don't clobber the saved value
		// with "low" (that would lose the real pre-voice level on Exit).
		b.mu.Unlock()
		return nil
	}
	b.voiceModeSavedEffort = b.effortLevel
	b.voiceModeActive = true
	b.mu.Unlock()

	return b.SendControl(ctx, &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": "low"},
	})
}

// ExitVoiceMode restores the effort level EnterVoiceMode saved for this
// session, undoing the low-effort override once the voice-originated turn
// has completed. A concrete saved level ("low"/"medium"/"high"/"max") is
// pushed live via apply_flag_settings — mirroring Agent.SetSessionEffort's
// own live-push behaviour. An empty/"off" saved value means no explicit
// override was active before voice mode started; there is no live "clear the
// override" wire message (Agent.SetSessionEffort has the identical
// limitation — clearing only takes effect on the next launch), so the push is
// skipped and CC stays at "low" until the next explicit /effort or backend
// restart re-establishes a baseline.
func (b *Backend) ExitVoiceMode(ctx context.Context) error {
	b.mu.Lock()
	if !b.voiceModeActive {
		b.mu.Unlock()
		return nil
	}
	prev := b.voiceModeSavedEffort
	b.voiceModeActive = false
	b.voiceModeSavedEffort = ""
	b.mu.Unlock()

	if prev == "" || prev == "off" {
		return nil
	}
	return b.SendControl(ctx, &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": prev},
	})
}
