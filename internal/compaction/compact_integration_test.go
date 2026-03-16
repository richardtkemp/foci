package compaction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/session"
)

func TestCompactBasic(t *testing.T) {
	// Verifies the end-to-end compaction workflow: a session with enough
	// messages is compacted into a new session key containing exactly a marker message, an
	// assistant summary from the mock API, and a user handoff message — in the correct roles
	// and with the expected text content.
	server := mockCompactionServer("Summary of conversation: user said hello, we discussed Go testing.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}
	if newKey == "" || newKey == sessionKey {
		t.Fatalf("expected rotated key, got %q", newKey)
	}

	// After compaction: should have 3 messages at the new key
	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3", len(msgs))
	}

	// First message: compaction marker
	if !strings.Contains(provider.TextOf(msgs[0].Content), "compacted") {
		t.Errorf("msgs[0] = %q, want compaction marker", provider.TextOf(msgs[0].Content))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	// Second message: summary from model
	if !strings.Contains(provider.TextOf(msgs[1].Content), "Summary") {
		t.Errorf("msgs[1] = %q, want summary", provider.TextOf(msgs[1].Content))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	// Third message: handoff
	if !strings.Contains(provider.TextOf(msgs[2].Content), "Compaction complete") {
		t.Errorf("msgs[2] = %q, want handoff", provider.TextOf(msgs[2].Content))
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}
}

func TestCompactDryRun(t *testing.T) {
	// Verifies that dry-run mode calls the API and returns a summary
	// but leaves the original session completely unmodified, proving the flag acts as a
	// true preview with no side effects on stored messages.
	server := mockCompactionServer("Dry-run summary of conversation.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, _, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", true)
	if err != nil {
		t.Fatalf("Compact dry-run: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary from dry-run")
	}
	// Summary comes from the mock server - we just verify it's non-empty above

	// Session messages should be UNCHANGED (no Replace)
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 6 {
		t.Fatalf("after dry-run: %d messages, want 6 (unchanged)", len(msgs))
	}

	// Verify original messages are still there
	if provider.TextOf(msgs[0].Content) != "user message" {
		t.Errorf("msgs[0] = %q, want original user message", provider.TextOf(msgs[0].Content))
	}
}

func TestCompactTooFewMessages(t *testing.T) {
	// Verifies that compaction is a no-op when the session has
	// fewer messages than minMessages: no API call is made, the session is unchanged, and
	// the returned summary is empty — proving the guard condition works correctly.
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add only 2 messages (below default minMessages=4)
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hello")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, _, err := c.Compact(context.Background(), nil, sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary != "" {
		t.Error("expected empty summary for too-few messages")
	}

	// Messages should be unchanged
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (unchanged)", len(msgs))
	}
}

func TestCompactWithScratchpad(t *testing.T) {
	// Verifies that when a scratchpad has entries, the compaction
	// handoff message includes a scratchpad section containing the stored keys and values,
	// so that important agent notes survive the session rotation.
	server := mockCompactionServer("Summary: testing scratchpad.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Create scratchpad with entries
	dbPath := filepath.Join(t.TempDir(), "scratchpad.db")
	sp, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	defer sp.Close()

	sp.Write("test", "plan", "Step 1: refactor\nStep 2: test")
	sp.Write("test", "notes", "important detail")

	// Add enough messages
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.Scratchpad = sp
	c.AgentID = "test"

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}

	// Handoff message should include scratchpad content
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, "scratchpad") {
		t.Errorf("handoff missing scratchpad mention: %q", handoff)
	}
	if !strings.Contains(handoff, "plan") {
		t.Errorf("handoff missing scratchpad key 'plan': %q", handoff)
	}
	if !strings.Contains(handoff, "notes") {
		t.Errorf("handoff missing scratchpad key 'notes': %q", handoff)
	}
}

