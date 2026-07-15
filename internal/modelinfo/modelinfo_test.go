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

func TestAccessorsWithProviderPrefix(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("my-provider", "acc-test", Model{
		ContextWindow: 333_000,
		Effort:         true,
		Thinking:       true,
		Caching:        true,
		InputPer1M:     3.00,
		OutputPer1M:    9.00,
	})

	// ContextWindow with provider prefix.
	if got := ContextWindow("my-provider/acc-test"); got != 333_000 {
		t.Errorf("ContextWindow(\"my-provider/acc-test\") = %d, want 333000", got)
	}

	// Capabilities with provider prefix.
	effort, thinking, speed := Capabilities("my-provider/acc-test")
	if !effort || !thinking || speed {
		t.Errorf("Capabilities(\"my-provider/acc-test\") = (%v, %v, %v), want (true, true, false)", effort, thinking, speed)
	}

	// Caching with provider prefix.
	if got := Caching("my-provider/acc-test"); !got {
		t.Errorf("Caching(\"my-provider/acc-test\") = false, want true")
	}

	// Without provider prefix, should NOT hit the provider-specific entry.
	// ContextWindow falls through to family/default (200k default for unknown).
	if got := ContextWindow("acc-test"); got == 333_000 {
		t.Error("ContextWindow without provider should not hit provider-specific entry")
	}
}

func TestDateSuffixWithProvider(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("prov", "dated-model", Model{
		ContextWindow: 128_000,
		InputPer1M:    1.00,
	})

	// Lookup with date suffix on the bare part should still match.
	m, ok := Lookup("prov", "dated-model-20260715")
	if !ok {
		t.Fatal("Lookup with date suffix should match after stripDateSuffix")
	}
	if m.ContextWindow != 128_000 {
		t.Errorf("ContextWindow = %d, want 128000", m.ContextWindow)
	}

	// Cost with date suffix in the full model string.
	cost := Cost("prov/dated-model-20260715", 1_000_000, 0, 0, 0)
	if cost != 1.00 {
		t.Errorf("Cost = %v, want 1.0", cost)
	}

	// ContextWindow with date suffix.
	if got := ContextWindow("prov/dated-model-20260715"); got != 128_000 {
		t.Errorf("ContextWindow with date suffix = %d, want 128000", got)
	}
}

func TestTwoProvidersSameModel(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("provider-a", "shared-model", Model{
		ContextWindow: 200_000,
		InputPer1M:    1.00,
		OutputPer1M:   5.00,
	})
	Register("provider-b", "shared-model", Model{
		ContextWindow: 400_000,
		InputPer1M:    2.00,
		OutputPer1M:   10.00,
	})

	// Each provider gets its own entry.
	mA, ok := Lookup("provider-a", "shared-model")
	if !ok || mA.InputPer1M != 1.00 || mA.ContextWindow != 200_000 {
		t.Errorf("provider-a entry = %+v, want InputPer1M=1.0 ContextWindow=200000", mA)
	}

	mB, ok := Lookup("provider-b", "shared-model")
	if !ok || mB.InputPer1M != 2.00 || mB.ContextWindow != 400_000 {
		t.Errorf("provider-b entry = %+v, want InputPer1M=2.0 ContextWindow=400000", mB)
	}

	// Cost routes to the right provider.
	costA := Cost("provider-a/shared-model", 1_000_000, 0, 0, 0)
	costB := Cost("provider-b/shared-model", 1_000_000, 0, 0, 0)
	if costA != 1.00 {
		t.Errorf("Cost provider-a = %v, want 1.0", costA)
	}
	if costB != 2.00 {
		t.Errorf("Cost provider-b = %v, want 2.0", costB)
	}

	// ContextWindow also routes correctly.
	if got := ContextWindow("provider-a/shared-model"); got != 200_000 {
		t.Errorf("ContextWindow provider-a = %d, want 200000", got)
	}
	if got := ContextWindow("provider-b/shared-model"); got != 400_000 {
		t.Errorf("ContextWindow provider-b = %d, want 400000", got)
	}
}
