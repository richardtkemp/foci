package discord

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestIsUnknownChannel verifies that isUnknownChannel correctly identifies
// Discord API error code 10003 and rejects other error types.
func TestIsUnknownChannel(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unknown channel error",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: 404},
				Message:  &discordgo.APIErrorMessage{Code: 10003, Message: "Unknown Channel"},
			},
			want: true,
		},
		{
			name: "different discord error code",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: 403},
				Message:  &discordgo.APIErrorMessage{Code: 50001, Message: "Missing Access"},
			},
			want: false,
		},
		{
			name: "rest error without message",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: 500},
			},
			want: false,
		},
		{
			name: "plain error",
			err:  fmt.Errorf("connection refused"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnknownChannel(tt.err); got != tt.want {
				t.Errorf("isUnknownChannel() = %v, want %v", got, tt.want)
			}
		})
	}
}

// mockIndex implements sessionIndexInterface for testing stale channel cleanup.
type mockIndex struct {
	agent map[string]string
}

func (m *mockIndex) GetChatMetadata(string, int64, string) (string, error) { return "", nil }
func (m *mockIndex) SetChatMetadata(string, int64, string, string) error   { return nil }
func (m *mockIndex) GetAgentMetadata(_, key string) (string, error) {
	return m.agent[key], nil
}
func (m *mockIndex) SetAgentMetadata(_, key, val string) error {
	m.agent[key] = val
	return nil
}
func (m *mockIndex) DeleteAgentMetadata(_, key string) error {
	delete(m.agent, key)
	return nil
}

// TestClearStaleChannel verifies that clearStaleChannel removes the channel
// from the in-memory cache, session index, and last-known channel state.
func TestClearStaleChannel(t *testing.T) {
	idx := &mockIndex{agent: map[string]string{"default_channel": "12345"}}
	bot := &Bot{
		agentID:         "test",
		chatSessionKeys: map[int64]string{12345: "test/c12345/999"},
		channelID:       12345,
		sessionIndex:    idx,
	}

	bot.clearStaleChannel("12345")

	// Chat session key cache should be cleared.
	bot.chatKeysMu.RLock()
	if _, ok := bot.chatSessionKeys[12345]; ok {
		t.Error("expected chat session key to be removed")
	}
	bot.chatKeysMu.RUnlock()

	// Default channel should be cleared from session index.
	if _, ok := idx.agent["default_channel"]; ok {
		t.Error("expected default_channel to be deleted from session index")
	}

	// Last known channel should be cleared.
	bot.channelMu.Lock()
	if bot.channelID != 0 {
		t.Errorf("expected channelID to be 0, got %d", bot.channelID)
	}
	bot.channelMu.Unlock()
}

// TestClearStaleChannelNonDefault verifies that clearStaleChannel only clears
// the default channel if it matches the stale channel, not unconditionally.
func TestClearStaleChannelNonDefault(t *testing.T) {
	idx := &mockIndex{agent: map[string]string{"default_channel": "99999"}}
	bot := &Bot{
		agentID:         "test",
		chatSessionKeys: map[int64]string{12345: "test/c12345/999"},
		channelID:       99999,
		sessionIndex:    idx,
	}

	bot.clearStaleChannel("12345")

	// Chat session key cache should be cleared for the stale channel.
	bot.chatKeysMu.RLock()
	if _, ok := bot.chatSessionKeys[12345]; ok {
		t.Error("expected stale chat session key to be removed")
	}
	bot.chatKeysMu.RUnlock()

	// Default channel should NOT be cleared (it's a different channel).
	if idx.agent["default_channel"] != "99999" {
		t.Error("expected default_channel to remain unchanged")
	}

	// Last known channel should NOT be cleared (it's a different channel).
	bot.channelMu.Lock()
	if bot.channelID != 99999 {
		t.Errorf("expected channelID to remain 99999, got %d", bot.channelID)
	}
	bot.channelMu.Unlock()
}
