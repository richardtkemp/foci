package main

import (
	"foci/internal/config"
	"foci/internal/log"
)

// warnStreamOutputWithoutStreaming warns at startup if any agent has
// stream_output enabled on a platform but the agent's intelligence provider
// does not have streaming enabled. In that case stream_output silently
// does nothing, which is confusing.
func warnStreamOutputWithoutStreaming(cfg *config.Config) {
	for _, msg := range checkStreamOutputWithoutStreaming(cfg) {
		log.Warnf("startup", "%s", msg)
	}
}

// checkStreamOutputWithoutStreaming returns warning messages for agents that
// have stream_output enabled but streaming disabled.
func checkStreamOutputWithoutStreaming(cfg *config.Config) []string {
	var warnings []string
	for _, acfg := range cfg.Agents {
		streaming := resolveStreamingConfig(acfg, cfg)
		if streaming {
			continue // provider streaming is on — no conflict
		}

		// Resolve effective stream_output for each platform.
		streamOutput := false
		if tg := acfg.Platform("telegram"); tg != nil && tg.StreamOutput != nil {
			streamOutput = *tg.StreamOutput
		}

		if streamOutput {
			warnings = append(warnings, "agent \""+acfg.ID+"\": stream_output is enabled for Telegram but streaming is disabled for the intelligence provider — streaming output will not work")
		}
	}
	return warnings
}
