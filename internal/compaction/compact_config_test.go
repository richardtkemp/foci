package compaction

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

func TestWithConfigOverrides(t *testing.T) {
	// Verifies that WithConfig applies maxTokens, minMessages, and
	// preserveMessages correctly, and that the model is never changed by WithConfig since it
	// must always match the agent's configured model.
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
	// Verifies that passing zero values to WithConfig does not
	// overwrite maxTokens or minMessages (zero is not a valid value), but preserveMessages=0
	// is a valid setting that should be applied as-is.
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

func TestWithEffort(t *testing.T) {
	// Verifies that WithEffort stores the given effort string and that
	// clearing it with an empty string returns to the no-effort default state.
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

func TestCompactCustomPrompts(t *testing.T) {
	// Verifies that caller-supplied summary and handoff prompts
	// are sent to the API and appear in the resulting session, proving the customisation
	// path overrides the built-in defaults end-to-end.
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

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "custom summary prompt", "custom handoff msg", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify the custom summary prompt was sent to the API
	if !strings.Contains(string(capturedBody), "custom summary prompt") {
		t.Errorf("API request body should contain custom summary prompt")
	}

	// Verify custom handoff message in resulting messages
	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, "custom handoff msg") {
		t.Errorf("handoff = %q, want custom handoff msg", handoff)
	}
}

func TestCompactDefaultPrompts(t *testing.T) {
	// Verifies that passing empty strings for the summary and
	// handoff prompts falls back to the built-in defaults, by inspecting the captured API
	// request body for the fallback summary phrase and comparing the handoff text against
	// DefaultHandoffMessage.
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

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	// Empty strings should fall back to defaults
	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify a fallback summary prompt was sent
	if !strings.Contains(string(capturedBody), "provide continuity") {
		t.Errorf("API request body should contain fallback summary prompt")
	}

	// Verify default handoff message
	msgs, _ := store.Load(newKey)
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, DefaultHandoffMessage) {
		t.Errorf("handoff = %q, want default handoff", handoff)
	}
}

func TestCheckConfig_Safe(t *testing.T) {
	// Verifies that checkConfig does not panic or error when the
	// sum of the compaction trigger point and maxTokens fits within the model's context
	// window, confirming a well-configured compactor is accepted silently.
	store := session.NewStore(t.TempDir())
	// With Claude (200k context) and 80% threshold, 160k is trigger point
	// maxTokens=5000, so 160k + 5k < 200k → should not warn
	c := NewCompactor(store, "claude-3-opus", 0.8)
	c.maxTokens = 5000

	// Should not warn - no assertion needed, just verify it doesn't panic
	c.checkConfig()
}

func TestCheckConfig_Unsafe(t *testing.T) {
	// Verifies that checkConfig handles the case where maxTokens
	// is large enough that trigger point + maxTokens would exceed the context window,
	// and does so without panicking (it logs a warning rather than returning an error).
	store := session.NewStore(t.TempDir())
	// With Claude (200k context) and 80% threshold, 160k is trigger point
	// maxTokens=50000, so 160k + 50k > 200k → should warn
	c := NewCompactor(store, "claude-3-opus", 0.8)
	c.maxTokens = 50000

	// Capture any warnings (they go to the logger)
	// Just verify it doesn't panic when config is unsafe
	c.checkConfig()
}
