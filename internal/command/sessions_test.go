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
	// Verifies that session index displays correctly with filtering.
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		all := []SessionIndexInfo{
			{SessionKey: "bot/c123/1000", CreatedAt: now, SessionType: "chat", Status: "active"},
			{SessionKey: "bot/ispawn-456/1000", CreatedAt: now.Add(-time.Hour), ParentSessionKey: "bot/c123/1000", SessionType: "spawn", Status: "active"},
			{SessionKey: "bot/ibg-789/1000", CreatedAt: now.Add(-2 * time.Hour), SessionType: "cron", Status: "compacted"},
		}
		var filtered []SessionIndexInfo
		for _, e := range all {
			if opts.TypeFilter != "" && e.SessionType != opts.TypeFilter {
				continue
			}
			if opts.StatusFilter != "" && e.Status != opts.StatusFilter {
				continue
			}
			filtered = append(filtered, e)
		}
		return filtered, nil
	}
	cmd := NewSessionsCommand(deps)

	// Default (active only)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "2 sessions") {
		t.Errorf("expected 2 active sessions, got %q", result)
	}
	if !strings.Contains(result, "bot/c123") {
		t.Errorf("expected chat session in output, got %q", result)
	}

	// All entries
	result, err = cmd.Execute(context.Background(), "index all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "3 sessions") {
		t.Errorf("expected 3 sessions with 'all', got %q", result)
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
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
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
	deps.IndexFn = func(SessionIndexOpts) ([]SessionIndexInfo, error) { return nil, nil }
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
	// Verifies that session keys are abbreviated for table display:
	// keeps agent + truncated typeID, drops versionTS, truncates children.
	tests := []struct {
		input, want string
	}{
		{"scout/c5970082313/1772794601", "scout/c597…"},                              // long chat ID truncated
		{"mybot/i1709596800/1709596800", "mybot/i170…"},                              // independent truncated
		{"bot/c123/1000/b1772795000", "bot/c123/b177…"},                              // branch child truncated
		{"bot/c123/1000", "bot/c123"},                                                 // short ID, no truncation
		{"raw-key", "raw-key"},                                                        // no slash, returned as-is
	}
	for _, tt := range tests {
		got := shortenSessionKey(tt.input)
		if got != tt.want {
			t.Errorf("shortenSessionKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSessionsIndexSortedByLastActive(t *testing.T) {
	// Verifies that index results are sorted by last activity, most recent first.
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		// Return in chronological order (oldest first) — command should reverse.
		return []SessionIndexInfo{
			{SessionKey: "bot/cold/1000", LastActivityAt: now.Add(-3 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/cmid/1000", LastActivityAt: now.Add(-1 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/cnew/1000", LastActivityAt: now.Add(-5 * time.Minute), SessionType: "chat", Status: "active"},
		}, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}

	// "cnew" should appear before "cmid" which should appear before "cold"
	newIdx := strings.Index(result, "bot/cnew")
	midIdx := strings.Index(result, "bot/cmid")
	oldIdx := strings.Index(result, "bot/cold")
	if newIdx == -1 || midIdx == -1 || oldIdx == -1 {
		t.Fatalf("expected all sessions in output, got %q", result)
	}
	if newIdx > midIdx || midIdx > oldIdx {
		t.Errorf("expected newest first: new@%d mid@%d old@%d", newIdx, midIdx, oldIdx)
	}
}

func TestSessionsIndexSortFallsBackToCreatedAt(t *testing.T) {
	// Verifies that entries with zero LastActivityAt sort by CreatedAt instead.
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		return []SessionIndexInfo{
			{SessionKey: "bot/icreated-old/1000", CreatedAt: now.Add(-2 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/iactive-new/1000", LastActivityAt: now.Add(-10 * time.Minute), SessionType: "chat", Status: "active"},
		}, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}

	newIdx := strings.Index(result, "bot/iact…")
	oldIdx := strings.Index(result, "bot/icre…")
	if newIdx == -1 || oldIdx == -1 {
		t.Fatalf("expected both sessions in output, got %q", result)
	}
	if newIdx > oldIdx {
		t.Errorf("expected active-new before created-old, new@%d old@%d", newIdx, oldIdx)
	}
}

func TestSessionsIndexMaxCount(t *testing.T) {
	// Verifies that a count argument limits the number of displayed sessions.
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		return []SessionIndexInfo{
			{SessionKey: "bot/c1/1000", LastActivityAt: now, SessionType: "chat", Status: "active"},
			{SessionKey: "bot/c2/1000", LastActivityAt: now.Add(-1 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/c3/1000", LastActivityAt: now.Add(-2 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/c4/1000", LastActivityAt: now.Add(-3 * time.Hour), SessionType: "chat", Status: "active"},
			{SessionKey: "bot/c5/1000", LastActivityAt: now.Add(-4 * time.Hour), SessionType: "chat", Status: "active"},
		}, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index 2")
	if err != nil {
		t.Fatal(err)
	}
	// Should show "2 of 5 sessions"
	if !strings.Contains(result, "2 of 5 sessions") {
		t.Errorf("expected '2 of 5 sessions' in output, got %q", result)
	}
	// Should contain the 2 most recent, not the older ones
	if !strings.Contains(result, "bot/c1") {
		t.Errorf("expected most recent session bot/c1, got %q", result)
	}
	if !strings.Contains(result, "bot/c2") {
		t.Errorf("expected second most recent session bot/c2, got %q", result)
	}
	if strings.Contains(result, "bot/c3") {
		t.Errorf("did not expect bot/c3 in limited output, got %q", result)
	}
}

func TestSessionsIndexMaxCountLargerThanResults(t *testing.T) {
	// Verifies that a count larger than the result set shows all results without "of N".
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		return []SessionIndexInfo{
			{SessionKey: "bot/c1/1000", LastActivityAt: now, SessionType: "chat", Status: "active"},
		}, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index 10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "1 sessions") {
		t.Errorf("expected '1 sessions' without 'of' qualifier, got %q", result)
	}
	if strings.Contains(result, "of") {
		t.Errorf("did not expect 'of' when count >= results, got %q", result)
	}
}

func TestSessionsIndexRelativeTime(t *testing.T) {
	// Verifies that the Active column uses relative time format (e.g. "3h ago").
	now := time.Now().UTC()
	deps := testSessionsDeps(nil, 0)
	deps.IndexFn = func(opts SessionIndexOpts) ([]SessionIndexInfo, error) {
		return []SessionIndexInfo{
			{SessionKey: "bot/c1/1000", LastActivityAt: now.Add(-3 * time.Hour), SessionType: "chat", Status: "active"},
		}, nil
	}
	cmd := NewSessionsCommand(deps)
	result, err := cmd.Execute(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "3h ago") {
		t.Errorf("expected relative time '3h ago' in output, got %q", result)
	}
	// Header should be "Active" not "Last Active"
	if !strings.Contains(result, "Active") {
		t.Errorf("expected 'Active' column header, got %q", result)
	}
}

func TestParseIndexArgsCount(t *testing.T) {
	// Verifies that parseIndexArgs recognizes a plain number as MaxCount.
	opts := parseIndexArgs([]string{"5"})
	if opts.MaxCount != 5 {
		t.Errorf("expected MaxCount=5, got %d", opts.MaxCount)
	}

	// Combined with other filters
	opts = parseIndexArgs([]string{"chat", "all", "3", "2d"})
	if opts.MaxCount != 3 {
		t.Errorf("expected MaxCount=3, got %d", opts.MaxCount)
	}
	if opts.TypeFilter != "chat" {
		t.Errorf("expected TypeFilter=chat, got %q", opts.TypeFilter)
	}
	if opts.StatusFilter != "" {
		t.Errorf("expected StatusFilter empty (all), got %q", opts.StatusFilter)
	}
	if opts.MaxAge != 48*time.Hour {
		t.Errorf("expected MaxAge=48h, got %v", opts.MaxAge)
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
