package command

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/state"
)

// sessionsTestCC builds a CommandContext for sessions tests using a real
// session.Store backed by a temp directory, and optionally a state.Store
// and session.SessionIndex.
func sessionsTestCC(t *testing.T, agentID string) (CommandContext, *session.Store, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := session.NewStore(dir)
	ss := state.New(filepath.Join(dir, "state.json"))
	return CommandContext{
		Sessions:    store,
		StateStore:  ss,
		AgentConfig: config.AgentConfig{ID: agentID},
	}, store, ss
}

// addChatSession writes messages for an agent/chat into the session store
// so that ListChatSessions picks it up. Returns the session key.
func addChatSession(t *testing.T, store *session.Store, agentID string, chatID int64, msgCount int) string {
	t.Helper()
	key := fmt.Sprintf("%s/c%d/1000000000", agentID, chatID)
	msgs := make([]provider.Message, msgCount)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: provider.TextContent("msg")}
	}
	if err := store.TestAppendAll(key, msgs); err != nil {
		t.Fatalf("TestAppendAll: %v", err)
	}
	return key
}

// setUsername stores a username in the state store for a given agent+chat.
func setUsername(t *testing.T, ss *state.Store, agentID string, chatID int64, username string) {
	t.Helper()
	key := fmt.Sprintf("agent/%s/chat/%d/username", agentID, chatID)
	if err := ss.Set(key, username); err != nil {
		t.Fatalf("set username: %v", err)
	}
}

// setDefaultChat stores the default chat ID in the state store.
func setDefaultChat(t *testing.T, ss *state.Store, agentID string, chatID int64) {
	t.Helper()
	if err := ss.Set("agent/"+agentID+"/default_chat", chatID); err != nil {
		t.Fatalf("set default chat: %v", err)
	}
}

// newTestSessionIndex creates an in-memory SQLite session index backed by a temp file.
func newTestSessionIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestSessionsListEmpty(t *testing.T) {
	// Verifies that /sessions list with no chat sessions returns an appropriate message.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "No chat sessions") {
		t.Errorf("expected no sessions message, got %q", result.Text)
	}
}

func TestSessionsListWithSessions(t *testing.T) {
	// Verifies that /sessions list shows chat IDs, usernames, message counts and default marker.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 42)
	addChatSession(t, store, "test-agent", 987654321, 10)
	setUsername(t, ss, "test-agent", 123456789, "alice")
	setUsername(t, ss, "test-agent", 987654321, "bob")
	setDefaultChat(t, ss, "test-agent", 123456789)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.Text, "123456789") {
		t.Error("expected chat ID 123456789 in output")
	}
	if !strings.Contains(result.Text, "@alice") {
		t.Error("expected @alice in output")
	}
	if !strings.Contains(result.Text, "@bob") {
		t.Error("expected @bob in output")
	}
	if !strings.Contains(result.Text, "★") {
		t.Error("expected default marker ★ in output")
	}
	if !strings.Contains(result.Text, "42") {
		t.Error("expected message count 42 in output")
	}
}

func TestSessionsDefaultValid(t *testing.T) {
	// Verifies that /sessions default <chatID> sets the default when the session exists.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 1)
	addChatSession(t, store, "test-agent", 987654321, 1)
	setDefaultChat(t, ss, "test-agent", 123456789)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default 987654321"}, cc)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the state store was updated
	var got int64
	if !ss.Get("agent/test-agent/default_chat", &got) {
		t.Error("expected default_chat to be set in state store")
	}
	if got != 987654321 {
		t.Errorf("expected default_chat=987654321, got %d", got)
	}
	if !strings.Contains(result.Text, "987654321") {
		t.Errorf("expected confirmation with chat ID, got %q", result.Text)
	}
}

func TestSessionsDefaultInvalid(t *testing.T) {
	// Verifies that /sessions default with a non-existent chat ID returns a not-found message.
	cc, store, _ := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 1)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default 999"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "No session found") {
		t.Errorf("expected not found message, got %q", result.Text)
	}
}

func TestSessionsDefaultBadInput(t *testing.T) {
	// Verifies that /sessions default with a non-numeric argument returns an error message.
	cc, _, _ := sessionsTestCC(t, "test-agent")

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default abc"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Invalid chat ID") {
		t.Errorf("expected invalid ID message, got %q", result.Text)
	}
}

func TestSessionsDefaultNoArg(t *testing.T) {
	// Verifies that /sessions default with no argument returns usage.
	cc, _, _ := sessionsTestCC(t, "test-agent")

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage message, got %q", result.Text)
	}
}

