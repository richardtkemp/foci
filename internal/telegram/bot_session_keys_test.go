package telegram

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestReceiveMessage_FreshSlashCommandDispatched(t *testing.T) {
	// Verifies that fresh slash
	// commands are dispatched immediately.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	// Fresh message (timestamp = now) — should be dispatched normally
	msg := makeMsg(111, "owner", "/ping")
	b.receiveMessage(context.Background(), msg)

	// Command should have been dispatched and replied
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for fresh /ping, got %d", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("fresh slash command should not be queued")
	}
}

func TestReceiveMessage_StaleSlashCommandDropped(t *testing.T) {
	// Verifies that stale slash
	// commands are dropped without reply.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	// Create a message with a stale timestamp (60 seconds ago)
	msg := makeMsg(111, "owner", "/ping")
	msg.Date = int64(time.Now().Add(-60 * time.Second).Unix())
	b.receiveMessage(context.Background(), msg)

	// Stale slash command should be dropped — no reply, no queue
	if mock.sentCount() != 0 {
		t.Errorf("stale slash command should not send a reply, got %d sends", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("stale slash command should not be queued")
	}
}

func TestReceiveMessage_StaleNonSlashMessageStillQueued(t *testing.T) {
	// Verifies that stale
	// non-slash messages are still queued.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	// Create a plain text message with a stale timestamp (60 seconds ago)
	msg := makeMsg(111, "owner", "hello from the past")
	msg.Date = int64(time.Now().Add(-60 * time.Second).Unix())
	b.receiveMessage(context.Background(), msg)

	// Non-slash messages should still be queued regardless of age
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message for stale non-slash message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if qm.text != "hello from the past" {
		t.Errorf("queued text = %q, want %q", qm.text, "hello from the past")
	}
}

func TestNewSessionKeyForChat(t *testing.T) {
	// Verifies that session keys are created with the
	// correct chat prefix.
	key := NewSessionKeyForChat("fotini", 123456789)
	if !strings.HasPrefix(key, "fotini/c123456789/") {
		t.Errorf("got %q, want prefix %q", key, "fotini/c123456789/")
	}
}

func TestNewSessionKeyForChat_DifferentChats(t *testing.T) {
	// Verifies that different chat IDs
	// produce different session keys.
	k1 := NewSessionKeyForChat("fotini", 111)
	k2 := NewSessionKeyForChat("fotini", 222)
	if k1 == k2 {
		t.Error("different chat IDs should produce different session keys")
	}
}

func TestDefaultChatAssignment(t *testing.T) {
	// Verifies that the default chat is set on first
	// message and does not change.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionIndex = idx

	// No default initially
	if chatID := b.defaultChatID(); chatID != 0 {
		t.Errorf("expected no default, got %d", chatID)
	}

	// First message sets default
	msg := makeMsg(111, "alice", "hello")
	b.receiveMessage(context.Background(), msg)

	if chatID := b.defaultChatID(); chatID != 12345 {
		t.Errorf("expected default 12345, got %d", chatID)
	}

	// Second message from different chat doesn't change default
	msg2 := &gotgbot.Message{
		From: &gotgbot.User{Id: 111, Username: "alice"},
		Chat: gotgbot.Chat{Id: 99999},
		Text: "hello again",
		Date: int64(time.Now().Unix()),
	}
	b.receiveMessage(context.Background(), msg2)

	if chatID := b.defaultChatID(); chatID != 12345 {
		t.Errorf("expected default still 12345, got %d", chatID)
	}
}

func TestDefaultSessionKey(t *testing.T) {
	// Verifies that DefaultSessionKey returns the correct
	// session key for the default chat.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionIndex = idx

	// No default → empty
	if sk := b.DefaultSessionKey(); sk != "" {
		t.Errorf("expected empty, got %q", sk)
	}

	// Set default chat
	b.setDefaultChat(12345)
	if sk := b.DefaultSessionKey(); !strings.HasPrefix(sk, "test-agent/c12345/") {
		t.Errorf("expected prefix test-agent/c12345/, got %q", sk)
	}
}

func TestSessionKey_PrimaryBotUsesDefault(t *testing.T) {
	// Verifies that primary bots use the
	// default chat session key.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionKey = "" // primary bots don't have an override
	b.sessionIndex = idx
	b.setDefaultChat(12345)

	// SessionKey() should return the default chat session
	if sk := b.SessionKey(); !strings.HasPrefix(sk, "test-agent/c12345/") {
		t.Errorf("expected prefix test-agent/c12345/, got %q", sk)
	}
}

func TestSessionKey_PrimaryBotIsStable(t *testing.T) {
	// Verifies that SessionKey() returns the
	// same value on repeated calls.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionKey = ""
	b.sessionIndex = idx
	b.setDefaultChat(12345)

	k1 := b.SessionKey()
	k2 := b.SessionKey()
	if k1 != k2 {
		t.Errorf("SessionKey() not stable: %q vs %q", k1, k2)
	}
}

func TestDefaultSessionKey_IsStable(t *testing.T) {
	// Verifies that DefaultSessionKey() returns
	// the same value on repeated calls.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionIndex = idx
	b.setDefaultChat(12345)

	k1 := b.DefaultSessionKey()
	k2 := b.DefaultSessionKey()
	if k1 != k2 {
		t.Errorf("DefaultSessionKey() not stable: %q vs %q", k1, k2)
	}
}

func TestUpdateChatSessionKey_ChangesDefaultSessionKey(t *testing.T) {
	// Verifies that UpdateChatSessionKey updates the cached session key
	// so that subsequent DefaultSessionKey calls return the new key.
	// This is the mechanism used by /reset to rotate session keys.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionIndex = idx
	b.setDefaultChat(12345)

	oldKey := b.DefaultSessionKey()
	if oldKey == "" {
		t.Fatal("expected non-empty default session key")
	}

	newKey := "test-agent/c12345/9999999999"
	b.UpdateChatSessionKey(12345, newKey)

	got := b.DefaultSessionKey()
	if got != newKey {
		t.Errorf("DefaultSessionKey() after UpdateChatSessionKey = %q, want %q", got, newKey)
	}
	if got == oldKey {
		t.Error("DefaultSessionKey() should have changed after UpdateChatSessionKey")
	}
}

func TestSessionKey_SecondaryBotUsesOverride(t *testing.T) {
	// Verifies that secondary bots use
	// the configured session key override.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("agent:test:facet:f-123")

	if sk := b.SessionKey(); sk != "agent:test:facet:f-123" {
		t.Errorf("expected override key, got %q", sk)
	}
}

func TestChatUsernameRecording(t *testing.T) {
	// Verifies that chat usernames are recorded when
	// messages are received.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionIndex = idx

	msg := makeMsg(111, "alice", "hello")
	b.receiveMessage(context.Background(), msg)

	// For now, just verify the message was processed without panic
	// (testBot doesn't set up the state/storage for usernames)
}

func TestSetSessionKey_FiresCallback(t *testing.T) {
	// Verifies that SetSessionKey fires the
	// registered callback.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true

	called := false
	b.OnSessionKeyChange = func(username, sessionKey string) {
		called = true
	}

	b.SetSessionKey("new-key")
	if !called {
		t.Error("callback should have been called")
	}
	if b.SessionKey() != "new-key" {
		t.Errorf("session key not set: got %q", b.SessionKey())
	}
}

func TestSetSessionKey_NilCallbackDoesNotPanic(t *testing.T) {
	// Verifies that SetSessionKey
	// handles nil callback without panicking.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	// No callback set

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("SetSessionKey panicked with nil callback: %v", r)
		}
	}()
	b.SetSessionKey("test-key")
}

