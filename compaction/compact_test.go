package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"foci/anthropic"
	"foci/memory"
	"foci/provider"
	"foci/session"
)

// nonStreamingClient wraps a provider.Client so it does not satisfy
// provider.StreamingClient. This ensures tests exercise the SendMessage
// fallback path rather than attempting (and failing) to stream via the
// SDK-only transport.
type nonStreamingClient struct{ provider.Client }

func noStream(c provider.Client) provider.Client { return nonStreamingClient{c} }

func TestEstimateTokens(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello world")},    // 11 chars / 4 = 2
		{Role: "assistant", Content: provider.TextContent("hi there!")}, // 9 chars / 4 = 2
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
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 200_000},
		{"claude-sonnet-4-5", 200_000},
		{"claude-opus-4-6", 200_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"unknown-model", 200_000},
	}
	for _, tt := range tests {
		if got := contextLimit(tt.model); got != tt.want {
			t.Errorf("contextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

func TestContextLimitExported(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 200_000},
		{"gemini-2.5-flash", 1_000_000},
		{"unknown-model", 200_000},
	}
	for _, tt := range tests {
		if got := ContextLimit(tt.model); got != tt.want {
			t.Errorf("ContextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

func TestShouldCompactWithUsage(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Under threshold (160k = 200k * 0.8)
	usage := &provider.Usage{InputTokens: 100_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at 100k tokens")
	}

	// Over threshold
	usage = &provider.Usage{InputTokens: 170_000}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact at 170k tokens")
	}

	// Cache tokens count toward total
	usage = &provider.Usage{
		InputTokens:          50_000,
		CacheReadInputTokens: 120_000,
	}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact when cache_read + input > threshold")
	}
}

func TestShouldCompactWithEstimate(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Small conversation — should not compact
	small := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi")},
	}
	if c.ShouldCompact(small, nil) {
		t.Error("should not compact small conversation")
	}
}

func TestShouldCompactExactThreshold(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Exactly at threshold (200k * 0.8 = 160k)
	usage := &provider.Usage{InputTokens: 160_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at exact threshold (> not >=)")
	}

	// One over
	usage = &provider.Usage{InputTokens: 160_001}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact one above threshold")
	}
}

// mockCompactionServer returns a test API server for compaction tests.
func mockCompactionServer(summaryText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent(summaryText),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
}

func TestCompactBasic(t *testing.T) {
	server := mockCompactionServer("Summary of conversation: user said hello, we discussed Go testing.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
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

func TestCompactDryRun(t *testing.T) {
	server := mockCompactionServer("Dry-run summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 6 messages (above default minMessages=4)
	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
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

func TestCompactTooFewMessages(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add only 2 messages (below default minMessages=4)
	store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hello")})

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
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
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
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
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
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
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

func TestWithConfigOverrides(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)
	c.WithConfig(2048, 8, 10)

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
	if c.preserveMessages != 10 {
		t.Errorf("preserveMessages = %d", c.preserveMessages)
	}
}

func TestWithConfigEmptyValues(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)
	original := *c
	c.WithConfig(0, 0, 0)

	// Zero values should not override maxTokens/minMessages but preserveMessages=0 is valid
	if c.maxTokens != original.maxTokens {
		t.Errorf("maxTokens changed to %d", c.maxTokens)
	}
	if c.minMessages != original.minMessages {
		t.Errorf("minMessages changed to %d", c.minMessages)
	}
	if c.preserveMessages != 0 {
		t.Errorf("preserveMessages = %d, want 0", c.preserveMessages)
	}
}

func TestCompactCustomPrompts(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "custom summary prompt", "custom handoff msg", false)
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
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, "custom handoff msg") {
		t.Errorf("handoff = %q, want custom handoff msg", handoff)
	}
}

func TestCompactDefaultPrompts(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	// Empty strings should fall back to defaults
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify a fallback summary prompt was sent
	if !strings.Contains(string(capturedBody), "provide continuity") {
		t.Errorf("API request body should contain fallback summary prompt")
	}

	// Verify default handoff message
	msgs, _ := store.Load(sessionKey)
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, DefaultHandoffMessage) {
		t.Errorf("handoff = %q, want default handoff", handoff)
	}
}

func TestCompactPreserveMessages(t *testing.T) {
	server := mockCompactionServer("Summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 10 messages (5 user + 5 assistant)
	for i := 0; i < 5; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("user msg %d", i))})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("assistant reply %d", i))})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 4) // preserve last 4 messages

	summary, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}

	// After compaction: preserved[0] is user, so handoff folds into summary.
	// 2 (marker + summary+handoff) + 4 preserved = 6
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 6 {
		t.Fatalf("after compact: %d messages, want 6", len(msgs))
	}

	// Summary+handoff (folded) should contain the preservation note
	summaryText := provider.TextOf(msgs[1].Content)
	if !strings.Contains(summaryText, "last 4 messages") {
		t.Errorf("summary missing preservation note: %q", summaryText)
	}
	// Handoff text should be folded into the summary
	if !strings.Contains(summaryText, "Compaction complete") {
		t.Errorf("summary should contain folded handoff: %q", summaryText)
	}

	// 10 messages: [u0,a0,u1,a1,u2,a2,u3,a3,u4,a4]
	// Preserve last 4: [u3,a3,u4,a4]
	// preserved[0]=user → handoff folded into summary
	// Result: [marker, summary+handoff, u3, a3, u4, a4]
	expected := []struct {
		role string
		text string
	}{
		{"user", "user msg 3"},
		{"assistant", "assistant reply 3"},
		{"user", "user msg 4"},
		{"assistant", "assistant reply 4"},
	}
	for i, exp := range expected {
		idx := 2 + i // preserved starts at index 2 (handoff folded)
		if msgs[idx].Role != exp.role {
			t.Errorf("preserved[%d].Role = %q, want %q", i, msgs[idx].Role, exp.role)
		}
		if provider.TextOf(msgs[idx].Content) != exp.text {
			t.Errorf("preserved[%d] = %q, want %q", i, provider.TextOf(msgs[idx].Content), exp.text)
		}
	}

	// Verify role alternation: every pair of consecutive messages has different roles
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
		}
	}
}

