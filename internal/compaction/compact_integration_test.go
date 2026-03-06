package compaction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/anthropic"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/session"
)

// TestCompactBasic verifies basic compaction workflow.
func TestCompactBasic(t *testing.T) {
	server := mockCompactionServer("Summary of conversation: user said hello, we discussed Go testing.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}

	// After compaction: should have 3 messages (marker + summary + handoff)
	msgs, _ := store.Load(sessionKey)
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

// TestCompactDryRun verifies dry-run mode does not modify session.
func TestCompactDryRun(t *testing.T) {
	server := mockCompactionServer("Dry-run summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", true)
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

// TestCompactTooFewMessages verifies compaction is skipped when under minimum.
func TestCompactTooFewMessages(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add only 2 messages (below default minMessages=4)
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hello")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	summary, err := c.Compact(context.Background(), nil, sessionKey, nil, "", "", false)
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

// TestCompactWithScratchpad verifies scratchpad content is included in handoff.
func TestCompactWithScratchpad(t *testing.T) {
	server := mockCompactionServer("Summary: testing scratchpad.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
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

	_, err = c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
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

// TestCompactEmptyScratchpad verifies no scratchpad mention when empty.
func TestCompactEmptyScratchpad(t *testing.T) {
	server := mockCompactionServer("Summary: empty scratchpad.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
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

	_, err = c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	handoff := provider.TextOf(msgs[2].Content)
	// Should have default handoff without scratchpad section
	if strings.Contains(handoff, "scratchpad") {
		t.Errorf("handoff should not mention scratchpad when empty: %q", handoff)
	}
}

// TestCompactAPIError verifies session is unchanged on API error.
func TestCompactAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
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

// TestCompactStreaming verifies streaming compaction works.
func TestCompactStreaming(t *testing.T) {
	server := mockStreamingCompactionServer("Streamed summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	client.SetUseSDK(true)

	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
	}

	c := NewCompactor(store, "anthropic/claude-haiku-4-5", 0.8)
	summary, err := c.Compact(context.Background(), client, sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact (streaming): %v", err)
	}
	if !strings.Contains(summary, "Streamed summary") {
		t.Errorf("summary = %q, want to contain 'Streamed summary'", summary)
	}

	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("after streaming compact: %d messages, want 3", len(msgs))
	}
	if !strings.Contains(provider.TextOf(msgs[1].Content), "Streamed summary") {
		t.Errorf("msgs[1] = %q, want streamed summary", provider.TextOf(msgs[1].Content))
	}
}

// TestSetLogger verifies logger can be set.
func TestSetLogger(t *testing.T) {
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

// TestContextLimit_Claude verifies context limits for Claude models.
func TestContextLimit_Claude(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-3-opus", 200_000},
		{"claude-3.5-sonnet-20241022", 200_000},
		{"anthropic/claude-haiku-4-5", 200_000},
	}

	for _, tt := range tests {
		got := ContextLimit(tt.model)
		if got != tt.want {
			t.Errorf("ContextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestContextLimit_Gemini verifies context limits for Gemini models.
func TestContextLimit_Gemini(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"gemini-1.5-flash", 2_000_000},
	}

	for _, tt := range tests {
		got := ContextLimit(tt.model)
		if got != tt.want {
			t.Errorf("ContextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestContextLimit_Default verifies default context limit for unknown models.
func TestContextLimit_Default(t *testing.T) {
	tests := []string{"gpt-4", "gpt-4o", "unknown-model", ""}
	for _, model := range tests {
		got := ContextLimit(model)
		if got != 200_000 {
			t.Errorf("ContextLimit(%q) = %d, want 200_000 (default)", model, got)
		}
	}
}

// TestCompactLoadError verifies error when session can't be loaded.
func TestCompactLoadError(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionKey := "test/imain/1000000000"

	// Create a file where the session directory should be to cause I/O error.
	// SessionPath returns <dir>/test/imain/1000000000/root.jsonl
	// Making 1000000000 a file prevents opening root.jsonl inside it.
	os.MkdirAll(filepath.Join(dir, "test", "imain"), 0755)
	os.WriteFile(filepath.Join(dir, "test", "imain", "1000000000"), []byte("conflict"), 0644)

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, err := c.Compact(context.Background(), nil, sessionKey, nil, "", "", false)
	if err == nil {
		t.Fatal("expected error when session can't be loaded")
	}
	if !strings.Contains(err.Error(), "load session for compaction") {
		t.Errorf("error = %q, want 'load session for compaction'", err)
	}
}

// TestCompactPreserveNegativeClamped verifies clamping when preserve goes negative.
func TestCompactPreserveNegativeClamped(t *testing.T) {
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
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

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Should have compacted everything (no preservation since preserveN clamped to 0)
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3 (no preserved)", len(msgs))
	}
}

// TestCompactWalkBackBelowMinMessages verifies walk-back drops preservation.
func TestCompactWalkBackBelowMinMessages(t *testing.T) {
	server := mockCompactionServer("Summary after walk-back.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

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

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Walk-back should have pushed split below minMessages, so nothing is preserved.
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3 (walk-back dropped preservation)", len(msgs))
	}
}

// TestCompactScratchpadError verifies graceful handling of scratchpad errors.
func TestCompactScratchpadError(t *testing.T) {
	server := mockCompactionServer("Summary with scratchpad error.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
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
	_, err = c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact should succeed despite scratchpad error: %v", err)
	}
}

// TestCompactReplaceError verifies error when session Replace fails.
func TestCompactReplaceError(t *testing.T) {
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
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
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err == nil {
		t.Fatal("expected error when Replace fails")
	}
	if !strings.Contains(err.Error(), "replace session after compaction") {
		t.Errorf("error = %q, want 'replace session after compaction'", err)
	}
}
