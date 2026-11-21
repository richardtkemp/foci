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
	// using the default 5m TTL when no CacheTTL is set.
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
	// Proves that an explicit CacheTTL overrides the default TTL.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		CacheTTL:        "10m",
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
	// Proves that an invalid CacheTTL returns disabled.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		CacheTTL:        "invalid",
	}
	enabled, _ := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled for invalid TTL")
	}
}

func TestResolveModelKeepalive_ZeroTTL(t *testing.T) {
	// Proves that a zero-value CacheTTL returns disabled.
	boolTrue := true
	resolved := &ResolvedModel{
		Developer:       "openai",
		ModelID:         "gpt-4o",
		EnableKeepalive: &boolTrue,
		CacheTTL:        "0s",
	}
	enabled, _ := ResolveModelKeepalive(resolved)
	if enabled {
		t.Error("expected disabled for zero TTL")
	}
}

func TestResolveModelKeepalive_OpenAIPerModelTTL(t *testing.T) {
	// Proves the OpenAI default TTL follows the model generation (#3240).
	// OpenAI's prompt-caching guide (checked 2026-11-21): GPT-5.6 and later
	// default to a 30-minute minimum lifetime (prompt_cache_options.ttl "30m");
	// earlier in_memory models stay warm only ~5-10 minutes, so they keep 5m.
	cases := []struct {
		model string
		ttl   time.Duration
	}{
		{"gpt-5.6-sol", 30 * time.Minute},
		{"gpt-5.6-luna-pro:batch", 30 * time.Minute},
		{"GPT-5.6-Terra", 30 * time.Minute},
		{"openai/gpt-5.6-sol", 30 * time.Minute},
		{"gpt-6-astra", 30 * time.Minute},
		{"gpt-6.1-terra", 30 * time.Minute},
		{"gpt-6.2-sol", 30 * time.Minute},
		{"gpt-10", 30 * time.Minute},
		{"gpt-5.10", 30 * time.Minute},
		{"gpt-5.5", 5 * time.Minute},
		{"gpt-5.5-pro", 5 * time.Minute},
		{"gpt-5.4mini", 5 * time.Minute},
		{"gpt-5", 5 * time.Minute},
		{"gpt-5-mini", 5 * time.Minute},
		{"gpt-4o", 5 * time.Minute},
		{"gpt-4.1", 5 * time.Minute},
		{"o3", 5 * time.Minute},
		{"gpt-oss-120b", 5 * time.Minute},
		{"gpt-sol-latest", 5 * time.Minute}, // alias: generation unknown, stay conservative
		{"", 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			enabled, interval := ResolveModelKeepalive(&ResolvedModel{Developer: "openai", ModelID: tc.model})
			if !enabled {
				t.Fatal("expected enabled for openai model")
			}
			if want := time.Duration(float64(tc.ttl) * 0.95); interval != want {
				t.Errorf("interval = %v, want %v (TTL %v)", interval, want, tc.ttl)
			}
		})
	}
}

func TestResolveModelKeepalive_ExplicitTTLBeatsOpenAIModelDefault(t *testing.T) {
	// Proves a [models.*] cache_ttl still overrides the per-model default.
	resolved := &ResolvedModel{Developer: "openai", ModelID: "gpt-5.6-sol", CacheTTL: "10m"}
	enabled, interval := ResolveModelKeepalive(resolved)
	if !enabled {
		t.Fatal("expected enabled")
	}
	if want := time.Duration(float64(10*time.Minute) * 0.95); interval != want {
		t.Errorf("interval = %v, want %v", interval, want)
	}
}