func TestCompactPreserveMessagesZero(t *testing.T) {
	server := mockCompactionServer("Summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 0) // preserve=0 → same as current behaviour

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3 (no preserved)", len(msgs))
	}

	// Summary should NOT contain preservation note
	summaryText := provider.TextOf(msgs[1].Content)
	if strings.Contains(summaryText, "last") {
		t.Errorf("summary should not have preservation note when preserve=0: %q", summaryText)
	}
}

func TestCompactPreserveMoreThanAvailable(t *testing.T) {
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// 10 messages, minMessages=4, preserve=100
	for i := 0; i < 5; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 100) // preserve=100 but only 10 messages

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	// Should clamp: 10 messages, need at least 4 to summarize, so preserve = 6
	// preserved[0] is user → handoff folded into summary
	// Result: 2 (marker + summary+handoff) + 6 preserved = 8
	if len(msgs) != 8 {
		t.Fatalf("after compact: %d messages, want 8 (clamped preserve)", len(msgs))
	}
}

func TestCompactPreserveRoleAlternation(t *testing.T) {
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")

	t.Run("preserved_starts_user", func(t *testing.T) {
		// Even preserve count from even total → preserved[0] is user
		store := session.NewStore(t.TempDir())
		key := "test/imain/1000000000"
		for i := 0; i < 5; i++ {
			store.Append(key, provider.Message{Role: "user", Content: provider.TextContent("u")})
			store.Append(key, provider.Message{Role: "assistant", Content: provider.TextContent("a")})
		}

		c := NewCompactor(store, "claude-haiku-4-5", 0.8)
		c.WithConfig(4096, 4, 4) // preserve 4 → [u3,a3,u4,a4] → starts user

		if _, err := c.Compact(context.Background(), noStream(client), key, nil, "", "", false); err != nil {
			t.Fatalf("Compact: %v", err)
		}

		msgs, _ := store.Load(key)
		// Handoff folded: 2 header + 4 preserved = 6
		if len(msgs) != 6 {
			t.Fatalf("got %d messages, want 6", len(msgs))
		}
		// Verify no consecutive same-role
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive %s at [%d,%d]", msgs[i].Role, i-1, i)
			}
		}
		// Handoff text should be folded into the assistant summary
		if !strings.Contains(provider.TextOf(msgs[1].Content), "Compaction complete") {
			t.Errorf("summary should contain folded handoff")
		}
	})

	t.Run("preserved_starts_assistant", func(t *testing.T) {
		// Odd preserve count from even total → preserved[0] is assistant
		store := session.NewStore(t.TempDir())
		key := "test/imain/1000000000"
		for i := 0; i < 5; i++ {
			store.Append(key, provider.Message{Role: "user", Content: provider.TextContent("u")})
			store.Append(key, provider.Message{Role: "assistant", Content: provider.TextContent("a")})
		}

		c := NewCompactor(store, "claude-haiku-4-5", 0.8)
		c.WithConfig(4096, 4, 3) // preserve 3 → [a3,u4,a4] → starts assistant

		if _, err := c.Compact(context.Background(), noStream(client), key, nil, "", "", false); err != nil {
			t.Fatalf("Compact: %v", err)
		}

		msgs, _ := store.Load(key)
		// Standard layout: 3 header + 3 preserved = 6
		if len(msgs) != 6 {
			t.Fatalf("got %d messages, want 6", len(msgs))
		}
		// Verify no consecutive same-role
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive %s at [%d,%d]", msgs[i].Role, i-1, i)
			}
		}
		// Handoff should be separate user message (not folded)
		if msgs[2].Role != "user" {
			t.Errorf("handoff role = %q, want user", msgs[2].Role)
		}
	})
}

