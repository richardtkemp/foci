package main

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/modelinfo"
)

// parseDurationDefault parses a Go duration string, returning fallback on error or empty.
func parseDurationDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// anyNotifyEnabled checks if any platform for this agent has the given
// notification feature enabled, using pre-resolved per-platform notify.
func anyNotifyEnabled(rc *config.ResolvedAgentConfig, cfg *config.Config, checker func(config.ResolvedNotify) bool) bool {
	for _, p := range cfg.Platforms {
		if checker(rc.PlatformNotify(p.ID)) {
			return true
		}
	}
	return false
}

// maxInjectionLevel returns the most permissive InjectionLevel across all
// platforms for a given extractor, using pre-resolved per-platform notify.
func maxInjectionLevel(rc *config.ResolvedAgentConfig, cfg *config.Config, extract func(config.ResolvedNotify) config.InjectionLevel) config.InjectionLevel {
	best := config.InjectionOff
	for _, p := range cfg.Platforms {
		level := extract(rc.PlatformNotify(p.ID))
		if level == config.InjectionAll {
			return config.InjectionAll
		}
		if level == config.InjectionErrors {
			best = config.InjectionErrors
		}
	}
	return best
}

// resolveShowToolCalls resolves the effective show_tool_calls value from
// the pre-resolved display config.
func resolveShowToolCalls(rc *config.ResolvedAgentConfig) string {
	if rc.Display.ShowToolCalls != "" {
		return rc.Display.ShowToolCalls
	}
	return string(config.ToolCallOff)
}

// modelMetaFn returns a function that looks up per-model metadata
// from [models.*] config by matching the developer/model_id string.
// Matching is case-insensitive on the whole string (config.ModelKey):
// "OpenRouter/Qwen/X" in config matches "openrouter/qwen/x" at lookup.
func modelMetaFn(models map[string]config.ModelConfig) func(string) modelinfo.ModelMeta {
	if len(models) == 0 {
		return nil
	}
	return func(model string) modelinfo.ModelMeta {
		key := config.ModelKey(model)
		for _, mc := range models {
			if config.ModelKey(mc.Model) == key {
				return modelinfo.ModelMeta{
					ContextWindow: int(mc.Context),
				}
			}
		}
		return modelinfo.ModelMeta{}
	}
}

// modelDefaultsFn returns a function that looks up per-model defaults
// from [models.*] config by matching the developer/model_id string,
// case-insensitively (config.ModelKey) — see modelMetaFn.
func modelDefaultsFn(models map[string]config.ModelConfig) func(string) config.ModelDefaults {
	if len(models) == 0 {
		return nil
	}
	return func(model string) config.ModelDefaults {
		key := config.ModelKey(model)
		for _, mc := range models {
			if config.ModelKey(mc.Model) == key {
				return config.ModelDefaults{
					Thinking:        string(mc.Thinking),
					Effort:          mc.Effort,
					Speed:           mc.Speed,
					CacheStrategy:   mc.CacheStrategy,
					CacheTTL:        mc.CacheTTL,
					ProviderRouting: mc.Provider,
				}
			}
		}
		return config.ModelDefaults{}
	}
}

// buildBotConflictSkipSet returns a map of agent IDs that should be skipped
// because they share a bot token with an earlier agent.  It also logs a loud
// banner for each conflict so operators notice immediately.
func buildBotConflictSkipSet(conflicts []config.BotTokenConflict) map[string]string {
	skip := make(map[string]string)
	for _, c := range conflicts {
		ids := strings.Join(c.AgentIDs, ", ")
		mainLog.Errorf("==============================================================")
		mainLog.Errorf("  DUPLICATE BOT TOKEN: %s bot %q used by agents: %s", c.Platform, c.BotName, ids)
		mainLog.Errorf("  Only agent %q will be started. Others skipped.", c.AgentIDs[0])
		mainLog.Errorf("==============================================================")
		for _, id := range c.AgentIDs[1:] {
			skip[id] = fmt.Sprintf("duplicate %s bot %q (already used by agent %q)", c.Platform, c.BotName, c.AgentIDs[0])
		}
	}
	return skip
}
