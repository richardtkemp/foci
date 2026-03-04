package gemini

import (
	"context"
	"testing"

	"google.golang.org/genai"
)

func TestContentHash_Deterministic(t *testing.T) {
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
	h := contentHash(nil, nil)
	// Should not panic, and should return a valid hash
	// MD5 of encoding nothing produces all zeros - that's fine, it represents "nothing to cache"
	_ = h
}

func TestContentHash_ToolsOnly(t *testing.T) {
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
	// TTL <= 0 should default to 1 hour
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
	m := NewCacheManager(nil, 0)
	// nil system and no tools → nothing to cache
	name := m.EnsureCache(context.TODO(), "model", nil, nil)
	if name != "" {
		t.Errorf("expected empty cache name, got %q", name)
	}
}
