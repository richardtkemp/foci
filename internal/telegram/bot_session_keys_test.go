package telegram

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestReceiveMessage_FreshSlashCommandDispatched(t *testing.T) {
	// Verifies that fresh slash commands are routed to the command channel (not
	// the agent queue) and produce a reply when the worker dispatches them.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/ping")
	b.receiveMessage(context.Background(), msg)

	// Should be in the command channel, not the agent queue.
	if len(b.mq.Chan()) != 0 {
		t.Error("fresh slash command should not be in agent queue")
	}

	// Simulate worker dispatch.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for fresh /ping, got %d", mock.sentCount())
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

	// Stale slash command should be dropped -- no reply, no queue
	if mock.sentCount() != 0 {
		t.Errorf("stale slash command should not send a reply, got %d sends", mock.sentCount())
	}
	if len(b.mq.Chan()) != 0 {
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
	if len(b.mq.Chan()) != 1 {
		t.Fatalf("expected 1 queued message for stale non-slash message, got %d", len(b.mq.Chan()))
	}
	qm := <-b.mq.Chan()
	if qm.Text != "hello from the past" {
		t.Errorf("queued text = %q, want %q", qm.Text, "hello from the past")
	}
}

func TestDefaultChatAssignment(t *testing.T) {
	// Verifies that the default chat is set on first
	// message and does not change.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Two allowed users so #853's sole-user default-chat seeding does NOT fire
	// (this test exercises first-message default assignment, not seeding — the
	// seeding path has its own test in bot_seed_default_chat_test.go).
	b, _ := testBot([]string{"111", "222"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.chatmeta.AgentID = "test-agent"
	b.SetSessionIndex(idx)

	// No default initially
	if chatID := b.DefaultChatID(); chatID != 0 {
		t.Errorf("expected no default, got %d", chatID)
	}

	// First message sets default
	msg := makeMsg(111, "alice", "hello")
	b.receiveMessage(context.Background(), msg)

	if chatID := b.DefaultChatID(); chatID != 12345 {
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

	if chatID := b.DefaultChatID(); chatID != 12345 {
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
	// Two allowed users so #853's sole-user default-chat seeding does NOT fire
	// (see bot_seed_default_chat_test.go for the seeding path's own coverage).
	b, _ := testBot([]string{"111", "222"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.chatmeta.AgentID = "test-agent"
	b.SetSessionIndex(idx)

	// No default -> empty
	if sk := b.DefaultSessionKey(); sk != "" {
		t.Errorf("expected empty, got %q", sk)
	}

	// Set default chat
	_ = idx.SetDefaultChat("test-agent", platformName, 12345)
	if sk := b.DefaultSessionKey(); sk != "test-agent/c12345" {
		t.Errorf("expected test-agent/c12345, got %q", sk)
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
	b.chatmeta.AgentID = "test-agent"
	b.sessionKey = "" // primary bots don't have an override
	b.SetSessionIndex(idx)
	_ = idx.SetDefaultChat("test-agent", platformName, 12345)

	// SessionKey() should return the default chat session
	if sk := b.SessionKey(); sk != "test-agent/c12345" {
		t.Errorf("expected test-agent/c12345, got %q", sk)
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
	b.chatmeta.AgentID = "test-agent"
	b.sessionKey = ""
	b.SetSessionIndex(idx)
	_ = idx.SetDefaultChat("test-agent", platformName, 12345)

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
	b.chatmeta.AgentID = "test-agent"
	b.SetSessionIndex(idx)
	_ = idx.SetDefaultChat("test-agent", platformName, 12345)

	k1 := b.DefaultSessionKey()
	k2 := b.DefaultSessionKey()
	if k1 != k2 {
		t.Errorf("DefaultSessionKey() not stable: %q vs %q", k1, k2)
	}
}

func TestSessionKey_SecondaryBotUsesOverride(t *testing.T) {
	// Verifies that secondary bots use
	// the configured session key override.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("test/c1/b123")

	if sk := b.SessionKey(); sk != "test/c1/b123" {
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
	b.chatmeta.AgentID = "test-agent"
	b.SetSessionIndex(idx)

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
	b.SetSessionKey("agent/c12345/b1111111111")

	got := b.dispatchSessionKey(12345)
	if got != "agent/c12345/b1111111111" {
		t.Errorf("dispatchSessionKey() = %q, want override branch key", got)
	}
}

func TestDispatchSessionKey_SecondaryIdleFallsBack(t *testing.T) {
	// Verifies that an idle secondary bot (no override key) falls back
	// to chat-based session key resolution.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.chatmeta.AgentID = "test-agent"
	b.isSecondary = true
	b.SetSessionKey("") // idle -- no session assigned

	got := b.dispatchSessionKey(12345)
	if got != "test-agent/c12345" {
		t.Errorf("dispatchSessionKey() = %q, want test-agent/c12345", got)
	}
}

func TestDispatchSessionKey_PrimaryUsesChat(t *testing.T) {
	// Verifies that primary bots resolve session keys from chatID
	// (not affected by the secondary-bot override logic).
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.chatmeta.AgentID = "test-agent"

	got := b.dispatchSessionKey(12345)
	if got != "test-agent/c12345" {
		t.Errorf("dispatchSessionKey() = %q, want test-agent/c12345", got)
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
