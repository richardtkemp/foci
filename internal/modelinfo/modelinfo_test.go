package modelinfo

import (
	"sync"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	ctx := 300_000
	m := Model{
		ContextWindow: ctx,
		Caching:       true,
		InputPer1M:    2.00, OutputPer1M: 10.00,
		CacheReadPer1M: 0.20, CacheWritePer1M: 2.50,
	}
	Register("", "test-register-model", m)

	got, ok := Lookup("", "test-register-model")
	if !ok {
		t.Fatal("Lookup failed for registered model")
	}
	if got.ContextWindow != ctx {
		t.Errorf("ContextWindow = %d, want %d", got.ContextWindow, ctx)
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty for providerless entry", got.Provider)
	}
}

func TestRegisterWithProvider(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("zai-coding-plan", "glm-5.2", Model{
		ContextWindow: 1_000_000,
		InputPer1M:    0.0, OutputPer1M: 0.0,
	})

	// Provider-specific lookup hits.
	m, ok := Lookup("zai-coding-plan", "glm-5.2")
	if !ok {
		t.Fatal("Lookup with provider failed")
	}
	if m.Provider != "zai-coding-plan" {
		t.Errorf("Provider = %q, want %q", m.Provider, "zai-coding-plan")
	}

	// Providerless lookup misses (no providerless entry for glm-5.2).
	if _, ok := Lookup("", "glm-5.2"); ok {
		t.Error("Lookup without provider should miss when only provider-specific entry exists")
	}

	// Cost with provider prefix hits the provider-specific entry.
	cost := Cost("zai-coding-plan/glm-5.2", 1_000_000, 500_000, 0, 0)
	if cost != 0 {
		t.Errorf("Cost = %v, want 0 (all prices zero)", cost)
	}

	// Cost without provider prefix misses → family/default fallback.
	// (Not a registry hit, so should use fallback pricing.)
	_ = Cost("glm-5.2", 100, 50, 0, 0) // should not panic
}

func TestProviderFallbackToProviderless(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	// Register a providerless entry.
	Register("", "test-fb", Model{
		ContextWindow: 200_000,
		InputPer1M:    1.00,
	})

	// Provider-specific lookup falls back to providerless.
	m, ok := Lookup("some-provider", "test-fb")
	if !ok {
		t.Fatal("Lookup with unknown provider should fall back to providerless")
	}
	if m.InputPer1M != 1.00 {
		t.Errorf("InputPer1M = %v, want 1.0 (providerless fallback)", m.InputPer1M)
	}

	// Provider-specific entry takes priority when it exists.
	Register("specific", "test-fb", Model{
		ContextWindow: 400_000,
		InputPer1M:    2.00,
	})
	m, ok = Lookup("specific", "test-fb")
	if !ok {
		t.Fatal("Lookup with specific provider failed")
	}
	if m.InputPer1M != 2.00 {
		t.Errorf("InputPer1M = %v, want 2.0 (provider-specific)", m.InputPer1M)
	}
}

func TestRegisterOverridesExisting(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	original, _ := Lookup("", "claude-haiku-4-5")
	newPrice := 99.99
	Register("", "claude-haiku-4-5", Model{
		ContextWindow: original.ContextWindow,
		Caching:       original.Caching,
		InputPer1M:    newPrice, OutputPer1M: original.OutputPer1M,
		CacheReadPer1M:  original.CacheReadPer1M,
		CacheWritePer1M: original.CacheWritePer1M,
	})

	got, ok := Lookup("", "claude-haiku-4-5")
	if !ok {
		t.Fatal("Lookup failed")
	}
	if got.InputPer1M != newPrice {
		t.Errorf("InputPer1M = %v, want %v (override did not take effect)", got.InputPer1M, newPrice)
	}

	cost := Cost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != newPrice {
		t.Errorf("Cost with 1M input = %v, want %v", cost, newPrice)
	}
}

func TestResetToBuiltIn(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("", "temp-model", Model{ContextWindow: 500_000})
	if _, ok := Lookup("", "temp-model"); !ok {
		t.Fatal("Register did not create entry")
	}

	ResetToBuiltIn()

	if _, ok := Lookup("", "temp-model"); ok {
		t.Error("temp-model still in registry after ResetToBuiltIn")
	}
	if _, ok := Lookup("", "claude-haiku-4-5"); !ok {
		t.Error("built-in claude-haiku-4-5 missing after ResetToBuiltIn")
	}
}

func TestRegisterLowercasesKey(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("", "My-Model", Model{ContextWindow: 100_000})

	if _, ok := Lookup("", "my-model"); !ok {
		t.Error("Lookup with lowercase key failed after Register with mixed-case key")
	}
	if _, ok := Lookup("", "My-Model"); !ok {
		t.Error("Lookup with original-case key failed after Register")
	}
}

func TestConcurrentAccessNoRace(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			Register("", "concurrent-test", Model{ContextWindow: i})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = ContextWindow("claude-haiku-4-5")
			_ = Cost("claude-haiku-4-5", 100, 50, 0, 0)
			_, _, _ = Capabilities("claude-haiku-4-5")
			_ = Caching("claude-haiku-4-5")
		}
	}()

	wg.Wait()
}
