package modelinfo

import (
	"sync"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	// Save and restore registry state.
	t.Cleanup(ResetToBuiltIn)

	ctx := 300_000
	m := Model{
		ContextWindow: ctx,
		Caching:       true,
		InputPer1M:    2.00, OutputPer1M: 10.00,
		CacheReadPer1M: 0.20, CacheWritePer1M: 2.50,
	}
	Register("test-register-model", m)

	got, ok := Lookup("test-register-model")
	if !ok {
		t.Fatal("Lookup failed for registered model")
	}
	if got.ContextWindow != ctx {
		t.Errorf("ContextWindow = %d, want %d", got.ContextWindow, ctx)
	}
}

func TestRegisterOverridesExisting(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	original, _ := Lookup("claude-haiku-4-5")
	newPrice := 99.99
	Register("claude-haiku-4-5", Model{
		ContextWindow: original.ContextWindow,
		Caching:       original.Caching,
		InputPer1M:    newPrice, OutputPer1M: original.OutputPer1M,
		CacheReadPer1M:  original.CacheReadPer1M,
		CacheWritePer1M: original.CacheWritePer1M,
	})

	got, ok := Lookup("claude-haiku-4-5")
	if !ok {
		t.Fatal("Lookup failed")
	}
	if got.InputPer1M != newPrice {
		t.Errorf("InputPer1M = %v, want %v (override did not take effect)", got.InputPer1M, newPrice)
	}

	// Cost should use the overridden price.
	cost := Cost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != newPrice {
		t.Errorf("Cost with 1M input = %v, want %v", cost, newPrice)
	}
}

func TestResetToBuiltIn(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("temp-model", Model{ContextWindow: 500_000})
	if _, ok := Lookup("temp-model"); !ok {
		t.Fatal("Register did not create entry")
	}

	ResetToBuiltIn()

	if _, ok := Lookup("temp-model"); ok {
		t.Error("temp-model still in registry after ResetToBuiltIn")
	}

	// Built-in entries should still be present.
	if _, ok := Lookup("claude-haiku-4-5"); !ok {
		t.Error("built-in claude-haiku-4-5 missing after ResetToBuiltIn")
	}
}

func TestRegisterLowercasesKey(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("My-Model", Model{ContextWindow: 100_000})

	if _, ok := Lookup("my-model"); !ok {
		t.Error("Lookup with lowercase key failed after Register with mixed-case key")
	}
	if _, ok := Lookup("My-Model"); !ok {
		t.Error("Lookup with original-case key failed after Register")
	}
}

func TestConcurrentAccessNoRace(t *testing.T) {
	// This test is meant to be run with -race.
	t.Cleanup(ResetToBuiltIn)

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer goroutine.
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			Register("concurrent-test", Model{ContextWindow: i})
		}
	}()

	// Reader goroutine.
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
