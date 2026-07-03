package discord

import (
	"fmt"
	"net/http"
	"testing"

	"foci/internal/chatmeta"
	"foci/internal/log"

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

// mockIndex implements platform.SessionIndex for testing stale channel cleanup.
type mockIndex struct {
	defaultChatID int64
}

func (m *mockIndex) GetChatMetadata(string, string, int64, string) (string, error) { return "", nil }
func (m *mockIndex) SetChatMetadata(string, string, int64, string, string) error   { return nil }
func (m *mockIndex) SetAgentMetadata(string, string, string) error                 { return nil }
func (m *mockIndex) SetDefaultChat(_ string, _ string, chatID int64) error {
	m.defaultChatID = chatID
	return nil
}
func (m *mockIndex) DefaultChatForAgent(string, string) int64 {
	return m.defaultChatID
}
func (m *mockIndex) ClearDefaultChat(string, string) error {
	m.defaultChatID = 0
	return nil
}
func (m *mockIndex) SetArchivedChat(string, string, int64, bool) error   { return nil }
func (m *mockIndex) ArchivedChatsForAgent(string, string) map[int64]bool { return nil }

// TestClearStaleChannel verifies that clearStaleChannel removes the channel
// from the session index default and last-known channel state.
func TestClearStaleChannel(t *testing.T) {
	idx := &mockIndex{defaultChatID: 12345}
	bot := &Bot{
		agentID:      "test",
		channelID:    12345,
		sessionIndex: idx,
		chatmeta: &chatmeta.Resolver{
			Index:        idx,
			AgentID:      "test",
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return defaultLogger },
		},
	}

	bot.clearStaleChannel("12345")

	// Default channel should be cleared from session index.
	if idx.defaultChatID != 0 {
		t.Error("expected default chat to be cleared")
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
	idx := &mockIndex{defaultChatID: 99999}
	bot := &Bot{
		agentID:      "test",
		channelID:    99999,
		sessionIndex: idx,
		chatmeta: &chatmeta.Resolver{
			Index:        idx,
			AgentID:      "test",
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return defaultLogger },
		},
	}

	bot.clearStaleChannel("12345")

	// Default channel should NOT be cleared (it's a different channel).
	if idx.defaultChatID != 99999 {
		t.Error("expected default chat to remain unchanged")
	}

	// Last known channel should NOT be cleared (it's a different channel).
	bot.channelMu.Lock()
	if bot.channelID != 99999 {
		t.Errorf("expected channelID to remain 99999, got %d", bot.channelID)
	}
	bot.channelMu.Unlock()
}
