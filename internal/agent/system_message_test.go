package agent

import (
	"testing"
	"time"

	"foci/shared/prompts"
)

func TestLastUserMessageTime_Default(t *testing.T) {
	// Proves that LastUserMessageTime returns a zero time for a session that has never received a message.
	a := &Agent{}
	got := a.LastUserMessageTime("test-session")
	if !got.IsZero() {
		t.Errorf("LastUserMessageTime for new session = %v, want zero", got)
	}
}

func TestLastUserMessageTime_AfterSeed(t *testing.T) {
	// Proves that LastUserMessageTime returns the seeded timestamp once session meta is populated.
	a := &Agent{}
	sm := a.getSessionMeta("test-session")
	now := time.Now()
	sm.lastMessageTime = now

	got := a.LastUserMessageTime("test-session")
	if !got.Equal(now) {
		t.Errorf("LastUserMessageTime = %v, want %v", got, now)
	}
}

func TestIsSystemMessage(t *testing.T) {
	// Proves that isSystemMessage recognises the messages the host actually
	// injects — both the FormatInjectedMessage-wrapped kind (proactive warnings,
	// scheduled wake, etc.) and the bare-tag keepalive — and rejects ordinary
	// human input. The prior test asserted a "[proactive system warnings]" prefix
	// that FormatInjectedMessage never produces, so the real payload slipped
	// through as a non-system message.
	warning := prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), "- [WARN] disk full")
	if !isSystemMessage(warning) {
		t.Errorf("isSystemMessage should recognise an injected proactive warning, got false for %q", warning)
	}
	wake := prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), "time to check in")
	if !isSystemMessage(wake) {
		t.Error("isSystemMessage should recognise an injected scheduled wake")
	}
	if !isSystemMessage("[KEEPALIVE] Cache keepalive ping. Respond with `[[NO_RESPONSE]]`.") {
		t.Error("isSystemMessage should recognise the bare-tag keepalive prefix")
	}
	if isSystemMessage("Hello, how are you?") {
		t.Error("isSystemMessage should not match regular human messages")
	}
}
