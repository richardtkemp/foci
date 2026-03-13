package agent

import (
	"testing"
	"time"
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

func TestIsSystemMessage_ProactiveWarnings(t *testing.T) {
	// Proves that isSystemMessage identifies the proactive warnings prefix and rejects ordinary messages.
	if !isSystemMessage("[proactive system warnings]\n- [WARN] disk full") {
		t.Error("isSystemMessage should recognize proactive system warnings prefix")
	}
	if isSystemMessage("Hello, how are you?") {
		t.Error("isSystemMessage should not match regular messages")
	}
}
