package main

import (
	"testing"

	"foci/internal/config"
	"foci/internal/provider"
)

func TestModelDefaultsFn_ThreadsProviderRouting(t *testing.T) {
	// Proves that modelDefaultsFn (used by both the agent turn loop and
	// compaction to look up per-model settings) carries a model's
	// [models.X.provider] config through into config.ModelDefaults.ProviderRouting
	// — the one-line addition that connects config parsing (internal/config)
	// to request building (internal/openai's buildParams). Without this,
	// ProviderRouting would always be nil regardless of config (#1478).
	sort := &provider.ProviderSort{By: "price"}
	models := map[string]config.ModelConfig{
		"deepseek": {
			Model: "openrouter/deepseek/deepseek-v4-pro:floor",
			Provider: &provider.ProviderRouting{
				Sort: sort,
			},
		},
		"plain": {
			Model: "anthropic/claude-haiku-4-5",
		},
	}

	fn := modelDefaultsFn(models)
	if fn == nil {
		t.Fatal("modelDefaultsFn returned nil for non-empty models map")
	}

	md := fn("openrouter/deepseek/deepseek-v4-pro:floor")
	if md.ProviderRouting == nil {
		t.Fatal("expected ProviderRouting to be populated for the deepseek model")
	}
	if md.ProviderRouting.Sort == nil || md.ProviderRouting.Sort.By != "price" {
		t.Errorf("ProviderRouting.Sort = %+v, want {By: price}", md.ProviderRouting.Sort)
	}

	mdPlain := fn("anthropic/claude-haiku-4-5")
	if mdPlain.ProviderRouting != nil {
		t.Errorf("ProviderRouting = %+v, want nil for a model with no [models.X.provider] block", mdPlain.ProviderRouting)
	}

	mdUnknown := fn("openai/gpt-4o")
	if mdUnknown.ProviderRouting != nil {
		t.Errorf("ProviderRouting = %+v, want nil for a model not present in the models map", mdUnknown.ProviderRouting)
	}
}

func TestModelDefaultsFn_CaseInsensitiveModelKey(t *testing.T) {
	// Proves that per-model defaults match the developer/model_id
	// case-insensitively on the WHOLE string: an entry spelled
	// "OpenRouter/DeepSeek/DeepSeek-V4-Flash" in config is found when the
	// session or a fallback asks for the all-lowercase spelling, and the
	// reverse. Before the fold, every per-model default (thinking, effort,
	// speed, cache, provider routing) was silently dropped on a case
	// difference.
	sort := &provider.ProviderSort{By: "price"}
	lowerSort := &provider.ProviderSort{By: "latency"}
	models := map[string]config.ModelConfig{
		"deepseek": {
			Model:         "OpenRouter/DeepSeek/DeepSeek-V4-Flash",
			Provider:      &provider.ProviderRouting{Sort: sort},
			CacheTTL:      "1h",
			CacheStrategy: "explicit",
			Thinking:      "adaptive",
			Effort:        "low",
			Speed:         "fast",
			Context:       123456,
		},
		"lower": {
			Model:    "openrouter/qwen/qwen3-x",
			Provider: &provider.ProviderRouting{Sort: lowerSort},
			Effort:   "high",
		},
	}

	fn := modelDefaultsFn(models)
	if fn == nil {
		t.Fatal("modelDefaultsFn returned nil for non-empty models map")
	}

	// Config spelled in mixed case, asked in lowercase.
	md := fn("openrouter/deepseek/deepseek-v4-flash")
	if md.ProviderRouting == nil || md.ProviderRouting.Sort == nil || md.ProviderRouting.Sort.By != "price" {
		t.Errorf("md(config-mixed-case, ask-lower) ProviderRouting = %+v, want the [models.deepseek.provider] table", md.ProviderRouting)
	}
	if md.CacheTTL != "1h" || md.CacheStrategy != "explicit" {
		t.Errorf("md(config-mixed-case, ask-lower) cache = {%q %q}, want {1h explicit}", md.CacheStrategy, md.CacheTTL)
	}

	// The reverse: config lowercase, asked in a different case. The lower
	// entry carries distinguishing settings so a hit cannot look like the
	// zero fallback a miss returns.
	mdRev := fn("OpenRouter/Qwen/Qwen3-X")
	if mdRev.Effort != "high" {
		t.Errorf("md(config-lower, ask-mixed) Effort = %q, want high (the lower entry's own setting)", mdRev.Effort)
	}
	if mdRev.ProviderRouting == nil || mdRev.ProviderRouting.Sort == nil || mdRev.ProviderRouting.Sort.By != "latency" {
		t.Errorf("md(config-lower, ask-mixed) ProviderRouting = %+v, want the [models.lower.provider] table", mdRev.ProviderRouting)
	}
	// The deepseek entry's settings must not leak into the lower hit.
	if mdRev.CacheTTL != "" || mdRev.CacheStrategy != "" {
		t.Errorf("md(config-lower, ask-mixed) cache = {%q %q}, want empty (no leakage from the deepseek entry)", mdRev.CacheStrategy, mdRev.CacheTTL)
	}

	// A model id in yet another spelling still misses (no substring folding).
	mdMiss := fn("openrouter/deepseek/deepseek-v4-pro")
	if mdMiss.ProviderRouting != nil || mdMiss.CacheTTL != "" {
		t.Errorf("md(different model) = %+v, want the empty defaults (a different model id must not match)", mdMiss)
	}
}

func TestModelMetaFn_CaseInsensitiveModelKey(t *testing.T) {
	// Proves modelMetaFn (the context-window lookup) matches
	// case-insensitively too — same match rule as modelDefaultsFn, same
	// silent-drop bug before the fold.
	models := map[string]config.ModelConfig{
		"deepseek": {
			Model:   "OpenRouter/DeepSeek/DeepSeek-V4-Flash",
			Context: 200000,
		},
		"lower": {
			Model:   "openrouter/qwen/qwen3-x",
			Context: 111000,
		},
	}

	fn := modelMetaFn(models)
	if fn == nil {
		t.Fatal("modelMetaFn returned nil for non-empty models map")
	}
	if got := fn("openrouter/deepseek/deepseek-v4-flash").ContextWindow; got != 200000 {
		t.Errorf("meta(config-mixed-case, ask-lower) ContextWindow = %d, want 200000", got)
	}
	if got := fn("OpenRouter/Qwen/Qwen3-X").ContextWindow; got != 111000 {
		t.Errorf("meta(config-lower, ask-mixed) ContextWindow = %d, want 111000", got)
	}
	if got := fn("openrouter/other/model").ContextWindow; got != 0 {
		t.Errorf("meta(unknown model) ContextWindow = %d, want 0", got)
	}
}
