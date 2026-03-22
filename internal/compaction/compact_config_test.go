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
	// preserveMessages correctly.
	c := NewCompactor(nil, 0.8)
	c.WithConfig(2048, 8, 10)

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
	c := NewCompactor(nil, 0.8)
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

func TestModelParamsFn(t *testing.T) {
	// Verifies that ModelParamsFn is nil by default and can be set
	// to provide per-model API params.
	c := NewCompactor(nil, 0.8)
	if c.ModelParamsFn != nil {
		t.Error("initial ModelParamsFn should be nil")
	}

	c.ModelParamsFn = func(model string) (string, string, string) {
		if model == "anthropic/claude-opus-4-6" {
			return "adaptive", "high", ""
		}
		return "", "", ""
	}
	thinking, effort, speed := c.ModelParamsFn("anthropic/claude-opus-4-6")
	if thinking != "adaptive" || effort != "high" || speed != "" {
		t.Errorf("ModelParamsFn(opus) = (%q, %q, %q), want (adaptive, high, \"\")", thinking, effort, speed)
	}
	thinking, effort, speed = c.ModelParamsFn("anthropic/claude-haiku-4-5")
	if thinking != "" || effort != "" || speed != "" {
		t.Errorf("ModelParamsFn(haiku) = (%q, %q, %q), want all empty", thinking, effort, speed)
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

	c := NewCompactor(store, 0.8)
	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "custom summary prompt", "custom handoff msg", false)
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

	c := NewCompactor(store, 0.8)
	// Empty strings should fall back to defaults
	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Verify a fallback summary prompt was sent
	if !strings.Contains(string(capturedBody), "continue seamlessly") {
		t.Errorf("API request body should contain fallback summary prompt")
	}

	// Verify default handoff message
	msgs, _ := store.Load(newKey)
	handoff := provider.TextOf(msgs[2].Content)
	if !strings.Contains(handoff, DefaultHandoffMessage) {
		t.Errorf("handoff = %q, want default handoff", handoff)
	}
}

