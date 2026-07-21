package codex

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// TestVoiceMode_EnterQueuesLowAndSavesPrior verifies EnterVoiceMode saves
// whatever effort override was pending and queues "low" in its place, so the
// next turn/start applies low effort (Codex has no mid-session control
// channel — pendingEffort is read by applyPendingControls at beginTurn).
func TestVoiceMode_EnterQueuesLowAndSavesPrior(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	b.pendingEffort = "high"

	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("EnterVoiceMode: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.voiceModeActive {
		t.Error("voiceModeActive should be true after Enter")
	}
	if b.voiceModeSavedEffort != "high" {
		t.Errorf("voiceModeSavedEffort = %q, want %q", b.voiceModeSavedEffort, "high")
	}
	if b.pendingEffort != "low" {
		t.Errorf("pendingEffort = %q, want %q", b.pendingEffort, "low")
	}
}

// TestVoiceMode_ExitRestoresSavedEffort verifies ExitVoiceMode restores the
// pendingEffort EnterVoiceMode saved and clears voice-mode state.
func TestVoiceMode_ExitRestoresSavedEffort(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	b.pendingEffort = "low"
	b.voiceModeActive = true
	b.voiceModeSavedEffort = "medium"

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceModeActive {
		t.Error("voiceModeActive should be false after Exit")
	}
	if b.voiceModeSavedEffort != "" {
		t.Errorf("voiceModeSavedEffort = %q, want cleared", b.voiceModeSavedEffort)
	}
	if b.pendingEffort != "medium" {
		t.Errorf("pendingEffort = %q, want %q", b.pendingEffort, "medium")
	}
}

// TestVoiceMode_ExitRestoresEmptyPrior verifies that when there was no
// pending override before voice mode (pendingEffort == ""), Exit restores
// exactly that — the next turn/start falls back to the model default,
// mirroring applyPendingControls' own "" == no override contract.
func TestVoiceMode_ExitRestoresEmptyPrior(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	b.pendingEffort = "low"
	b.voiceModeActive = true
	b.voiceModeSavedEffort = ""

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}

	b.mu.Lock()
	pendingEffort := b.pendingEffort
	b.mu.Unlock()
	if pendingEffort != "" {
		t.Errorf("pendingEffort = %q, want empty (no override)", pendingEffort)
	}

	params := &turnStartParams{ThreadID: "th-1", Cwd: "/tmp"}
	b.applyPendingControls(params)
	if params.Effort != "" {
		t.Errorf("params.Effort = %q, want empty after restoring no-override", params.Effort)
	}
}

// TestVoiceMode_ExitNoOpWhenNotActive verifies ExitVoiceMode without a prior
// Enter leaves pendingEffort untouched.
func TestVoiceMode_ExitNoOpWhenNotActive(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	b.pendingEffort = "high"

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingEffort != "high" {
		t.Errorf("pendingEffort = %q, want unchanged %q", b.pendingEffort, "high")
	}
}

// TestVoiceMode_DoubleEnterDoesNotClobberSavedValue verifies a second Enter
// call (shouldn't happen in production) doesn't overwrite the saved prior
// effort with "low".
func TestVoiceMode_DoubleEnterDoesNotClobberSavedValue(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	b.pendingEffort = "high"

	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("first EnterVoiceMode: %v", err)
	}
	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("second EnterVoiceMode: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceModeSavedEffort != "high" {
		t.Errorf("voiceModeSavedEffort = %q, want %q (unchanged by the second Enter)", b.voiceModeSavedEffort, "high")
	}
}

// TestVoiceMode_ImplementsInterface is a compile-time-ish check that Backend
// satisfies delegator.VoiceModer.
func TestVoiceMode_ImplementsInterface(t *testing.T) {
	var _ delegator.VoiceModer = (*Backend)(nil)
}
