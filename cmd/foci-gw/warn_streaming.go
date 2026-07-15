package main

import (
	"foci/internal/config"
	"foci/internal/delegator"
)

// warnStreamOutputWithoutStreaming warns at startup if any agent has
// stream_output enabled on a platform but the agent's intelligence provider
// does not support streaming. In that case stream_output silently
// does nothing, which is confusing.
func warnStreamOutputWithoutStreaming(cfg *config.Config) {
	for _, msg := range checkStreamOutputWithoutStreaming(cfg) {
		startupLog.Warnf("%s", msg)
	}
}

// checkStreamOutputWithoutStreaming returns warning messages for agents that
// have stream_output enabled but streaming is not supported.
func checkStreamOutputWithoutStreaming(cfg *config.Config) []string {
	var warnings []string
	for _, acfg := range cfg.Agents {
		// For delegated backends, streaming capability is a property of the
		// backend type (queried via CapabilitiesForBackend), not the
		// [agent_loop].streaming config flag which only gates the API path.
		var streaming bool
		if acfg.IsDelegated() {
			streaming = delegator.CapabilitiesForBackend(acfg.Backend).Streaming
		} else {
			streaming = config.Resolve(cfg, acfg).Loop.Streaming
		}
		if streaming {
			continue
		}

		// Resolve effective stream_output for each platform.
		streamOutput := false
		if tg := acfg.Platform("telegram"); tg != nil && tg.Display.StreamOutput != nil {
			streamOutput = *tg.Display.StreamOutput
		}

		if streamOutput {
			warnings = append(warnings, "agent \""+acfg.ID+"\": stream_output is enabled for Telegram but streaming is not supported by the intelligence provider — streaming output will not work")
		}
	}
	return warnings
}
