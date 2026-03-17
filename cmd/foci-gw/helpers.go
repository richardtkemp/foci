package main

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// resolveZeroable returns the per-agent value if non-zero, otherwise global.
// Works for any comparable type where zero value means "not set".
func resolveZeroable[T comparable](perAgent, global T) T {
	var zero T
	if perAgent != zero {
		return perAgent
	}
	return global
}

// resolvePtr returns *perAgent if non-nil, otherwise global.
// Works for any pointer type.
func resolvePtr[T any](perAgent *T, global T) T {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// Typed wrappers for readability at call sites.

// resolveInt returns the per-agent value if non-zero, otherwise global.
func resolveInt(perAgent, global int) int {
	return resolveZeroable(perAgent, global)
}

// resolveInt64 returns the per-agent value if non-zero, otherwise global.
func resolveInt64(perAgent, global int64) int64 {
	return resolveZeroable(perAgent, global)
}

// resolveIntPtr returns *perAgent if non-nil, otherwise global.
func resolveIntPtr(perAgent *int, global int) int {
	return resolvePtr(perAgent, global)
}

// resolveBoolPtr returns the per-agent value if non-nil, otherwise the global default.
func resolveBoolPtr(perAgent *bool, global bool) bool {
	return resolvePtr(perAgent, global)
}

// resolveFloat64Ptr returns *perAgent if non-nil, otherwise global.
func resolveFloat64Ptr(perAgent *float64, global float64) float64 {
	return resolvePtr(perAgent, global)
}

// resolveString returns the per-agent value if non-empty, otherwise global.
func resolveString(perAgent, global string) string {
	return resolveZeroable(perAgent, global)
}

// resolveIntPtrPtr returns perAgent if non-nil, otherwise global (both are *int).
func resolveIntPtrPtr(perAgent, global *int) *int {
	if perAgent != nil {
		return perAgent
	}
	return global
}

// resolveIdlePreserve is an alias for resolveIntPtrPtr for idle-preserve config.
var resolveIdlePreserve = resolveIntPtrPtr

// resolveFloat64PtrDefault returns *perAgent if non-nil, *global if non-nil, otherwise fallback.
func resolveFloat64PtrDefault(perAgent, global *float64, fallback float64) float64 {
	if perAgent != nil {
		return *perAgent
	}
	if global != nil {
		return *global
	}
	return fallback
}

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

// resolveShowToolCalls resolves the effective show_tool_calls value:
// per-agent → global telegram → "off".
func resolveShowToolCalls(acfg config.AgentConfig, cfg *config.Config) string {
	switch {
	case acfg.ShowToolCalls != nil:
		return string(*acfg.ShowToolCalls)
	case cfg.Telegram.ShowToolCalls != nil:
		return string(*cfg.Telegram.ShowToolCalls)
	default:
		return string(config.ToolCallOff)
	}
}

// resolveStreamingConfig resolves the streaming setting for an agent.
// Per-agent *bool overrides defaults *bool which overrides global anthropic.streaming.
// Streaming is forced off when use_sdk is false.
func resolveStreamingConfig(acfg config.AgentConfig, cfg *config.Config) bool {
	if !cfg.Anthropic.UseSDK {
		return false // streaming requires SDK
	}
	if acfg.Streaming != nil {
		return *acfg.Streaming
	}
	return cfg.Anthropic.Streaming
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