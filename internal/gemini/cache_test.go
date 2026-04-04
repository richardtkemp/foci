package gemini

import (
	"context"
	"testing"

	"google.golang.org/genai"
)

func TestContentHash_Deterministic(t *testing.T) {
	// Proves that contentHash produces the same output for the same inputs on repeated calls.
	system := &genai.Content{
		Parts: []*genai.Part{{Text: "You are helpful."}},
		Role:  "user",
	}
	tools := []*genai.Tool{
		{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "exec", Description: "run commands"},
		}},
	}

	h1 := contentHash(system, tools)
	h2 := contentHash(system, tools)
	if h1 != h2 {
		t.Errorf("contentHash not deterministic: %x != %x", h1, h2)
	}
}

func TestContentHash_DiffersOnChange(t *testing.T) {
	// Proves that changing the system prompt content produces a different hash, so cache misses are detected.
	system1 := &genai.Content{
		Parts: []*genai.Part{{Text: "Version 1"}},
		Role:  "user",
	}
	system2 := &genai.Content{
		Parts: []*genai.Part{{Text: "Version 2"}},
		Role:  "user",
	}

	h1 := contentHash(system1, nil)
	h2 := contentHash(system2, nil)
	if h1 == h2 {
		t.Error("contentHash should differ for different system prompts")
	}
}

func TestContentHash_NilInputs(t *testing.T) {
	// Proves that contentHash handles nil system content and nil tools,
	// returning a deterministic hash.
	h1 := contentHash(nil, nil)
	h2 := contentHash(nil, nil)
	if h1 != h2 {
		t.Errorf("contentHash(nil, nil) not deterministic: %x vs %x", h1, h2)
	}
}

func TestContentHash_ToolsOnly(t *testing.T) {
	// Proves that tool definitions contribute to the hash, so adding tools invalidates the cache.
	tools := []*genai.Tool{
		{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "read", Description: "read files"},
		}},
	}

	h1 := contentHash(nil, tools)
	h2 := contentHash(nil, nil)
	if h1 == h2 {
		t.Error("contentHash should differ when tools are added")
	}
}

func TestNewCacheManager_DefaultTTL(t *testing.T) {
	// Proves that a zero or negative TTL is replaced with a sensible default (1 hour) so misconfigured deployments still work.
	m := NewCacheManager(nil, 0)
	if m.ttl.Hours() != 1 {
		t.Errorf("ttl = %v, want 1h", m.ttl)
	}

	m = NewCacheManager(nil, -1)
	if m.ttl.Hours() != 1 {
		t.Errorf("ttl = %v, want 1h", m.ttl)
	}
}

func TestEnsureCache_NothingToCache(t *testing.T) {
	// Proves that EnsureCache returns an empty name when there is nothing cacheable (no system prompt, no tools), avoiding unnecessary API calls.
	m := NewCacheManager(nil, 0)
	name := m.EnsureCache(context.TODO(), "model", nil, nil)
	if name != "" {
		t.Errorf("expected empty cache name, got %q", name)
	}
}
