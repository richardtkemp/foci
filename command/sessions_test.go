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
	result, err := cmd.Execute(context.Background(), "list")
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

func TestSessionsNoArgsShowsUsage(t *testing.T) {
	cmd := NewSessionsCommand(testSessionsDeps(nil, 0))

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage, got %q", result)
	}
	if !strings.Contains(result, "list") {
		t.Error("expected usage to mention 'list' subcommand")
	}
	if !strings.Contains(result, "default") {
		t.Error("expected usage to mention 'default' subcommand")
	}
	if !strings.Contains(result, "info") {
		t.Error("expected usage to mention 'info' subcommand")
	}
}

func TestSessionsIndexWithResults(t *testing.T) {
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(sessionType, status string) ([]SessionIndexInfo, error) {
		all := []SessionIndexInfo{
			{SessionKey: "agent:bot:chat:123", CreatedAt: now, SessionType: "chat", Status: "active"},
			{SessionKey: "agent:bot:spawn:spawn-456", CreatedAt: now.Add(-time.Hour), ParentSessionKey: "agent:bot:chat:123", SessionType: "spawn", Status: "active"},
			{SessionKey: "agent:bot:cron:bg-789", CreatedAt: now.Add(-2 * time.Hour), SessionType: "cron", Status: "compacted"},
		}
		var filtered []SessionIndexInfo
		for _, e := range all {
			if sessionType != "" && e.SessionType != sessionType {
				continue
			}
			if status != "" && e.Status != status {
				continue
			}
			filtered = append(filtered, e)
		}
		return filtered, nil
	}
	cmd := NewSessionsCommand(deps)

	// All entries
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "3 sessions") {
		t.Errorf("expected 3 sessions, got %q", result)
	}
	if !strings.Contains(result, "bot/chat:123") {
		t.Errorf("expected chat session in output, got %q", result)
	}
	if !strings.Contains(result, "spawn") {
		t.Errorf("expected spawn type in output, got %q", result)
	}

	// Filter by type
	result, err = cmd.Execute(context.Background(), "index chat")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "1 sessions") {
		t.Errorf("expected 1 session filtered by type, got %q", result)
	}

	// Filter by type and status
	result, err = cmd.Execute(context.Background(), "index cron compacted")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "1 sessions") {
		t.Errorf("expected 1 session filtered by type+status, got %q", result)
	}
}

func TestSessionsIndexEmpty(t *testing.T) {
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(sessionType, status string) ([]SessionIndexInfo, error) {
		return nil, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No sessions found") {
		t.Errorf("expected no sessions message, got %q", result)
	}
}

func TestSessionsIndexNotAvailable(t *testing.T) {
	deps := testSessionsDeps(nil, 0) // IndexFn is nil
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "not available") {
		t.Errorf("expected not available message, got %q", result)
	}
}

func TestSessionsKeyboardIncludesIndex(t *testing.T) {
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(string, string) ([]SessionIndexInfo, error) { return nil, nil }
	cmd := NewSessionsCommand(deps)
	opts := cmd.KeyboardOptions(context.Background())
	found := false
	for _, o := range opts {
		if o.Data == "index" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'index' in keyboard options when IndexFn is set")
	}
}

func TestSessionsKeyboardExcludesIndexWhenNil(t *testing.T) {
	deps := testSessionsDeps(nil, 0) // IndexFn is nil
	cmd := NewSessionsCommand(deps)
	opts := cmd.KeyboardOptions(context.Background())
	for _, o := range opts {
		if o.Data == "index" {
			t.Error("did not expect 'index' in keyboard when IndexFn is nil")
		}
	}
}

func TestSessionsListCurrentMarker(t *testing.T) {
	now := time.Now().UTC()
	sessions := []SessionChatInfo{
		{ChatID: 111, Username: "alice", MessageCount: 5, LastActivity: now},
		{ChatID: 222, Username: "bob", MessageCount: 3, LastActivity: now},
	}
	cmd := NewSessionsCommand(testSessionsDeps(sessions, 222)) // 222 is default
	ctx := context.WithValue(context.Background(), ChatIDKey{}, int64(111))
	result, err := cmd.Execute(ctx, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "◉") {
		t.Errorf("expected current marker ◉ in output, got %q", result)
	}
	if !strings.Contains(result, "★") {
		t.Errorf("expected default marker ★ in output, got %q", result)
	}
	if !strings.Contains(result, "◉ = current") {
		t.Errorf("expected legend for ◉, got %q", result)
	}
}

func TestShortenSessionKey(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"agent:mybot:chat:5970082313", "mybot/chat:59700823…"},
		{"agent:mybot:branch:abc123-def456", "mybot/branch:abc123-d…"},
		{"agent:bot:cron:bg-789", "bot/cron:bg-789"}, // short ID, no truncation
		{"agent:bot:chat:123", "bot/chat:123"},         // short ID, no truncation
		{"raw-key", "raw-key"},                          // no agent: prefix
	}
	for _, tt := range tests {
		got := shortenSessionKey(tt.input)
		if got != tt.want {
			t.Errorf("shortenSessionKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
