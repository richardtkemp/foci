package gemini

import (
	"context"
	"net/http"
	"testing"
	"time"

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

func TestEnsureCache_ExtendsTTLNearExpiry(t *testing.T) {
	// Proves that reusing a cache past the halfway point of its TTL triggers a TTL extension (PATCH) and pushes expiry forward, preventing expiry during active use.
	f := newFakeAPI()
	m := newTestCacheManager(t, f, time.Hour)
	ctx := context.Background()
	system := testSystem("be helpful")

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name != "cachedContents/test-cache-1" {
		t.Fatalf("create: name = %q", name)
	}

	// Simulate time passing beyond the halfway point of the TTL.
	m.mu.Lock()
	m.expiresAt = time.Now().Add(m.ttl / 4)
	m.mu.Unlock()

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name != "cachedContents/test-cache-1" {
		t.Fatalf("reuse: name = %q", name)
	}
	if got := f.callCount("cacheUpdate"); got != 1 {
		t.Errorf("cacheUpdate calls = %d, want 1", got)
	}
	m.mu.Lock()
	extended := m.expiresAt.After(time.Now().Add(m.ttl / 2))
	m.mu.Unlock()
	if !extended {
		t.Error("expiresAt should have been pushed forward by the TTL extension")
	}
}

func TestEnsureCache_ExtendTTLFailureKeepsCache(t *testing.T) {
	// Proves that a failed TTL extension is non-fatal: the cache name is still returned and the recorded expiry is left unchanged.
	f := newFakeAPI()
	f.set("cacheUpdate", http.StatusInternalServerError, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`)
	m := newTestCacheManager(t, f, time.Hour)
	ctx := context.Background()
	system := testSystem("be helpful")

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name == "" {
		t.Fatal("create failed")
	}

	nearExpiry := time.Now().Add(m.ttl / 4)
	m.mu.Lock()
	m.expiresAt = nearExpiry
	m.mu.Unlock()

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name != "cachedContents/test-cache-1" {
		t.Errorf("name = %q, want cache despite failed extension", name)
	}
	m.mu.Lock()
	unchanged := m.expiresAt.Equal(nearExpiry)
	m.mu.Unlock()
	if !unchanged {
		t.Error("expiresAt should be unchanged after failed extension")
	}
}

func TestEnsureCache_ExpiredRecreates(t *testing.T) {
	// Proves that an expired cache is deleted and recreated rather than reused, since the server would reject a reference to it.
	f := newFakeAPI()
	m := newTestCacheManager(t, f, time.Hour)
	ctx := context.Background()
	system := testSystem("be helpful")

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name == "" {
		t.Fatal("create failed")
	}

	m.mu.Lock()
	m.expiresAt = time.Now().Add(-time.Minute)
	m.mu.Unlock()

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name != "cachedContents/test-cache-1" {
		t.Errorf("recreate: name = %q", name)
	}
	if got := f.callCount("cacheDelete"); got != 1 {
		t.Errorf("cacheDelete calls = %d, want 1", got)
	}
	if got := f.callCount("cacheCreate"); got != 2 {
		t.Errorf("cacheCreate calls = %d, want 2", got)
	}
}

func TestEnsureCache_ContentChangeRecreates(t *testing.T) {
	// Proves that changing the system prompt or the model invalidates the cache: the old one is deleted and a fresh one created for the new content.
	f := newFakeAPI()
	m := newTestCacheManager(t, f, time.Hour)
	ctx := context.Background()

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", testSystem("v1"), nil); name == "" {
		t.Fatal("create failed")
	}

	// Changed system prompt → delete + recreate.
	if name := m.EnsureCache(ctx, "gemini-2.5-flash", testSystem("v2"), nil); name == "" {
		t.Fatal("recreate after content change failed")
	}
	if got := f.callCount("cacheDelete"); got != 1 {
		t.Errorf("cacheDelete calls = %d, want 1", got)
	}

	// Changed model → delete + recreate again.
	if name := m.EnsureCache(ctx, "gemini-2.5-pro", testSystem("v2"), nil); name == "" {
		t.Fatal("recreate after model change failed")
	}
	if got := f.callCount("cacheDelete"); got != 2 {
		t.Errorf("cacheDelete calls = %d, want 2", got)
	}
	if got := f.callCount("cacheCreate"); got != 3 {
		t.Errorf("cacheCreate calls = %d, want 3", got)
	}
}

func TestEnsureCache_CreateErrorRetriesNextCall(t *testing.T) {
	// Proves that transient cache-creation failures (server error, plain rate limit) do not permanently disable caching — the next call tries again, unlike free-tier detection.
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`},
		{"plain rate limit", http.StatusTooManyRequests, `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI()
			f.set("cacheCreate", tt.status, tt.body)
			m := newTestCacheManager(t, f, time.Hour)
			ctx := context.Background()
			system := testSystem("be helpful")

			if name := m.EnsureCache(ctx, "gemini-2.5-flash", system, nil); name != "" {
				t.Errorf("name = %q, want empty on create failure", name)
			}
			if m.IsCachingNotSupported() {
				t.Error("transient failure should not mark caching as unsupported")
			}
			// Next call should attempt creation again.
			_ = m.EnsureCache(ctx, "gemini-2.5-flash", system, nil)
			if got := f.callCount("cacheCreate"); got != 2 {
				t.Errorf("cacheCreate calls = %d, want 2 (retry on next call)", got)
			}
		})
	}
}

func TestCacheManager_Close(t *testing.T) {
	// Proves that Close deletes the active cache and resets state even when the delete request fails, and that a second Close is a no-op.
	f := newFakeAPI()
	f.set("cacheDelete", http.StatusInternalServerError, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`)
	m := newTestCacheManager(t, f, time.Hour)
	ctx := context.Background()

	if name := m.EnsureCache(ctx, "gemini-2.5-flash", testSystem("be helpful"), nil); name == "" {
		t.Fatal("create failed")
	}

	m.Close(ctx)
	if got := f.callCount("cacheDelete"); got != 1 {
		t.Errorf("cacheDelete calls = %d, want 1", got)
	}
	m.mu.Lock()
	cleared := m.cacheName == "" && m.model == ""
	m.mu.Unlock()
	if !cleared {
		t.Error("cache state should be reset even when delete fails")
	}

	// Second Close must not issue another delete.
	m.Close(ctx)
	if got := f.callCount("cacheDelete"); got != 1 {
		t.Errorf("cacheDelete calls after second Close = %d, want 1", got)
	}
}
