package discord

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/session"
)

// TestNewSessionKeyForChat verifies that new session keys follow the expected format
// and contain the agent ID and chat ID.
func TestNewSessionKeyForChat(t *testing.T) {
	key := NewSessionKeyForChat("myagent", 123456789)
	if key == "" {
		t.Fatal("expected non-empty session key")
	}
	if !strings.HasPrefix(key, "myagent/") {
		t.Errorf("expected key to start with agent ID, got %q", key)
	}
	if !strings.Contains(key, "c123456789") {
		t.Errorf("expected key to contain chat ID, got %q", key)
	}
}

// TestNewSessionKeyForChatDifferentChats verifies that different chat IDs produce
// different keys even at the same timestamp.
func TestNewSessionKeyForChatDifferentChats(t *testing.T) {
	key1 := NewSessionKeyForChat("agent", 100)
	key2 := NewSessionKeyForChat("agent", 200)
	if key1 == key2 {
		t.Errorf("expected different keys for different chats, got identical: %q", key1)
	}
}

// TestSessionKeyForChatCaching verifies that repeated calls to SessionKeyForChat
// return the same persisted key from the DB.
func TestSessionKeyForChatCaching(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		agentID:      "test",
		sessionIndex: idx,
	}
	key1 := bot.SessionKeyForChat(42)
	key2 := bot.SessionKeyForChat(42)
	if key1 != key2 {
		t.Errorf("expected cached key, got %q and %q", key1, key2)
	}
}

// TestSessionKeyForChatDifferentChats verifies that different chat IDs produce
// different session keys.
func TestSessionKeyForChatDifferentChats(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		agentID:      "test",
		sessionIndex: idx,
	}
	key1 := bot.SessionKeyForChat(1)
	key2 := bot.SessionKeyForChat(2)
	if key1 == key2 {
		t.Errorf("expected different keys for different chats, got %q", key1)
	}
}

// TestSetSessionKey verifies that SetSessionKey updates the key and fires the callback.
func TestSetSessionKey(t *testing.T) {
	var calledWith string
	bot := &Bot{

		botUserID:       "bot123",
		OnSessionKeyChange: func(botID, sessionKey string) {
			calledWith = sessionKey
		},
	}

	bot.SetSessionKey("agent/c100/12345")
	if got := bot.SessionKey(); got != "agent/c100/12345" {
		t.Errorf("expected session key agent/c100/12345, got %q", got)
	}
	if calledWith != "agent/c100/12345" {
		t.Errorf("expected callback with agent/c100/12345, got %q", calledWith)
	}
}

// TestSetSessionKeyDirect verifies that SetSessionKeyDirect does NOT fire the callback.
func TestSetSessionKeyDirect(t *testing.T) {
	called := false
	bot := &Bot{

		OnSessionKeyChange: func(_, _ string) {
			called = true
		},
	}

	bot.SetSessionKeyDirect("test/c1/999")
	if called {
		t.Error("SetSessionKeyDirect should not fire OnSessionKeyChange")
	}
	if got := bot.SessionKey(); got != "test/c1/999" {
		t.Errorf("expected session key test/c1/999, got %q", got)
	}
}

// TestUpdateChatSessionKey verifies that updating a chat session key overwrites
// the persisted value in the DB.
func TestUpdateChatSessionKey(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		agentID:      "agent",
		sessionIndex: idx,
	}

	// First access creates a key
	key1 := bot.SessionKeyForChat(55)

	// Update should replace it
	bot.UpdateChatSessionKey(55, "agent/c55/newversion")
	key2 := bot.SessionKeyForChat(55)

	if key1 == key2 {
		t.Error("expected different key after update")
	}
	if key2 != "agent/c55/newversion" {
		t.Errorf("expected updated key, got %q", key2)
	}
}

// TestChatIDGetSet verifies the ChatID getter/setter.
func TestChatIDGetSet(t *testing.T) {
	bot := &Bot{}
	if bot.ChatID() != 0 {
		t.Errorf("expected 0, got %d", bot.ChatID())
	}
	bot.SetChatID(999)
	if bot.ChatID() != 999 {
		t.Errorf("expected 999, got %d", bot.ChatID())
	}
}

// TestUsernameReturnsBotUserID verifies that Username() returns the bot's user ID.
func TestUsernameReturnsBotUserID(t *testing.T) {
	bot := &Bot{
		botUserID:       "12345",

	}
	if bot.Username() != "12345" {
		t.Errorf("expected 12345, got %q", bot.Username())
	}
}