func TestSessionsInfo(t *testing.T) {
	// Verifies that /sessions info shows chat ID, default status, message count, and username.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 42)
	setUsername(t, ss, "test-agent", 123456789, "alice")
	setDefaultChat(t, ss, "test-agent", 123456789)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", ChatID: 123456789}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Chat ID: 123456789") {
		t.Errorf("expected chat ID, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "Default: yes") {
		t.Errorf("expected default yes, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "Messages: 42") {
		t.Errorf("expected message count, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "@alice") {
		t.Errorf("expected username, got %q", result.Text)
	}
}

func TestSessionsInfoNoChatID(t *testing.T) {
	// Verifies that /sessions info without a chat context returns an appropriate message.
	cc, _, _ := sessionsTestCC(t, "test-agent")

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", ChatID: 0}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Not in a chat context") {
		t.Errorf("expected no context message, got %q", result.Text)
	}
}

func TestSessionsInfoNonDefault(t *testing.T) {
	// Verifies that /sessions info shows "Default: no" when the current chat is not the default.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 5)
	setDefaultChat(t, ss, "test-agent", 999)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", ChatID: 123456789}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Default: no") {
		t.Errorf("expected default no, got %q", result.Text)
	}
}

func TestSessionsUnknownSubcommand(t *testing.T) {
	// Verifies that an unknown subcommand falls back to usage.
	cc, _, _ := sessionsTestCC(t, "test-agent")

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "foo"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage, got %q", result.Text)
	}
}

func TestSessionsNoArgsShowsUsage(t *testing.T) {
	// Verifies that /sessions with no args returns usage listing subcommands.
	cc, _, _ := sessionsTestCC(t, "test-agent")

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "list") {
		t.Error("expected usage to mention 'list' subcommand")
	}
	if !strings.Contains(result.Text, "default") {
		t.Error("expected usage to mention 'default' subcommand")
	}
	if !strings.Contains(result.Text, "info") {
		t.Error("expected usage to mention 'info' subcommand")
	}
}

