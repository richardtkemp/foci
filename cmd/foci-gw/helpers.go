package main

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
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

// resolveNotify returns the effective NotifyConfig for a platform connection
// by resolving the 5-level cascade.
func resolveNotify(acfg config.AgentConfig, cfg *config.Config, platformName string) config.NotifyConfig {
	return config.Merge(
		acfg.Platform(platformName).SafeNotify(),
		acfg.Notify,
		cfg.Platform(platformName).SafeNotify(),
		cfg.Defaults.Notify,
	)
}

// resolveDebug returns the effective DebugConfig for a platform connection
// by resolving the 5-level cascade: per-agent platform → per-agent debug →
// global platform → global [debug].
func resolveDebug(acfg config.AgentConfig, cfg *config.Config, platformName string) config.DebugConfig {
	return config.Merge(
		acfg.Platform(platformName).SafeDebug(),
		acfg.Debug,
		cfg.Platform(platformName).SafeDebug(),
		cfg.Debug,
	)
}

// anyDebugEnabled checks if any platform for this agent has the given
// debug feature enabled.
func anyDebugEnabled(acfg config.AgentConfig, cfg *config.Config, checker func(config.DebugConfig) bool) bool {
	for _, p := range cfg.Platforms {
		if checker(resolveDebug(acfg, cfg, p.ID)) {
			return true
		}
	}
	return false
}

// maxInjectionLevel returns the most permissive InjectionLevel across all
// platforms for a given extractor.
func maxInjectionLevel(acfg config.AgentConfig, cfg *config.Config, extract func(config.DebugConfig) config.InjectionLevel) config.InjectionLevel {
	best := config.InjectionOff
	for _, p := range cfg.Platforms {
		level := extract(resolveDebug(acfg, cfg, p.ID))
		if level == config.InjectionAll {
			return config.InjectionAll
		}
		if level == config.InjectionErrors {
			best = config.InjectionErrors
		}
	}
	return best
}

// resolveDisplay returns the effective DisplayConfig for an agent by resolving
// the cascade: per-agent → global defaults → platform defaults (any platform).
// This is for agent-level resolution (environment block, agent struct);
// platform-specific display resolution is done in ApplyAgentDisplaySettings.
func resolveDisplay(acfg config.AgentConfig, cfg *config.Config) config.DisplayConfig {
	layers := []config.DisplayConfig{acfg.Display, cfg.Defaults.Display}
	for _, p := range cfg.Platforms {
		layers = append(layers, p.Display)
	}
	return config.Merge(layers...)
}

// resolveShowToolCalls resolves the effective show_tool_calls value via the
// display config cascade: per-agent → global defaults → platform defaults → code default.
func resolveShowToolCalls(acfg config.AgentConfig, cfg *config.Config) string {
	dc := resolveDisplay(acfg, cfg)
	if dc.ShowToolCalls != nil {
		return string(*dc.ShowToolCalls)
	}
	return string(config.ToolCallOff)
}

// modelParamsFn returns a function that looks up per-model API params
// (thinking, effort, speed) from [models.*] config by matching the
// developer/model_id string.
func modelParamsFn(models map[string]config.ModelConfig) func(string) (string, string, string) {
	if len(models) == 0 {
		return nil
	}
	return func(model string) (thinking, effort, speed string) {
		for _, mc := range models {
			if mc.Model == model {
				return string(mc.Thinking), mc.Effort, mc.Speed
			}
		}
		return "", "", ""
	}
}

// modelMetaFn returns a function that looks up structural model metadata
// (context window) from [models.*] config by matching the developer/model_id string.
func modelMetaFn(models map[string]config.ModelConfig) func(string) modelinfo.ModelMeta {
	if len(models) == 0 {
		return nil
	}
	return func(model string) modelinfo.ModelMeta {
		for _, mc := range models {
			if mc.Model == model {
				return modelinfo.ModelMeta{ContextWindow: int(mc.Context)}
			}
		}
		return modelinfo.ModelMeta{}
	}
}

// resolveStreamingConfig resolves the streaming setting for an agent.
// Cascade: per-agent → global defaults → false.
func resolveStreamingConfig(acfg config.AgentConfig, cfg *config.Config) bool {
	dc := config.Merge(acfg.Display, cfg.Defaults.Display)
	if dc.Streaming != nil {
		return *dc.Streaming
	}
	return false
}

// buildBotConflictSkipSet returns a map of agent IDs that should be skipped
// because they share a bot token with an earlier agent.  It also logs a loud
// banner for each conflict so operators notice immediately.
func buildBotConflictSkipSet(conflicts []config.BotTokenConflict) map[string]string {
	skip := make(map[string]string)
	for _, c := range conflicts {
		ids := strings.Join(c.AgentIDs, ", ")
		log.Errorf("main", "==============================================================")
		log.Errorf("main", "  DUPLICATE BOT TOKEN: %s bot %q used by agents: %s", c.Platform, c.BotName, ids)
		log.Errorf("main", "  Only agent %q will be started. Others skipped.", c.AgentIDs[0])
		log.Errorf("main", "==============================================================")
		for _, id := range c.AgentIDs[1:] {
			skip[id] = fmt.Sprintf("duplicate %s bot %q (already used by agent %q)", c.Platform, c.BotName, c.AgentIDs[0])
		}
	}
	return skip
}