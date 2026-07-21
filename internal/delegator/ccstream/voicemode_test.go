package ccstream

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"foci/internal/delegator"
)

// readOneLine reads a single line off pr in the background and delivers it
// (or a zero-value timeout) on the returned channel — used to observe (or
// assert the absence of) a control_request write.
func readOneLine(pr *io.PipeReader) <-chan []byte {
	ch := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		ch <- buf[:n]
	}()
	return ch
}

func decodeApplyFlagSettings(t *testing.T, line []byte) string {
	t.Helper()
	var env struct {
		Type    string          `json:"type"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (line=%q)", err, line)
	}
	if env.Type != "control_request" {
		t.Fatalf("type = %q, want %q", env.Type, "control_request")
	}
	var req struct {
		Subtype  string         `json:"subtype"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(env.Request, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Subtype != "apply_flag_settings" {
		t.Fatalf("subtype = %q, want %q", req.Subtype, "apply_flag_settings")
	}
	eff, _ := req.Settings["effortLevel"].(string)
	return eff
}

// TestVoiceMode_EnterPushesLowAndSavesPrior verifies EnterVoiceMode sends
// effortLevel="low" over the wire and records the prior level so Exit can
// restore it.
func TestVoiceMode_EnterPushesLowAndSavesPrior(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	b := &Backend{
		writer:      NewWriter(pw),
		effortLevel: "high",
	}
	lineCh := readOneLine(pr)

	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("EnterVoiceMode: %v", err)
	}
	pw.Close()

	line := <-lineCh
	if got := decodeApplyFlagSettings(t, line); got != "low" {
		t.Errorf("effortLevel pushed = %q, want %q", got, "low")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.voiceModeActive {
		t.Error("voiceModeActive should be true after Enter")
	}
	if b.voiceModeSavedEffort != "high" {
		t.Errorf("voiceModeSavedEffort = %q, want %q", b.voiceModeSavedEffort, "high")
	}
	if b.effortLevel != "low" {
		t.Errorf("effortLevel = %q, want %q (foci's own record tracks the live push)", b.effortLevel, "low")
	}
}

// TestVoiceMode_ExitRestoresSavedEffort verifies ExitVoiceMode pushes the
// pre-voice-mode level back and clears voice-mode state.
func TestVoiceMode_ExitRestoresSavedEffort(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	b := &Backend{
		writer:               NewWriter(pw),
		effortLevel:          "low",
		voiceModeActive:      true,
		voiceModeSavedEffort: "medium",
	}
	lineCh := readOneLine(pr)

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}
	pw.Close()

	line := <-lineCh
	if got := decodeApplyFlagSettings(t, line); got != "medium" {
		t.Errorf("effortLevel restored = %q, want %q", got, "medium")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceModeActive {
		t.Error("voiceModeActive should be false after Exit")
	}
	if b.voiceModeSavedEffort != "" {
		t.Errorf("voiceModeSavedEffort = %q, want cleared", b.voiceModeSavedEffort)
	}
	if b.effortLevel != "medium" {
		t.Errorf("effortLevel = %q, want %q", b.effortLevel, "medium")
	}
}

// TestVoiceMode_ExitNoOpWhenNotActive verifies ExitVoiceMode without a prior
// Enter sends nothing over the wire and doesn't panic.
func TestVoiceMode_ExitNoOpWhenNotActive(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()
	b := &Backend{writer: NewWriter(pw)}
	lineCh := readOneLine(pr)

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}

	select {
	case line := <-lineCh:
		t.Fatalf("ExitVoiceMode without a prior Enter wrote to the wire: %q", line)
	case <-time.After(50 * time.Millisecond):
		// expected — no write
	}
}

// TestVoiceMode_ExitSkipsPushWhenNoPriorOverride verifies that when
// EnterVoiceMode saved an empty (unset) prior effort, ExitVoiceMode skips the
// live push (mirrors Agent.SetSessionEffort's own "clear/off skip the live
// push" rule) but still clears voice-mode state.
func TestVoiceMode_ExitSkipsPushWhenNoPriorOverride(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()
	b := &Backend{
		writer:               NewWriter(pw),
		effortLevel:          "low",
		voiceModeActive:      true,
		voiceModeSavedEffort: "",
	}
	lineCh := readOneLine(pr)

	if err := b.ExitVoiceMode(context.Background()); err != nil {
		t.Fatalf("ExitVoiceMode: %v", err)
	}

	select {
	case line := <-lineCh:
		t.Fatalf("ExitVoiceMode with no prior override wrote to the wire: %q", line)
	case <-time.After(50 * time.Millisecond):
		// expected — no live "clear" push exists
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceModeActive {
		t.Error("voiceModeActive should be false after Exit")
	}
	// effortLevel stays at whatever the low-effort push left it at — there's
	// no live way to "unset" mid-session (see doc comment).
	if b.effortLevel != "low" {
		t.Errorf("effortLevel = %q, want %q (unchanged — no live clear)", b.effortLevel, "low")
	}
}

// TestVoiceMode_DoubleEnterDoesNotClobberSavedValue verifies a second
// EnterVoiceMode call (which shouldn't happen in production — RunInference
// scopes one Enter per begin-turn dispatch) is a safe no-op rather than
// overwriting the saved prior effort with "low".
func TestVoiceMode_DoubleEnterDoesNotClobberSavedValue(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	b := &Backend{
		writer:      NewWriter(pw),
		effortLevel: "high",
	}
	lineCh := readOneLine(pr)
	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("first EnterVoiceMode: %v", err)
	}
	<-lineCh // drain the first push

	lineCh2 := readOneLine(pr)
	if err := b.EnterVoiceMode(context.Background()); err != nil {
		t.Fatalf("second EnterVoiceMode: %v", err)
	}
	pw.Close()

	select {
	case line := <-lineCh2:
		if len(line) > 0 {
			t.Fatalf("second EnterVoiceMode while already active wrote to the wire: %q", line)
		}
	case <-time.After(50 * time.Millisecond):
		// expected
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