func TestSessionsIndexWithResults(t *testing.T) {
	// Verifies that session index displays correctly with filtering.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	// Populate index with entries of different types/statuses.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/c123/1000",
		CreatedAt:   now,
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:       "bot/ispawn-456/1000",
		CreatedAt:        now.Add(-time.Hour),
		ParentSessionKey: "bot/c123/1000",
		SessionType:      session.SessionTypeSpawn,
		Status:           session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/ibg-789/1000",
		CreatedAt:   now.Add(-2 * time.Hour),
		SessionType: session.SessionTypeCron,
		Status:      session.SessionStatusCompacted,
	})

	cmd := SessionsCommand()

	// Default (active only)
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "2 sessions") {
		t.Errorf("expected 2 active sessions, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "bot/c123") {
		t.Errorf("expected chat session in output, got %q", result.Text)
	}

	// All entries
	result, err = cmd.Execute(context.Background(), Request{Args: "index all"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "3 sessions") {
		t.Errorf("expected 3 sessions with 'all', got %q", result.Text)
	}
	if !strings.Contains(result.Text, "bot/ispa") {
		t.Errorf("expected spawn session in output, got %q", result.Text)
	}

	// Filter by type
	result, err = cmd.Execute(context.Background(), Request{Args: "index chat"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "1 sessions") {
		t.Errorf("expected 1 session filtered by type, got %q", result.Text)
	}

	// Filter by type and status
	result, err = cmd.Execute(context.Background(), Request{Args: "index cron compacted"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "1 sessions") {
		t.Errorf("expected 1 session filtered by type+status, got %q", result.Text)
	}
}

func TestSessionsIndexEmpty(t *testing.T) {
	// Verifies that an empty index returns an appropriate message.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cc.SessionIndex = newTestSessionIndex(t)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "No sessions found") {
		t.Errorf("expected no sessions message, got %q", result.Text)
	}
}

func TestSessionsIndexNotAvailable(t *testing.T) {
	// Verifies that /sessions index with no SessionIndex returns a "not available" message.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	// cc.SessionIndex is nil

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "not available") {
		t.Errorf("expected not available message, got %q", result.Text)
	}
}

func TestSessionsKeyboardIncludesIndex(t *testing.T) {
	// Verifies that keyboard options include "index" when SessionIndex is set.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cc.SessionIndex = newTestSessionIndex(t)

	cmd := SessionsCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	found := false
	for _, o := range opts {
		if o.Data == "index" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'index' in keyboard options when SessionIndex is set")
	}
}

func TestSessionsKeyboardExcludesIndexWhenNil(t *testing.T) {
	// Verifies that keyboard options exclude "index" when SessionIndex is nil.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	// cc.SessionIndex is nil

	cmd := SessionsCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	for _, o := range opts {
		if o.Data == "index" {
			t.Error("did not expect 'index' in keyboard when SessionIndex is nil")
		}
	}
}

func TestSessionsListCurrentMarker(t *testing.T) {
	// Verifies that the current session is marked with ◉ and the default with ★.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 111, 5)
	addChatSession(t, store, "test-agent", 222, 3)
	setDefaultChat(t, ss, "test-agent", 222) // 222 is default

	cmd := SessionsCommand()
	// Current chat is 111, default chat is 222.
	result, err := cmd.Execute(context.Background(), Request{Args: "list", ChatID: 111}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "◉") {
		t.Errorf("expected current marker ◉ in output, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "★") {
		t.Errorf("expected default marker ★ in output, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "◉ = current") {
		t.Errorf("expected legend for ◉, got %q", result.Text)
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
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	// Insert in chronological order (oldest first) — command should reverse.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cold/1000",
		CreatedAt:      now.Add(-3 * time.Hour),
		LastActivityAt: now.Add(-3 * time.Hour),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cmid/1000",
		CreatedAt:      now.Add(-1 * time.Hour),
		LastActivityAt: now.Add(-1 * time.Hour),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cnew/1000",
		CreatedAt:      now.Add(-5 * time.Minute),
		LastActivityAt: now.Add(-5 * time.Minute),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}

	// "cnew" should appear before "cmid" which should appear before "cold"
	newIdx := strings.Index(result.Text, "bot/cnew")
	midIdx := strings.Index(result.Text, "bot/cmid")
	oldIdx := strings.Index(result.Text, "bot/cold")
	if newIdx == -1 || midIdx == -1 || oldIdx == -1 {
		t.Fatalf("expected all sessions in output, got %q", result.Text)
	}
	if newIdx > midIdx || midIdx > oldIdx {
		t.Errorf("expected newest first: new@%d mid@%d old@%d", newIdx, midIdx, oldIdx)
	}
}

func TestSessionsIndexSortFallsBackToCreatedAt(t *testing.T) {
	// Verifies that entries with zero LastActivityAt sort by CreatedAt instead.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/icreated-old/1000",
		CreatedAt:   now.Add(-2 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/iactive-new/1000",
		CreatedAt:      now.Add(-2 * time.Hour),
		LastActivityAt: now.Add(-10 * time.Minute),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}

	newIdx := strings.Index(result.Text, "bot/iact…")
	oldIdx := strings.Index(result.Text, "bot/icre…")
	if newIdx == -1 || oldIdx == -1 {
		t.Fatalf("expected both sessions in output, got %q", result.Text)
	}
	if newIdx > oldIdx {
		t.Errorf("expected active-new before created-old, new@%d old@%d", newIdx, oldIdx)
	}
}

func TestSessionsIndexMaxCount(t *testing.T) {
	// Verifies that a count argument limits the number of displayed sessions.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	for i, key := range []string{"bot/c1/1000", "bot/c2/1000", "bot/c3/1000", "bot/c4/1000", "bot/c5/1000"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:     key,
			CreatedAt:      now.Add(-time.Duration(i) * time.Hour),
			LastActivityAt: now.Add(-time.Duration(i) * time.Hour),
			SessionType:    session.SessionTypeChat,
			Status:         session.SessionStatusActive,
		})
	}

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index 2"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	// Should show "2 of 5 sessions"
	if !strings.Contains(result.Text, "2 of 5 sessions") {
		t.Errorf("expected '2 of 5 sessions' in output, got %q", result.Text)
	}
	// Should contain the 2 most recent, not the older ones
	if !strings.Contains(result.Text, "bot/c1") {
		t.Errorf("expected most recent session bot/c1, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "bot/c2") {
		t.Errorf("expected second most recent session bot/c2, got %q", result.Text)
	}
	if strings.Contains(result.Text, "bot/c3") {
		t.Errorf("did not expect bot/c3 in limited output, got %q", result.Text)
	}
}

func TestSessionsIndexMaxCountLargerThanResults(t *testing.T) {
	// Verifies that a count larger than the result set shows all results without "of N".
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/c1/1000",
		CreatedAt:      now,
		LastActivityAt: now,
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index 10"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "1 sessions") {
		t.Errorf("expected '1 sessions' without 'of' qualifier, got %q", result.Text)
	}
	if strings.Contains(result.Text, "of") {
		t.Errorf("did not expect 'of' when count >= results, got %q", result.Text)
	}
}

func TestSessionsIndexRelativeTime(t *testing.T) {
	// Verifies that the Active column uses relative time format (e.g. "3h ago").
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/c1/1000",
		CreatedAt:      now.Add(-3 * time.Hour),
		LastActivityAt: now.Add(-3 * time.Hour),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "3h ago") {
		t.Errorf("expected relative time '3h ago' in output, got %q", result.Text)
	}
	// Header should be "Active" not "Last Active"
	if !strings.Contains(result.Text, "Active") {
		t.Errorf("expected 'Active' column header, got %q", result.Text)
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
	// Verifies that errors from the session store are propagated.
	// Use a store backed by a non-existent path to trigger list errors.
	// Actually, ListChatSessions returns nil for missing dirs, so we test
	// by using a broken state store write path for the default subcommand error.
	// Instead, we verify the error path via a bad default set on non-existing session.
	cc, store, ss := sessionsTestCC(t, "test")
	addChatSession(t, store, "test", 111, 1)
	setDefaultChat(t, ss, "test", 111)

	// Removing StateStore triggers error in sessionsDefaultCmd.
	cc.StateStore = nil
	cmd := SessionsCommand()
	_, err := cmd.Execute(context.Background(), Request{Args: "default 111"}, cc)
	if err == nil {
		t.Fatal("expected error when StateStore is nil for set default")
	}
}