// --- tool_use / tool_result repair tests ---

// toolUseMsg builds an assistant message with one or more tool_use blocks.
func toolUseMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "test_tool",
			Input: json.RawMessage(`{}`),
		})
	}
	return provider.Message{Role: "assistant", Content: blocks}
}

// toolResultMsg builds a user message with tool_result blocks matching the given IDs.
func toolResultMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ToolResultBlock(id, "ok", false))
	}
	return provider.Message{Role: "user", Content: blocks}
}

func TestHasToolUse(t *testing.T) {
	if hasToolUse(provider.Message{Role: "user", Content: provider.TextContent("hi")}) {
		t.Error("plain user message should not have tool_use")
	}
	if !hasToolUse(toolUseMsg("toolu_1")) {
		t.Error("tool_use message should be detected")
	}
}

func TestToolUseIDs(t *testing.T) {
	ids := toolUseIDs(toolUseMsg("toolu_A", "toolu_B"))
	if len(ids) != 2 || ids[0] != "toolu_A" || ids[1] != "toolu_B" {
		t.Errorf("toolUseIDs = %v, want [toolu_A, toolu_B]", ids)
	}
}

func TestToolResultIDs(t *testing.T) {
	ids := toolResultIDs(toolResultMsg("toolu_X", "toolu_Y"))
	if !ids["toolu_X"] || !ids["toolu_Y"] || len(ids) != 2 {
		t.Errorf("toolResultIDs = %v, want {toolu_X, toolu_Y}", ids)
	}
}

func TestSafeSplitPointNoToolUse(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		{Role: "assistant", Content: provider.TextContent("a0")},
		{Role: "user", Content: provider.TextContent("u1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	// Split at 2 — no tool_use, should stay at 2.
	got := safeSplitPoint(msgs, 2, 25)
	if got != 2 {
		t.Errorf("safeSplitPoint = %d, want 2", got)
	}
}

func TestSafeSplitPointBreaksPair(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},      // 0
		{Role: "assistant", Content: provider.TextContent("a0")}, // 1
		{Role: "user", Content: provider.TextContent("u1")},      // 2
		toolUseMsg("toolu_1"),    // 3: assistant tool_use
		toolResultMsg("toolu_1"), // 4: user tool_result
		{Role: "assistant", Content: provider.TextContent("done")}, // 5
	}
	// Split at 4 would separate tool_use (3) from tool_result (4).
	// Should walk back to 3.
	got := safeSplitPoint(msgs, 4, 25)
	if got != 3 {
		t.Errorf("safeSplitPoint = %d, want 3", got)
	}
}

func TestSafeSplitPointConsecutiveToolPairs(t *testing.T) {
	// In a corrupt session, two assistant tool_use messages in a row.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A"),                                       // 1: assistant tool_use (corrupt — no result follows)
		toolUseMsg("toolu_B"),                                       // 2: assistant tool_use
		toolResultMsg("toolu_B"),                                    // 3: user tool_result
		{Role: "assistant", Content: provider.TextContent("done")}, // 4
	}
	// Split at 3: prev is toolUseMsg("toolu_B") → walk to 2.
	// Split at 2: prev is toolUseMsg("toolu_A") → walk to 1.
	// Split at 1: prev is user text → stop.
	got := safeSplitPoint(msgs, 3, 25)
	if got != 1 {
		t.Errorf("safeSplitPoint = %d, want 1", got)
	}
}

