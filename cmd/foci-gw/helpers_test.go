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
