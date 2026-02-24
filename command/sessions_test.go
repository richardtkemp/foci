package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testSessionsDeps(sessions []SessionChatInfo, defaultChat int64) SessionsDeps {
	return SessionsDeps{
		AgentID: "test-agent",
		ListFn: func() ([]SessionChatInfo, error) {
			return sessions, nil
		},
		SetDefaultFn: func(chatID int64) error {
			return nil
		},
		DefaultChatFn: func() int64 {
			return defaultChat
		},
	}
}

func TestSessionsListEmpty(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))
	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No chat sessions") {
		t.Errorf("expected no sessions message, got %q", result)
	}
}

func TestSessionsListWithSessions(t *testing.T) {
	now := time.Now().UTC()
	sessions := []SessionChatInfo{
		{ChatID: 123456789, Username: "alice", MessageCount: 42, LastActivity: now},
		{ChatID: 987654321, Username: "bob", MessageCount: 10, LastActivity: now.Add(-time.Hour)},
	}
	cmd := NewSessionsCommand(testSessionsDeps(sessions, 123456789))
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result, "123456789") {
		t.Error("expected chat ID 123456789 in output")
	}
	if !strings.Contains(result, "@alice") {
		t.Error("expected @alice in output")
	}
	if !strings.Contains(result, "@bob") {
		t.Error("expected @bob in output")
	}
	if !strings.Contains(result, "★") {
		t.Error("expected default marker ★ in output")
	}
	if !strings.Contains(result, "42") {
		t.Error("expected message count 42 in output")
	}
}

func TestSessionsDefaultValid(t *testing.T) {
	sessions := []SessionChatInfo{
		{ChatID: 123456789},
		{ChatID: 987654321},
	}
	var setChatID int64
	deps := testSessionsDeps(sessions, 123456789)
	deps.SetDefaultFn = func(chatID int64) error {
		setChatID = chatID
		return nil
	}
	cmd := NewSessionsCommand(deps)

	result, err := cmd.Execute(context.Background(), "default 987654321")
	if err != nil {
		t.Fatal(err)
	}
	if setChatID != 987654321 {
		t.Errorf("expected set default to 987654321, got %d", setChatID)
	}
	if !strings.Contains(result, "987654321") {
		t.Errorf("expected confirmation with chat ID, got %q", result)
	}
}

func TestSessionsDefaultInvalid(t *testing.T) {
	sessions := []SessionChatInfo{
		{ChatID: 123456789},
	}
	cmd := NewSessionsCommand(testSessionsDeps(sessions, 123456789))

	result, err := cmd.Execute(context.Background(), "default 999")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No session found") {
		t.Errorf("expected not found message, got %q", result)
	}
}

func TestSessionsDefaultBadInput(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))

	result, err := cmd.Execute(context.Background(), "default abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Invalid chat ID") {
		t.Errorf("expected invalid ID message, got %q", result)
	}
}

func TestSessionsDefaultNoArg(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))

	result, err := cmd.Execute(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage message, got %q", result)
	}
}

func TestSessionsInfo(t *testing.T) {
	now := time.Now().UTC()
	sessions := []SessionChatInfo{
		{ChatID: 123456789, Username: "alice", MessageCount: 42, LastActivity: now},
	}
	cmd := NewSessionsCommand(testSessionsDeps(sessions, 123456789))

	ctx := context.WithValue(context.Background(), ChatIDKey{}, int64(123456789))
	result, err := cmd.Execute(ctx, "info")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Chat ID: 123456789") {
		t.Errorf("expected chat ID, got %q", result)
	}
	if !strings.Contains(result, "Default: yes") {
		t.Errorf("expected default yes, got %q", result)
	}
	if !strings.Contains(result, "Messages: 42") {
		t.Errorf("expected message count, got %q", result)
	}
	if !strings.Contains(result, "@alice") {
		t.Errorf("expected username, got %q", result)
	}
}

func TestSessionsInfoNoChatID(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))

	result, err := cmd.Execute(context.Background(), "info")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Not in a chat context") {
		t.Errorf("expected no context message, got %q", result)
	}
}

func TestSessionsInfoNonDefault(t *testing.T) {
	sessions := []SessionChatInfo{
		{ChatID: 123456789, MessageCount: 5},
	}
	cmd := NewSessionsCommand(testSessionsDeps(sessions, 999))

	ctx := context.WithValue(context.Background(), ChatIDKey{}, int64(123456789))
	result, err := cmd.Execute(ctx, "info")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Default: no") {
		t.Errorf("expected default no, got %q", result)
	}
}

func TestSessionsUnknownSubcommand(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))

	result, err := cmd.Execute(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage, got %q", result)
	}
}

func TestSessionsListError(t *testing.T) {
	deps := SessionsDeps{
		AgentID: "test",
		ListFn: func() ([]SessionChatInfo, error) {
			return nil, fmt.Errorf("disk error")
		},
		DefaultChatFn: func() int64 { return 0 },
	}
	cmd := NewSessionsCommand(deps)
	_, err := cmd.Execute(context.Background(), "list")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disk error") {
		t.Errorf("expected disk error, got %v", err)
	}
}
