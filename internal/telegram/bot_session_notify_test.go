package telegram

import (
	"path/filepath"
	"testing"

	"foci/internal/command"
	"foci/internal/session"
)

// setDefaultChat wires a session index with a default chat onto the bot so the
// session-aware routing has a fallback target to (not) use.
func setDefaultChat(t *testing.T, b *Bot, agentID string, chatID int64) {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b.agentID = agentID
	b.chatmeta.AgentID = agentID
	b.SetSessionIndex(idx)
	if err := idx.SetDefaultChat(agentID, platformName, chatID); err != nil {
		t.Fatal(err)
	}
}

// TestSendNotificationToSession_RoutesToSessionChat is the core #911 fix: a
// per-session notice goes to the chat embedded in the session key, NOT the
// default chat — even when a (different) default chat is configured.
func TestSendNotificationToSession_RoutesToSessionChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999) // default is chat 999...

	msgID := b.SendNotificationToSession("main/c222", "compacting…")

	if mock.lastSendChatID != 222 {
		t.Errorf("routed to chat %d, want 222 (the session's chat, not the default 999)", mock.lastSendChatID)
	}
	if msgID == "" {
		t.Error("expected a non-empty message ID for a later in-place edit")
	}
}

// TestSendNotificationToSession_FallsBackToDefault: a session key with no
// embedded chat ID (independent/agent-initiated session) falls back to the
// default chat — preserving today's behaviour for unparseable keys.
func TestSendNotificationToSession_FallsBackToDefault(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999)

	b.SendNotificationToSession("main/i1709596800", "system notice")

	if mock.lastSendChatID != 999 {
		t.Errorf("routed to chat %d, want 999 (default fallback for a chatless key)", mock.lastSendChatID)
	}
}

// TestSendNotificationToSession_SkipsEmpty proves empty text is a no-op.
func TestSendNotificationToSession_SkipsEmpty(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	if id := b.SendNotificationToSession("main/c222", "  "); id != "" {
		t.Errorf("empty text should return empty id, got %q", id)
	}
	if mock.sentCount() != 0 {
		t.Errorf("empty text should send nothing, sentCount=%d", mock.sentCount())
	}
}

// TestEditNotificationInSession_TargetsSessionChat: the ⏳→✅ in-place edit must
// target the session's chat (Telegram's editMessageText needs chatID+msgID; the
// default chat would reject a msgID from another chat) (#911).
func TestEditNotificationInSession_TargetsSessionChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999)

	if err := b.EditNotificationInSession("main/c222", "5", "✅ done"); err != nil {
		t.Fatalf("EditNotificationInSession: %v", err)
	}
	if mock.lastEditOpts == nil || mock.lastEditOpts.ChatId != 222 {
		t.Errorf("edit targeted chat %v, want 222 (the session's chat)", mock.lastEditOpts)
	}
}
