package config

import (
	"testing"
	"time"
)

func TestResolveModelKeepalive_NilResolved(t *testing.T) {
	// Proves that a nil ResolvedModel returns disabled with zero interval.
	enabled, interval := ResolveModelKeepalive(nil)
	if enabled {
		t.Error("expected disabled for nil resolved model")
	}
	if interval != 0 {
		t.Errorf("expected 0 interval, got %v", interval)
	}
}

func TestResolveModelKeepalive_OpenAIAutoDetect(t *testing.T) {
	// Proves that OpenAI models auto-detect keepalive enabled with 95% of 5m TTL.
	resolved := &ResolvedModel{Developer: "openai", ModelID: "gpt-4o"}
	enabled, interval := ResolveModelKeepalive(resolved)
	if !enabled {
		t.Error("expected enabled for openai model")
	}
	want := time.Duration(float64(5*time.Minute) * 0.95)
	if interval != want {
		t.Errorf("interval = %v, want %v", interval, want)
	}
}

func TestResolveModelKeepalive_DeepSeekAutoDetect(t *testing.T) {
	// Proves that DeepSeek models auto-detect keepalive enabled with 95% of 5m TTL.
	resolved := &ResolvedModel{Developer: "deepseek", ModelID: "deepseek-chat"}
	enabled, interval := ResolveModelKeepalive(resolved)
	if !enabled {
		t.Error("expected enabled for deepseek model")
	}
	want := time.Duration(float64(5*time.Minute) * 0.95)
	if interval != want {
		t.Errorf("interval = %v, want %v", interval, want)
	}
}

func TestResolveModelKeepalive_AnthropicNoAutoDetect(t *testing.T) {
	// Proves that Anthropic models do not auto-detect keepalive (not in defaults map).
	resolved := &ResolvedModel{Developer: "anthropic", ModelID: "claude-opus-4-6"}
	enabled, interval := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled for anthropic model (no auto-detect)")
	}
	if interval != 0 {
		t.Errorf("expected 0 interval, got %v", interval)
	}
}

func TestResolveModelKeepalive_ExplicitEnabled(t *testing.T) {
	// Proves that explicit EnableKeepalive=true overrides auto-detect,
	// using the default 5m TTL when no PromptCacheTTL is set.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "anthropic",
		ModelID:         "claude-opus-4-6",
		EnableKeepalive: &boolTrue,
	}
	enabled, interval := ResolveModelKeepalive(resolved)
	if !enabled {
		t.Error("expected enabled when explicitly set")
	}
	want := time.Duration(float64(5*time.Minute) * 0.95)
	if interval != want {
		t.Errorf("interval = %v, want %v", interval, want)
	}
}

func TestResolveModelKeepalive_ExplicitDisabled(t *testing.T) {
	// Proves that explicit EnableKeepalive=false overrides auto-detect for openai.
	boolFalse := false
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolFalse,
	}
	enabled, _ := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled when explicitly set to false")
	}
}

func TestResolveModelKeepalive_CustomTTL(t *testing.T) {
	// Proves that an explicit PromptCacheTTL overrides the default TTL.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		PromptCacheTTL:  "10m",
	}
	enabled, interval := ResolveModelKeepalive(resolved)
	if !enabled {
		t.Error("expected enabled")
	}
	want := time.Duration(float64(10*time.Minute) * 0.95)
	if interval != want {
		t.Errorf("interval = %v, want %v", interval, want)
	}
}

func TestResolveModelKeepalive_InvalidTTL(t *testing.T) {
	// Proves that an invalid PromptCacheTTL returns disabled.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		PromptCacheTTL:  "invalid",
	}
	enabled, _ := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled for invalid TTL")
	}
}

func TestResolveModelKeepalive_ZeroTTL(t *testing.T) {
	// Proves that a zero-value PromptCacheTTL returns disabled.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		PromptCacheTTL:  "0s",
	}
	enabled, _ := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled for zero TTL")
	}
}
