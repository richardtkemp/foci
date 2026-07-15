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
)

// sessionsTestCC builds a CommandContext for sessions tests using a real
// session.Store backed by a temp directory, and a session.SessionIndex.
func sessionsTestCC(t *testing.T, agentID string) (CommandContext, *session.Store, *session.SessionIndex) {
	t.Helper()
	dir := t.TempDir()
	store := session.NewStore(dir)
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return CommandContext{
		Sessions:     store,
		SessionIndex: idx,
		AgentConfig:  config.AgentConfig{ID: agentID},
	}, store, idx
}

// addChatSession writes messages for an agent/chat into the session store
// so that ListChatSessions picks it up. Returns the session key.
func addChatSession(t *testing.T, store *session.Store, agentID string, chatID int64, msgCount int) string {
	t.Helper()
	key := fmt.Sprintf("%s/c%d", agentID, chatID)
	msgs := make([]provider.Message, msgCount)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: provider.TextContent("msg")}
	}
	if err := store.TestAppendAll(key, msgs); err != nil {
		t.Fatalf("TestAppendAll: %v", err)
	}
	return key
}

// indexChatRoot registers an active chat root in the session index so
// index-based subcommands (list, info, default) see it.
func indexChatRoot(t *testing.T, ss *session.SessionIndex, agentID string, chatID int64) {
	t.Helper()
	ss.Upsert(session.SessionIndexEntry{
		SessionKey:  session.NewChatSessionKey(agentID, chatID),
		CreatedAt:   time.Now().UTC(),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
}

// setUsername stores a username in the session index for a given agent+chat.
func setUsername(t *testing.T, ss *session.SessionIndex, agentID string, chatID int64, username string) {
	t.Helper()
	if err := ss.SetChatMetadata(agentID, "", chatID, "username", username); err != nil {
		t.Fatalf("set username: %v", err)
	}
}

// setDefaultChat stores the default chat ID in the session index.
func setDefaultChat(t *testing.T, ss *session.SessionIndex, agentID string, chatID int64) {
	t.Helper()
	if err := ss.SetDefaultChat(agentID, "", chatID); err != nil {
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

// TestSessionsListEmpty verifies that executing the "list" subcommand when no
// chat sessions exist in the store returns a "No chat sessions" message rather
// than an empty table or error.
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

// TestSessionsListWithSessions verifies that the "list" subcommand renders a
// table containing each chat's ID, @username, message count, and the default
// marker (★) next to the chat that has been set as the default. Two sessions
// with distinct usernames and message counts are created, one marked as default,
// and the output is checked for all expected fields.
func TestSessionsListWithSessions(t *testing.T) {
	// Verifies that /sessions list shows chat IDs, usernames, message counts and default marker.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 123456789, 42)
	addChatSession(t, store, "test-agent", 987654321, 10)
	indexChatRoot(t, ss, "test-agent", 123456789)
	indexChatRoot(t, ss, "test-agent", 987654321)
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

// TestSessionsDefaultValid verifies that "default <chatID>" successfully
// updates the per-platform default chat (is_default) when the target chat
// session exists, and returns a confirmation message containing the new default chat ID.
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

	// Verify the session index was updated
	defaultChat := ss.DefaultChatForAgent("test-agent", "")
	if defaultChat != 987654321 {
		t.Errorf("expected default chat=987654321, got %d", defaultChat)
	}
	if !strings.Contains(result.Text, "987654321") {
		t.Errorf("expected confirmation with chat ID, got %q", result.Text)
	}
}

// TestSessionsDefaultInvalid verifies that "default <chatID>" with a chat ID
// that has no corresponding session in the store returns a "No session found"
// message instead of silently updating the default.
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

// TestSessionsDefaultBadInput verifies that "default <non-numeric>" returns an
// "Invalid chat ID" error message when the argument cannot be parsed as an int64.
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

// TestSessionsDefaultNoArg verifies that "default" with no explicit chat ID
// uses the current chat (req.ChatID) as the default.
func TestSessionsDefaultNoArg(t *testing.T) {
	// Verifies that /sessions default with no argument sets the current chat as default.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 555, 3)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default", ChatID: 555}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "555") {
		t.Errorf("expected confirmation with chat ID 555, got %q", result.Text)
	}
	defaultChat := ss.DefaultChatForAgent("test-agent", "")
	if defaultChat != 555 {
		t.Errorf("expected default chat=555, got %d", defaultChat)
	}
}

// TestSessionsInfo verifies that "info" displays the current chat's details
// including its numeric chat ID, whether it is the default ("Default: yes"),
// total message count, and the associated @username from the state store.
func TestSessionsInfo(t *testing.T) {
	// Verifies that /sessions info shows the session_index row joined with all
	// session_metadata, including unset metadata keys rendered as null/false.
	cc, _, ss := sessionsTestCC(t, "test-agent")
	key := session.NewChatSessionKey("test-agent", 123456789)
	ss.Upsert(session.SessionIndexEntry{
		SessionKey:  key,
		CreatedAt:   time.Now().UTC(),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	if err := ss.SetSessionMetadata(key, "model", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", ChatID: 123456789}, cc)
	if err != nil {
		t.Fatal(err)
	}
	// Index row columns present.
	for _, want := range []string{key, "session_type", "chat", "status", "active"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("expected index field %q, got %q", want, result.Text)
		}
	}
	// A set metadata key shows its value.
	if !strings.Contains(result.Text, "meta:model") || !strings.Contains(result.Text, "claude-opus-4-8") {
		t.Errorf("expected set metadata, got %q", result.Text)
	}
	// An unset boolean key renders as false; an unset string key as null.
	if !strings.Contains(result.Text, "meta:no_compact") {
		t.Errorf("expected unset no_compact row, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "meta:effort") {
		t.Errorf("expected unset effort row, got %q", result.Text)
	}
}

// TestSessionsInfoBackendOnly verifies info handles a chat with no index row
// (the CC-backend / brand-new case) by still listing all metadata keys.
func TestSessionsInfoBackendOnly(t *testing.T) {
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", ChatID: 42}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "no row") {
		t.Errorf("expected backend-only note, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "meta:model") {
		t.Errorf("expected metadata keys listed, got %q", result.Text)
	}
}

// TestSessionsInfoNoChatID verifies that "info" returns a "Not in a chat
// context" message when the request has no ChatID (zero value), indicating
// the command was invoked outside of a specific chat conversation.
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

// TestSessionsInfoBySessionKey verifies "info" resolves the request's
// SessionKey directly (e.g. a facet/branch) rather than deriving it from ChatID.
func TestSessionsInfoBySessionKey(t *testing.T) {
	cc, _, ss := sessionsTestCC(t, "test-agent")
	key := "test-agent/c123/b1700000000"
	ss.Upsert(session.SessionIndexEntry{
		SessionKey:       key,
		CreatedAt:        time.Now().UTC(),
		ParentSessionKey: "test-agent/c123",
		SessionType:      session.SessionTypeFacet,
		Status:           session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "info", SessionKey: key, ChatID: 123}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, key) {
		t.Errorf("expected info for %q, got %q", key, result.Text)
	}
	if !strings.Contains(result.Text, "facet") {
		t.Errorf("expected facet type, got %q", result.Text)
	}
}

// TestSessionsUnknownSubcommand verifies that an unrecognized subcommand (e.g.
// "foo") falls back to displaying the usage message listing valid subcommands,
// rather than returning an error or doing nothing.
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

// TestSessionsNoArgsShowsUsage verifies that invoking /sessions with an empty
// argument string returns usage text that enumerates all available subcommands
// (list, default, info), so the user knows what actions are possible.
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

// TestSessionsIndexWithResults verifies the "index" subcommand against a
// populated SessionIndex containing chat, spawn, and cron entries with mixed
// statuses. It checks four filter modes: default (active only, expects 2),
// "all" (includes compacted, expects 3), type filter "chat" (expects 1), and
// combined type+status filter "cron compacted" (expects 1).
func TestSessionsIndexWithResults(t *testing.T) {
	// Verifies that session index displays correctly with filtering.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	// Populate index with entries of different types/statuses.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/c123",
		CreatedAt:   now,
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:       "bot/ispawn-456",
		CreatedAt:        now.Add(-time.Hour),
		ParentSessionKey: "bot/c123",
		SessionType:      session.SessionTypeSpawn,
		Status:           session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/ibg-789",
		CreatedAt:   now.Add(-2 * time.Hour),
		SessionType: session.SessionTypeKeepalive,
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
	if !strings.Contains(result.Text, "bot/ispawn-456") {
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
	result, err = cmd.Execute(context.Background(), Request{Args: "index keepalive compacted"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "1 sessions") {
		t.Errorf("expected 1 session filtered by type+status, got %q", result.Text)
	}
}

// TestSessionsIndexEmpty verifies that "index" returns a "No sessions found"
// message when the SessionIndex exists but contains zero entries.
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

// TestSessionsIndexNotAvailable verifies that "index" returns a "not available"
// message when the CommandContext has a nil SessionIndex, indicating the index
// feature has not been configured for this agent.
func TestSessionsIndexNotAvailable(t *testing.T) {
	// Verifies that /sessions index with no SessionIndex returns a "not available" message.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cc.SessionIndex = nil

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "not available") {
		t.Errorf("expected not available message, got %q", result.Text)
	}
}

// TestSessionsKeyboardIncludesIndex verifies that the keyboard options returned
// by SessionsCommand include an "index" button when the CommandContext has a
// non-nil SessionIndex, so Telegram users can access the index subcommand.
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

// TestSessionsKeyboardExcludesIndexWhenNil verifies that the keyboard options
// omit the "index" button when the SessionIndex is nil, preventing users from
// seeing a subcommand that would just return "not available".
func TestSessionsKeyboardExcludesIndexWhenNil(t *testing.T) {
	// Verifies that keyboard options exclude "index" when SessionIndex is nil.
	cc, _, _ := sessionsTestCC(t, "test-agent")
	cc.SessionIndex = nil

	cmd := SessionsCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	for _, o := range opts {
		if o.Data == "index" {
			t.Error("did not expect 'index' in keyboard when SessionIndex is nil")
		}
	}
}

// TestSessionsListCurrentMarker verifies that the "list" output distinguishes
// between the current chat session and the default chat session using separate
// markers: the filled circle (◉) for the chat from which the command was issued
// (ChatID 111), and the star (★) for the stored default (ChatID 222), along
// with a legend explaining the markers.
func TestSessionsListCurrentMarker(t *testing.T) {
	// Verifies that the current session is marked with ◉ and the default with ★.
	cc, store, ss := sessionsTestCC(t, "test-agent")
	addChatSession(t, store, "test-agent", 111, 5)
	addChatSession(t, store, "test-agent", 222, 3)
	indexChatRoot(t, ss, "test-agent", 111)
	indexChatRoot(t, ss, "test-agent", 222)
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

// TestSessionsListIndexOnly verifies list shows a CC-backend / app chat that
// exists only in the index (no <agent>/c<chatID>/root.jsonl on disk) — the case
// the old filesystem scan missed. Message count is "—" (transcript is CC's).
func TestSessionsListIndexOnly(t *testing.T) {
	cc, _, ss := sessionsTestCC(t, "test-agent")
	indexChatRoot(t, ss, "test-agent", 4117293257876803825)

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "4117293257876803825") {
		t.Errorf("expected index-only chat listed, got %q", result.Text)
	}
}

// TestSessionsIndexSortedByLastActive verifies that the "index" subcommand
// sorts results by LastActivityAt in descending order (most recent first). Three
// sessions are inserted in chronological order (oldest first) and the output is
// checked to confirm the newest session key appears before the middle one, which
// appears before the oldest.
func TestSessionsIndexSortedByLastActive(t *testing.T) {
	// Verifies that index results are sorted by last activity, most recent first.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	// Insert in chronological order (oldest first) — command should reverse.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cold",
		CreatedAt:      now.Add(-3 * time.Hour),
		LastActivityAt: now.Add(-3 * time.Hour),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cmid",
		CreatedAt:      now.Add(-1 * time.Hour),
		LastActivityAt: now.Add(-1 * time.Hour),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/cnew",
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

func TestSessionsIndexHybridSort(t *testing.T) {
	// Verifies the hybrid recency sort: a chat is ranked by last_user_activity_at
	// (human touch), NOT last_activity_at (any turn), while a non-chat session is
	// ranked by last_activity_at. chatA has the most recent ANY-activity but an
	// old human touch, so it must sink below both a recently human-touched chat
	// and a recently active non-chat.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	// chatA: any-activity now, but human last touched 2h ago → ranks by 2h.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "bot/cA", CreatedAt: now.Add(-3 * time.Hour), LastActivityAt: now,
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	idx.TouchUserActivity("bot/cA", now.Add(-2*time.Hour))
	// chatB: human touched 5m ago → ranks by 5m (newest).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "bot/cB", CreatedAt: now.Add(-3 * time.Hour), LastActivityAt: now.Add(-time.Hour),
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	idx.TouchUserActivity("bot/cB", now.Add(-5*time.Minute))
	// spawnC: non-chat, any-activity 30m ago, no human touch → ranks by 30m.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "bot/iC", CreatedAt: now.Add(-3 * time.Hour), LastActivityAt: now.Add(-30 * time.Minute),
		SessionType: session.SessionTypeUnknown, Status: session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	// Expected order by recency: cB (5m) < iC (30m) < cA (2h), newest first.
	bIdx := strings.Index(result.Text, "bot/cB")
	cIdx := strings.Index(result.Text, "bot/iC")
	aIdx := strings.Index(result.Text, "bot/cA")
	if bIdx == -1 || cIdx == -1 || aIdx == -1 {
		t.Fatalf("expected all sessions in output, got %q", result.Text)
	}
	if !(bIdx < cIdx && cIdx < aIdx) {
		t.Errorf("hybrid sort wrong: want cB<iC<cA, got cB@%d iC@%d cA@%d (chatA's recent any-activity must not float it up)", bIdx, cIdx, aIdx)
	}
}

// TestSessionsIndexSortFallsBackToCreatedAt verifies that when a session has a
// zero-value LastActivityAt, the sort uses CreatedAt as the fallback timestamp.
// A session with a recent LastActivityAt should appear before one that only has
// an older CreatedAt and no recorded activity.
func TestSessionsIndexSortFallsBackToCreatedAt(t *testing.T) {
	// Verifies that entries with zero LastActivityAt sort by CreatedAt instead.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "bot/icreated-old",
		CreatedAt:   now.Add(-2 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/iactive-new",
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

	newIdx := strings.Index(result.Text, "bot/iactive-new")
	oldIdx := strings.Index(result.Text, "bot/icreated-old")
	if newIdx == -1 || oldIdx == -1 {
		t.Fatalf("expected both sessions in output, got %q", result.Text)
	}
	if newIdx > oldIdx {
		t.Errorf("expected active-new before created-old, new@%d old@%d", newIdx, oldIdx)
	}
}

// TestSessionsIndexMaxCount verifies that passing a numeric count argument to
// "index" (e.g. "index 2") limits the displayed results to that many sessions,
// showing only the most recent ones and reporting "2 of 5 sessions" in the
// header. Sessions beyond the limit must not appear in the output.
func TestSessionsIndexMaxCount(t *testing.T) {
	// Verifies that a count argument limits the number of displayed sessions.
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	for i, key := range []string{"bot/c1", "bot/c2", "bot/c3", "bot/c4", "bot/c5"} {
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

// TestSessionsIndexMaxCountLargerThanResults verifies that when the count
// argument exceeds the number of matching sessions, all results are shown and
// the header says "1 sessions" without an "of N" qualifier, since no truncation
// occurred.
func TestSessionsIndexMaxCountLargerThanResults(t *testing.T) {
	// Verifies that a count larger than the result set shows all results without "of N".
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/c1",
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

// TestSessionsIndexRelativeTime verifies that the "Active" column in the index
// table renders timestamps as human-readable relative durations (e.g. "3h ago")
// rather than absolute timestamps, and that the column header is "Active".
func TestSessionsIndexRelativeTime(t *testing.T) {
	// Verifies that the Active column uses relative time format (e.g. "3h ago").
	now := time.Now().UTC()
	cc, _, _ := sessionsTestCC(t, "test-agent")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "bot/c1",
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

// TestParseIndexArgsCount verifies that parseIndexArgs correctly extracts a
// plain numeric argument as MaxCount, and that it can parse all argument types
// simultaneously (type filter, "all" status, numeric count, and duration-based
// MaxAge like "2d") without interference between them.
func TestParseIndexArgsCount(t *testing.T) {
	// Verifies that parseIndexArgs recognizes a plain number as MaxCount.
	opts := parseIndexArgs([]string{"5"}, "test-agent", "test-agent/c1", nil)
	if opts.MaxCount != 5 {
		t.Errorf("expected MaxCount=5, got %d", opts.MaxCount)
	}

	// Combined with other filters
	opts = parseIndexArgs([]string{"chat", "all", "3", "2d"}, "test-agent", "test-agent/c1", nil)
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

// TestSessionsListError verifies that the "default" subcommand propagates
// errors when the SessionIndex is nil. By setting cc.SessionIndex to nil after
// creating a valid session, the test triggers the error path in the session
// index persistence logic and confirms the error is returned to the caller.
func TestSessionsListError(t *testing.T) {
	// Verifies that errors from the session index are propagated.
	// Setting SessionIndex to nil triggers the error path in sessionsDefaultCmd.
	cc, store, ss := sessionsTestCC(t, "test")
	addChatSession(t, store, "test", 111, 1)
	setDefaultChat(t, ss, "test", 111)

	// Removing SessionIndex triggers error in sessionsDefaultCmd.
	cc.SessionIndex = nil
	cmd := SessionsCommand()
	_, err := cmd.Execute(context.Background(), Request{Args: "default 111"}, cc)
	if err == nil {
		t.Fatal("expected error when SessionIndex is nil for set default")
	}
}

// TestSessionsDefaultIndexOnly reproduces the reported bug: a CC-backend / app
// chat exists only in the session index (no <agent>/c<chatID>/root.jsonl on
// disk), and the old ListChatSessions-based check reported "No session found"
// even from the active session. The index-based check must accept it.
func TestSessionsDefaultIndexOnly(t *testing.T) {
	cc, _, ss := sessionsTestCC(t, "test-agent")
	ss.Upsert(session.SessionIndexEntry{
		SessionKey:  session.NewChatSessionKey("test-agent", 4117293257876803825),
		CreatedAt:   time.Now().UTC(),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default 4117293257876803825"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "No session found") {
		t.Errorf("index-only chat should be found, got %q", result.Text)
	}
	if ss.DefaultChatForAgent("test-agent", "") != 4117293257876803825 {
		t.Errorf("default not set, got %d", ss.DefaultChatForAgent("test-agent", ""))
	}
}

// TestSessionsDefaultRegisteredOnly verifies an app chat known only via a
// platform registration (chat_metadata) is accepted.
func TestSessionsDefaultRegisteredOnly(t *testing.T) {
	cc, _, ss := sessionsTestCC(t, "test-agent")
	if err := ss.SetChatMetadata("test-agent", "app", 777, "registered", "1"); err != nil {
		t.Fatal(err)
	}

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "default 777"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "No session found") {
		t.Errorf("registered chat should be found, got %q", result.Text)
	}
}

func TestParseIndexArgsScope(t *testing.T) {
	agents := map[string]bool{"scout": true}
	tests := []struct {
		name        string
		args        []string
		wantAgent   string
		wantRoot    string
		wantStatus  string
	}{
		{"family word", []string{"this"}, "", "clutch/c123", ""},
		{"self word", []string{"me"}, "clutch", "", "active"},
		{"known agent", []string{"scout"}, "scout", "", "active"},
		{"session key", []string{"arnix/c999/b1700000000"}, "", "arnix/c999", ""},
		{"none", []string{"active"}, "", "", "active"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := parseIndexArgs(tc.args, "clutch", "clutch/c123/b1700000000", agents)
			if opts.AgentID != tc.wantAgent {
				t.Errorf("AgentID: want %q got %q", tc.wantAgent, opts.AgentID)
			}
			if opts.RootKey != tc.wantRoot {
				t.Errorf("RootKey: want %q got %q", tc.wantRoot, opts.RootKey)
			}
			if opts.StatusFilter != tc.wantStatus {
				t.Errorf("StatusFilter: want %q got %q", tc.wantStatus, opts.StatusFilter)
			}
		})
	}
}

// TestSessionsIndexFamily verifies a family-scoped query returns the root and
// its branches (all statuses) but excludes unrelated sessions.
func TestSessionsIndexFamily(t *testing.T) {
	cc, _, _ := sessionsTestCC(t, "clutch")
	idx := newTestSessionIndex(t)
	cc.SessionIndex = idx
	now := time.Now().UTC()

	idx.Upsert(session.SessionIndexEntry{SessionKey: "clutch/c123", CreatedAt: now, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	idx.Upsert(session.SessionIndexEntry{SessionKey: "clutch/c123/b1700000001", CreatedAt: now, ParentSessionKey: "clutch/c123", SessionType: session.SessionTypeReflection, Status: session.SessionStatusCompacted})
	idx.Upsert(session.SessionIndexEntry{SessionKey: "clutch/c999", CreatedAt: now, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})

	cmd := SessionsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "index clutch/c123"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "clutch/c123/b1700000001") {
		t.Errorf("expected branch in family view, got %q", result.Text)
	}
	if strings.Contains(result.Text, "clutch/c999") {
		t.Errorf("unrelated session should be excluded, got %q", result.Text)
	}
}