func TestSafeSplitPointBounded(t *testing.T) {
	// Walk-back bounded by maxWalkBack.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A"),                                       // 1
		toolUseMsg("toolu_B"),                                       // 2
		toolUseMsg("toolu_C"),                                       // 3
		toolResultMsg("toolu_C"),                                    // 4
		{Role: "assistant", Content: provider.TextContent("done")}, // 5
	}
	// Split at 4, maxWalkBack=2 → walks to 3, then 2, stops (2 steps).
	got := safeSplitPoint(msgs, 4, 2)
	if got != 2 {
		t.Errorf("safeSplitPoint = %d, want 2", got)
	}
}

func TestSafeSplitPointAtZero(t *testing.T) {
	msgs := []provider.Message{
		toolUseMsg("toolu_1"),
		toolResultMsg("toolu_1"),
	}
	// Split at 0 — already at start, can't walk back.
	got := safeSplitPoint(msgs, 0, 25)
	if got != 0 {
		t.Errorf("safeSplitPoint = %d, want 0", got)
	}
}

func TestRepairOrphanedToolUseNoOrphans(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		toolResultMsg("toolu_1"),
		{Role: "assistant", Content: provider.TextContent("done")},
	}
	repaired := repairOrphanedToolUse(msgs)
	if len(repaired) != len(msgs) {
		t.Errorf("repaired has %d messages, want %d (no change)", len(repaired), len(msgs))
	}
}

func TestRepairOrphanedToolUseMissingResult(t *testing.T) {
	// Assistant has tool_use but no tool_result follows at all.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		// Missing: tool_result for toolu_1
		{Role: "user", Content: provider.TextContent("u1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject synthetic result into the existing user message at index 2.
	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}

	// The user message after tool_use should now have synthetic + original content.
	userMsg := repaired[2]
	if userMsg.Role != "user" {
		t.Fatalf("repaired[2].Role = %q, want user", userMsg.Role)
	}
	// Should have 2 blocks: synthetic tool_result + original text.
	if len(userMsg.Content) != 2 {
		t.Fatalf("repaired[2] has %d blocks, want 2", len(userMsg.Content))
	}
	if userMsg.Content[0].Type != "tool_result" || userMsg.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("repaired[2].Content[0] = %+v, want tool_result for toolu_1", userMsg.Content[0])
	}
	if !userMsg.Content[0].IsError {
		t.Error("synthetic tool_result should be is_error=true")
	}
}

func TestRepairOrphanedToolUseNoNextMessage(t *testing.T) {
	// Tool_use is the last message — no following message at all.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject a standalone user message.
	if len(repaired) != 3 {
		t.Fatalf("repaired has %d messages, want 3", len(repaired))
	}
	if repaired[2].Role != "user" {
		t.Errorf("repaired[2].Role = %q, want user", repaired[2].Role)
	}
	if len(repaired[2].Content) != 1 || repaired[2].Content[0].Type != "tool_result" {
		t.Errorf("repaired[2] should be a single tool_result block")
	}
}

func TestRepairOrphanedToolUsePartialMatch(t *testing.T) {
	// Assistant has 2 tool_use blocks, but only 1 has a result.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A", "toolu_B"),
		toolResultMsg("toolu_A"), // only A matched
		{Role: "assistant", Content: provider.TextContent("done")},
	}
	repaired := repairOrphanedToolUse(msgs)

	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}

	// User message should now have synthetic result for B + original result for A.
	userMsg := repaired[2]
	if len(userMsg.Content) != 2 {
		t.Fatalf("repaired[2] has %d blocks, want 2", len(userMsg.Content))
	}
	// Synthetic comes first (prepended).
	if userMsg.Content[0].ToolUseID != "toolu_B" || !userMsg.Content[0].IsError {
		t.Errorf("synthetic block = %+v, want tool_result for toolu_B with is_error", userMsg.Content[0])
	}
	if userMsg.Content[1].ToolUseID != "toolu_A" {
		t.Errorf("original block = %+v, want tool_result for toolu_A", userMsg.Content[1])
	}
}

func TestRepairOrphanedToolUseNextIsAssistant(t *testing.T) {
	// Corrupt: assistant tool_use followed by another assistant message.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject a standalone user message between the two assistant messages.
	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}
	if repaired[2].Role != "user" || repaired[2].Content[0].Type != "tool_result" {
		t.Errorf("repaired[2] should be injected tool_result user message")
	}
	if repaired[3].Role != "assistant" {
		t.Errorf("repaired[3] should be original assistant message")
	}
}

