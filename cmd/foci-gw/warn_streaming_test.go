package main

import (
	"foci/internal/config"
	"testing"
)

// TestCheckStreamOutputWithoutStreaming verifies that warnings are produced
// when stream_output is enabled for Telegram but the provider's streaming is
// disabled, and that no warnings appear when the config is consistent.
// Covers: global telegram default, per-agent overrides, per-agent streaming
// override, and use_sdk=false forcing streaming off.
func TestCheckStreamOutputWithoutStreaming(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name        string
		cfg         *config.Config
		wantWarning bool
	}{
		{
			name: "global stream_output on, streaming off — warns",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: false},
				Telegram:  config.TelegramConfig{StreamOutput: true},
				Agents:    []config.AgentConfig{{ID: "bot1"}},
			},
			wantWarning: true,
		},
		{
			name: "per-agent stream_output on, streaming off — warns",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: false},
				Telegram:  config.TelegramConfig{StreamOutput: false},
				Agents: []config.AgentConfig{{
					ID:        "bot1",
					Platforms: &config.PlatformsConfig{Telegram: &config.TelegramPlatformConfig{StreamOutput: &trueVal}},
				}},
			},
			wantWarning: true,
		},
		{
			name: "stream_output on, streaming on — no warning",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: true},
				Telegram:  config.TelegramConfig{StreamOutput: true},
				Agents:    []config.AgentConfig{{ID: "bot1"}},
			},
			wantWarning: false,
		},
		{
			name: "stream_output off — no warning",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: false},
				Telegram:  config.TelegramConfig{StreamOutput: false},
				Agents:    []config.AgentConfig{{ID: "bot1"}},
			},
			wantWarning: false,
		},
		{
			name: "per-agent stream_output explicitly off overrides global on — no warning",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: false},
				Telegram:  config.TelegramConfig{StreamOutput: true},
				Agents: []config.AgentConfig{{
					ID:        "bot1",
					Platforms: &config.PlatformsConfig{Telegram: &config.TelegramPlatformConfig{StreamOutput: &falseVal}},
				}},
			},
			wantWarning: false,
		},
		{
			name: "per-agent streaming override on — no warning",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: true, Streaming: false},
				Telegram:  config.TelegramConfig{StreamOutput: true},
				Agents: []config.AgentConfig{{
					ID:        "bot1",
					Streaming: &trueVal,
				}},
			},
			wantWarning: false,
		},
		{
			name: "use_sdk false forces streaming off — warns",
			cfg: &config.Config{
				Anthropic: config.AnthropicConfig{UseSDK: false, Streaming: true},
				Telegram:  config.TelegramConfig{StreamOutput: true},
				Agents:    []config.AgentConfig{{ID: "bot1"}},
			},
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := checkStreamOutputWithoutStreaming(tt.cfg)
			if tt.wantWarning && len(warnings) == 0 {
				t.Error("expected a warning but got none")
			}
			if !tt.wantWarning && len(warnings) > 0 {
				t.Errorf("unexpected warning: %s", warnings[0])
			}
		})
	}
}