func TestCompactEmptyScratchpad(t *testing.T) {
	// Verifies that when the scratchpad has no entries, the
	// handoff message does not include any scratchpad section, keeping the handoff clean
	// and avoiding misleading references to empty storage.
	server := mockCompactionServer("Summary: empty scratchpad.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Create empty scratchpad
	dbPath := filepath.Join(t.TempDir(), "scratchpad.db")
	sp, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	defer sp.Close()

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.Scratchpad = sp
	c.AgentID = "test"

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(newKey)
	handoff := provider.TextOf(msgs[2].Content)
	// Should have default handoff without scratchpad section
	if strings.Contains(handoff, "scratchpad") {
		t.Errorf("handoff should not mention scratchpad when empty: %q", handoff)
	}
}

func TestCompactAPIError(t *testing.T) {
	// Verifies that when the summarisation API returns an error, Compact
	// propagates a descriptive error and leaves the original session messages entirely
	// unchanged, ensuring atomicity — either compaction fully succeeds or nothing changes.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, _, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err == nil {
		t.Fatal("expected error from API failure")
	}
	if !strings.Contains(err.Error(), "summarize for compaction") {
		t.Errorf("error = %q", err.Error())
	}

	// Messages should be unchanged after error
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 6 {
		t.Fatalf("messages = %d, want 6 (unchanged after error)", len(msgs))
	}
}

func TestCompactStreaming(t *testing.T) {
	// Verifies that when the client supports SSE streaming, compaction
	// collects the streamed summary correctly and produces a valid compacted session,
	// proving the streaming path reaches the same end state as the non-streaming path.
	server := mockStreamingCompactionServer("Streamed summary of conversation.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	client.SetUseSDK(true)

	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "anthropic/claude-haiku-4-5", 0.8)
	summary, newKey, err := c.Compact(context.Background(), client, sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact (streaming): %v", err)
	}
	if !strings.Contains(summary, "Streamed summary") {
		t.Errorf("summary = %q, want to contain 'Streamed summary'", summary)
	}

	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("after streaming compact: %d messages, want 3", len(msgs))
	}
	if !strings.Contains(provider.TextOf(msgs[1].Content), "Streamed summary") {
		t.Errorf("msgs[1] = %q, want streamed summary", provider.TextOf(msgs[1].Content))
	}
}

func TestSetLogger(t *testing.T) {
	// Verifies that SetLogger replaces the compactor's internal logger with
	// the supplied one, confirming the logger is injectable for test isolation and
	// structured log routing at runtime.
	store := session.NewStore(t.TempDir())
	c := NewCompactor(store, "gpt-4o", 0.8)

	oldLog := c.log
	newLog := log.NewComponentLogger("test")

	c.SetLogger(newLog)

	if c.log != newLog {
		t.Error("SetLogger did not set the logger")
	}
	if c.log == oldLog {
		t.Error("SetLogger should have changed the logger")
	}
}

func TestCompactLoadError(t *testing.T) {
	// Verifies that Compact returns a descriptive error and does not
	// panic when the session cannot be loaded from disk, by placing a file where the session
	// directory is expected so that the store's open call fails with an I/O error.
	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionKey := "test/imain/1000000000"

	// Create a file where the session directory should be to cause I/O error.
	// SessionPath returns <dir>/test/imain/1000000000/root.jsonl
	// Making 1000000000 a file prevents opening root.jsonl inside it.
	os.MkdirAll(filepath.Join(dir, "test", "imain"), 0755)
	os.WriteFile(filepath.Join(dir, "test", "imain", "1000000000"), []byte("conflict"), 0644)

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, _, err := c.Compact(context.Background(), nil, sessionKey, nil, "", "", false)
	if err == nil {
		t.Fatal("expected error when session can't be loaded")
	}
	if !strings.Contains(err.Error(), "load session for compaction") {
		t.Errorf("error = %q, want 'load session for compaction'", err)
	}
}

