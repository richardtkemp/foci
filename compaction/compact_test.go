package compaction

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clod/anthropic"
	"clod/memory"
	"clod/session"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent("hello world")},    // 11 chars / 4 = 2
		{Role: "assistant", Content: anthropic.TextContent("hi there!")}, // 9 chars / 4 = 2
	}

	tokens := estimateTokens(msgs)
	if tokens < 2 {
		t.Errorf("estimateTokens = %d, expected >= 2", tokens)
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	tokens := estimateTokens(nil)
	if tokens != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", tokens)
	}
}

func TestContextLimit(t *testing.T) {
	models := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-6", "unknown-model"}
	for _, model := range models {
		limit := contextLimit(model)
		if limit != 200_000 {
			t.Errorf("contextLimit(%q) = %d, want 200000", model, limit)
		}
	}
}

func TestContextLimitExported(t *testing.T) {
	models := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-6", "unknown-model"}
	for _, model := range models {
		limit := ContextLimit(model)
		if limit != 200_000 {
			t.Errorf("ContextLimit(%q) = %d, want 200000", model, limit)
		}
	}
}

func TestShouldCompactWithUsage(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Under threshold (160k = 200k * 0.8)
	usage := &anthropic.Usage{InputTokens: 100_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at 100k tokens")
	}

	// Over threshold
	usage = &anthropic.Usage{InputTokens: 170_000}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact at 170k tokens")
	}

	// Cache tokens count toward total
	usage = &anthropic.Usage{
		InputTokens:          50_000,
		CacheReadInputTokens: 120_000,
	}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact when cache_read + input > threshold")
	}
}

func TestShouldCompactWithEstimate(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Small conversation — should not compact
	small := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent("hello")},
		{Role: "assistant", Content: anthropic.TextContent("hi")},
	}
	if c.ShouldCompact(small, nil) {
		t.Error("should not compact small conversation")
	}
}

func TestShouldCompactExactThreshold(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Exactly at threshold (200k * 0.8 = 160k)
	usage := &anthropic.Usage{InputTokens: 160_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at exact threshold (> not >=)")
	}

	// One over
	usage = &anthropic.Usage{InputTokens: 160_001}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact one above threshold")
	}
}

// mockCompactionServer returns a test API server for compaction tests.
func mockCompactionServer(summaryText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent(summaryText),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
}

func TestCompactBasic(t *testing.T) {
	server := mockCompactionServer("Summary of conversation: user said hello, we discussed Go testing.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("user message")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("assistant reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	err := c.Compact(context.Background(), sessionKey, nil, "", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// After compaction: should have 3 messages (marker + summary + handoff)
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3", len(msgs))
	}

	// First message: compaction marker
	if !strings.Contains(anthropic.TextOf(msgs[0].Content), "compacted") {
		t.Errorf("msgs[0] = %q, want compaction marker", anthropic.TextOf(msgs[0].Content))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	// Second message: summary from model
	if !strings.Contains(anthropic.TextOf(msgs[1].Content), "Summary") {
		t.Errorf("msgs[1] = %q, want summary", anthropic.TextOf(msgs[1].Content))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	// Third message: handoff
	if !strings.Contains(anthropic.TextOf(msgs[2].Content), "Compaction complete") {
		t.Errorf("msgs[2] = %q, want handoff", anthropic.TextOf(msgs[2].Content))
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}
}

func TestCompactTooFewMessages(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	// Add only 2 messages (below default minMessages=4)
	store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("hi")})
	store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("hello")})

	c := NewCompactor(nil, store, "claude-haiku-4-5", 0.8)
	err := c.Compact(context.Background(), sessionKey, nil, "", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Messages should be unchanged
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (unchanged)", len(msgs))
	}
}

func TestCompactWithScratchpad(t *testing.T) {
	server := mockCompactionServer("Summary: testing scratchpad.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

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
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("msg")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	c.Scratchpad = sp
	c.AgentID = "test"

	err = c.Compact(context.Background(), sessionKey, nil, "", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}

	// Handoff message should include scratchpad content
	handoff := anthropic.TextOf(msgs[2].Content)
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
	server := mockCompactionServer("Summary: empty scratchpad.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	// Create empty scratchpad
	dbPath := filepath.Join(t.TempDir(), "scratchpad.db")
	sp, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	defer sp.Close()

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("msg")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	c.Scratchpad = sp
	c.AgentID = "test"

	err = c.Compact(context.Background(), sessionKey, nil, "", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	handoff := anthropic.TextOf(msgs[2].Content)
	// Should have default handoff without scratchpad section
	if strings.Contains(handoff, "scratchpad") {
		t.Errorf("handoff should not mention scratchpad when empty: %q", handoff)
	}
}

func TestCompactAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("msg")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	err := c.Compact(context.Background(), sessionKey, nil, "", "")
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

func TestWithConfigOverrides(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)
	c.WithConfig(2048, 8)

	// Model stays as initialized (always uses agent's model)
	if c.model != "claude-haiku-4-5" {
		t.Errorf("model = %q", c.model)
	}
	if c.maxTokens != 2048 {
		t.Errorf("maxTokens = %d", c.maxTokens)
	}
	if c.minMessages != 8 {
		t.Errorf("minMessages = %d", c.minMessages)
	}
}

func TestWithConfigEmptyValues(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)
	original := *c
	c.WithConfig(0, 0)

	// Zero values should not override
	if c.maxTokens != original.maxTokens {
		t.Errorf("maxTokens changed to %d", c.maxTokens)
	}
	if c.minMessages != original.minMessages {
		t.Errorf("minMessages changed to %d", c.minMessages)
	}
}

func TestCompactCustomPrompts(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("msg")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	err := c.Compact(context.Background(), sessionKey, nil, "custom summary prompt", "custom handoff msg")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify the custom summary prompt was sent to the API
	if !strings.Contains(string(capturedBody), "custom summary prompt") {
		t.Errorf("API request body should contain custom summary prompt")
	}

	// Verify custom handoff message in resulting messages
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	handoff := anthropic.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, "custom handoff msg") {
		t.Errorf("handoff = %q, want custom handoff msg", handoff)
	}
}

func TestCompactDefaultPrompts(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "agent:test:main"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, anthropic.Message{Role: "user", Content: anthropic.TextContent("msg")})
		store.Append(sessionKey, anthropic.Message{Role: "assistant", Content: anthropic.TextContent("reply")})
	}

	c := NewCompactor(client, store, "claude-haiku-4-5", 0.8)
	// Empty strings should fall back to defaults
	err := c.Compact(context.Background(), sessionKey, nil, "", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify a fallback summary prompt was sent
	if !strings.Contains(string(capturedBody), "concise summary") {
		t.Errorf("API request body should contain fallback summary prompt")
	}

	// Verify default handoff message
	msgs, _ := store.Load(sessionKey)
	handoff := anthropic.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, DefaultHandoffMessage) {
		t.Errorf("handoff = %q, want default handoff", handoff)
	}
}