func TestSetSessionKeyDirect_DoesNotFireCallback(t *testing.T) {
	// Verifies that SetSessionKeyDirect
	// does not fire the callback.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true

	called := false
	b.OnSessionKeyChange = func(username, sessionKey string) {
		called = true
	}

	b.SetSessionKeyDirect("direct-key")
	if called {
		t.Error("callback should not have been called for SetSessionKeyDirect")
	}
	if b.SessionKey() != "direct-key" {
		t.Errorf("session key not set: got %q", b.SessionKey())
	}
}

func TestDispatchSessionKey_SecondaryUsesOverride(t *testing.T) {
	// Verifies that command dispatch for secondary bots uses the override
	// session key (branch key) rather than resolving from chatID.
	// This ensures /status shows the correct session in facet chats.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("agent/c12345/1000000000/b1111111111")

	got := b.dispatchSessionKey(12345)
	if got != "agent/c12345/1000000000/b1111111111" {
		t.Errorf("dispatchSessionKey() = %q, want override branch key", got)
	}
}

func TestDispatchSessionKey_SecondaryIdleFallsBack(t *testing.T) {
	// Verifies that an idle secondary bot (no override key) falls back
	// to chat-based session key resolution.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.isSecondary = true
	b.SetSessionKey("") // idle — no session assigned

	got := b.dispatchSessionKey(12345)
	if !strings.HasPrefix(got, "test-agent/c12345/") {
		t.Errorf("dispatchSessionKey() = %q, want prefix test-agent/c12345/", got)
	}
}

func TestDispatchSessionKey_PrimaryUsesChat(t *testing.T) {
	// Verifies that primary bots resolve session keys from chatID
	// (not affected by the secondary-bot override logic).
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"

	got := b.dispatchSessionKey(12345)
	if !strings.HasPrefix(got, "test-agent/c12345/") {
		t.Errorf("dispatchSessionKey() = %q, want prefix test-agent/c12345/", got)
	}
}

func TestUsername_NilSafe(t *testing.T) {
	// Verifies that the bot handles nil API without panicking.
	b, _ := testBot([]string{}, command.NewRegistry())
	// Bot created with testBot doesn't set API, so Username() should return empty
	// Just verify no panic when accessing Username()
	username := b.Username()
	// Username will be empty for test bots since API is nil
	_ = username
}
