package main

import (
	"foci/internal/config"
)

// warnStreamOutputWithoutStreaming warns at startup if any agent has
// stream_output enabled on a platform but the agent's intelligence provider
// does not have streaming enabled. In that case stream_output silently
// does nothing, which is confusing.
func warnStreamOutputWithoutStreaming(cfg *config.Config) {
	for _, msg := range checkStreamOutputWithoutStreaming(cfg) {
		startupLog.Warnf("%s", msg)
	}
}

// checkStreamOutputWithoutStreaming returns warning messages for agents that
// have stream_output enabled but streaming disabled.
func checkStreamOutputWithoutStreaming(cfg *config.Config) []string {
	var warnings []string
	for _, acfg := range cfg.Agents {
		// Delegated backends (ccstream, opencode) stream unconditionally —
		// their Capabilities().Streaming is always true, regardless of the
		// [agent_loop].streaming config flag (which only gates the API path's
		// StreamHandler). Skip the check entirely for delegated agents.
		if acfg.IsDelegated() {
			continue
		}

		streaming := config.Resolve(cfg, acfg).Loop.Streaming
		if streaming {
			continue // provider streaming is on — no conflict
		}

		// Resolve effective stream_output for each platform.
		streamOutput := false
		if tg := acfg.Platform("telegram"); tg != nil && tg.Display.StreamOutput != nil {
			streamOutput = *tg.Display.StreamOutput
		}

		if streamOutput {
			warnings = append(warnings, "agent \""+acfg.ID+"\": stream_output is enabled for Telegram but streaming is disabled for the intelligence provider — streaming output will not work")
		}
	}
	return warnings
}