func TestCompactPreserveNegativeClamped(t *testing.T) {
	// Verifies that when the preserve count would leave
	// fewer messages than minMessages for summarisation, it is clamped to zero rather than
	// going negative, resulting in a standard no-preservation compact output.
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add exactly 4 messages (equals minMessages)
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u0")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a0")})
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u1")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a1")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	// preserveMessages=3 with only 4 messages and minMessages=4:
	// len(messages)-preserveN = 4-3 = 1 < minMessages(4), so preserveN = 4-4 = 0
	c.WithConfig(4096, 4, 3)

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Should have compacted everything (no preservation since preserveN clamped to 0)
	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3 (no preserved)", len(msgs))
	}
}

func TestCompactWalkBackBelowMinMessages(t *testing.T) {
	// Verifies that when safeSplitPoint walk-back would push the split below
	// minMessages, the original (pre-walk-back) split is kept instead of dropping
	// all preservation. The orphaned tool_use at the boundary is repaired by
	// repairOrphanedToolUse. This prevents the "preserve-all → preserve-nothing"
	// cliff that caused the double-compaction bug.
	server := mockCompactionServer("Summary after walk-back.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// 8 messages: [u0, a0, u1, tool_use_A, tool_result_A, tool_use_B, tool_result_B, done]
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u0")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a0")})
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u1")})
	store.TestAppend(sessionKey, toolUseMsg("toolu_A"))
	store.TestAppend(sessionKey, toolResultMsg("toolu_A"))
	store.TestAppend(sessionKey, toolUseMsg("toolu_B"))
	store.TestAppend(sessionKey, toolResultMsg("toolu_B"))
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("done")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 6, 3) // minMessages=6, preserve=3

	// With preserve=3, minMessages=6: preserveN clamped to 2 (8-6), splitIdx=6.
	// Walk-back: msgs[5]=tool_use_B → walks to 5, but 5 < minMessages=6 →
	// reverts to original splitIdx=6. preserveN stays at 2.
	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// New behavior: keeps original split, preserves 2 messages (tool_result_B, done).
	// preserved[0]=user (tool_result) → handoff folded: 2 header + 2 preserved = 4.
	msgs, _ := store.Load(newKey)
	if len(msgs) != 4 {
		t.Fatalf("after compact: %d messages, want 4 (walk-back reverted, preserved 2)", len(msgs))
	}

	// Last message should be preserved "done"
	if provider.TextOf(msgs[len(msgs)-1].Content) != "done" {
		t.Errorf("last message = %q, want 'done'", provider.TextOf(msgs[len(msgs)-1].Content))
	}
}

func TestCompactScratchpadError(t *testing.T) {
	// Verifies that a scratchpad failure (here: a closed database)
	// is treated as best-effort and does not abort the compaction — the overall Compact call
	// succeeds even when scratchpad content cannot be read.
	server := mockCompactionServer("Summary with scratchpad error.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	// Create a scratchpad backed by a closed database to trigger All() error.
	dbPath := filepath.Join(t.TempDir(), "scratchpad.db")
	sp, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	sp.Close() // close before use → All() will error

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.Scratchpad = sp
	c.AgentID = "test"

	// Should succeed despite scratchpad error (it's best-effort).
	_, _, err = c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact should succeed despite scratchpad error: %v", err)
	}
}

func TestCompactReplaceError(t *testing.T) {
	// Verifies that when the session store cannot write the compacted
	// result (here: the session directory is made read-only), Compact returns a descriptive
	// error wrapping "replace session after compaction" rather than silently succeeding with
	// a corrupt state.
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	// Make the session directory read-only so Replace can't write.
	sessDir := filepath.Join(dir, "test", "imain", "1000000000")
	os.Chmod(sessDir, 0555)
	t.Cleanup(func() { os.Chmod(sessDir, 0755) })

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, _, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err == nil {
		t.Fatal("expected error when Replace fails")
	}
	if !strings.Contains(err.Error(), "replace session after compaction") {
		t.Errorf("error = %q, want 'replace session after compaction'", err)
	}
}