func TestCompactSplitBreaksToolUsePair(t *testing.T) {
	server := mockCompactionServer("Summary of tool conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Build session: 5 text pairs + 1 tool pair + 1 text pair = 14 messages
	for i := 0; i < 5; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("u%d", i))})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("a%d", i))})
	}
	store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("run tool")})
	store.Append(sessionKey, toolUseMsg("toolu_SPLIT"))
	store.Append(sessionKey, toolResultMsg("toolu_SPLIT"))
	store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("tool done")})

	// preserve=3 would split between tool_use[11] and tool_result[12].
	// safeSplitPoint should adjust to 11, making preserve=3.
	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 3)

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)

	// Verify no orphaned tool_use: every assistant with tool_use must be followed
	// by a user with matching tool_result.
	for i, msg := range msgs {
		if msg.Role != "assistant" {
			continue
		}
		ids := toolUseIDs(msg)
		if len(ids) == 0 {
			continue
		}
		if i+1 >= len(msgs) {
			t.Fatalf("assistant tool_use at end of compacted session (index %d)", i)
		}
		next := msgs[i+1]
		resultIDs := toolResultIDs(next)
		for _, id := range ids {
			if !resultIDs[id] {
				t.Errorf("orphaned tool_use %s at index %d — no matching tool_result", id, i)
			}
		}
	}

	// Verify role alternation.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
		}
	}
}

func TestCompactOrphanedToolUseInHistory(t *testing.T) {
	server := mockCompactionServer("Summary of corrupt session.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Build session with an orphaned tool_use deep in history.
	store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u0")})
	store.Append(sessionKey, toolUseMsg("toolu_ORPHAN"))
	// Missing tool_result — simulate data corruption.
	store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u1")})
	store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a1")})
	store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u2")})
	store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a2")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 0) // no preservation — all messages summarized

	// This should not fail — repairOrphanedToolUse should inject synthetic results.
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact with orphaned tool_use: %v", err)
	}
}

func TestCompactPreserveWithScratchpad(t *testing.T) {
	server := mockCompactionServer("Summary with scratchpad.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Create scratchpad
	dbPath := filepath.Join(t.TempDir(), "scratchpad.db")
	sp, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	defer sp.Close()
	sp.Write("test", "plan", "my plan")

	for i := 0; i < 5; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 3)
	c.Scratchpad = sp
	c.AgentID = "test"

	_, err = c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	// 3 (marker+summary+handoff) + 3 preserved = 6
	if len(msgs) != 6 {
		t.Fatalf("after compact: %d messages, want 6", len(msgs))
	}

	// Handoff should include scratchpad
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, "scratchpad") {
		t.Errorf("handoff missing scratchpad: %q", handoff)
	}
	if !strings.Contains(handoff, "plan") {
		t.Errorf("handoff missing scratchpad key: %q", handoff)
	}

	// Preserved messages should be present after handoff
	if msgs[3].Role != "assistant" || provider.TextOf(msgs[3].Content) != "reply" {
		t.Errorf("preserved[0] = role=%q text=%q", msgs[3].Role, provider.TextOf(msgs[3].Content))
	}
}

func TestCompactWithEffortOverride(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithEffort("high")
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	body := string(capturedBody)
	if !strings.Contains(body, `"effort":"high"`) {
		t.Errorf("API request body should contain effort=high, got: %s", body)
	}
	if !strings.Contains(body, `"output_config"`) {
		t.Errorf("API request body should contain output_config, got: %s", body)
	}
}

func TestCompactWithoutEffortOverride(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	// Not setting effort — should omit from request
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	body := string(capturedBody)
	if strings.Contains(body, `"effort"`) {
		t.Errorf("API request body should not contain effort when not set, got: %s", body)
	}
	if strings.Contains(body, `"output_config"`) {
		t.Errorf("API request body should not contain output_config when effort not set, got: %s", body)
	}
}

func TestWithEffort(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)
	if c.effort != "" {
		t.Errorf("initial effort = %q, want empty", c.effort)
	}

	c.WithEffort("high")
	if c.effort != "high" {
		t.Errorf("after WithEffort, effort = %q, want high", c.effort)
	}

	c.WithEffort("")
	if c.effort != "" {
		t.Errorf("after clearing, effort = %q, want empty", c.effort)
	}
}

// mockStreamingCompactionServer returns an SSE-streaming test server for compaction.
func mockStreamingCompactionServer(summaryText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flushing not supported", http.StatusInternalServerError)
			return
		}

		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_compact_stream","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			fmt.Sprintf(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s"}}`, summaryText),
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
}

func TestCompactStreaming(t *testing.T) {
	server := mockStreamingCompactionServer("Streamed summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	client.SetUseSDK(true)

	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.Append(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("user message")})
		store.Append(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("assistant reply")})
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
