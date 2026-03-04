package main

import (
	"time"

	"foci/config"
)

// resolveInt returns the per-agent value if non-zero, otherwise global.
func resolveInt(perAgent, global int) int {
	if perAgent != 0 {
		return perAgent
	}
	return global
}

// resolveInt64 returns the per-agent value if non-zero, otherwise global.
func resolveInt64(perAgent, global int64) int64 {
	if perAgent != 0 {
		return perAgent
	}
	return global
}

// resolveIntPtr returns *perAgent if non-nil, otherwise global.
func resolveIntPtr(perAgent *int, global int) int {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// resolveBoolPtr returns the per-agent value if non-nil, otherwise the global default.
func resolveBoolPtr(perAgent *bool, global bool) bool {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// resolveFloat64Ptr returns *perAgent if non-nil, otherwise global.
func resolveFloat64Ptr(perAgent *float64, global float64) float64 {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// resolveString returns the per-agent value if non-empty, otherwise global.
func resolveString(perAgent, global string) string {
	if perAgent != "" {
		return perAgent
	}
	return global
}

// resolveOrientPath resolves the branch orientation prompt path for a given variant.
// Precedence: specific per-agent → specific global → deprecated per-agent → deprecated global.
func resolveOrientPath(specificAgent, specificGlobal, deprecatedAgent, deprecatedGlobal string) string {
	if specificAgent != "" {
		return specificAgent
	}
	if specificGlobal != "" {
		return specificGlobal
	}
	return resolveString(deprecatedAgent, deprecatedGlobal)
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