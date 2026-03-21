package main

import (
	"foci/internal/config"
	"testing"
)

// TestCheckStreamOutputWithoutStreaming verifies that warnings are produced
// when stream_output is enabled for Telegram but streaming is disabled,
// and that no warnings appear when the config is consistent.
// Covers: global default, per-agent overrides, per-agent streaming override.
func TestCheckStreamOutputWithoutStreaming(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name        string
		cfg         *config.Config
		wantWarning bool
	}{
		{
			name: "agent platform stream_output on, streaming off — warns",
			cfg: &config.Config{
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &trueVal},
					}},
				}},
			},
			wantWarning: true,
		},
		{
			name: "stream_output on, streaming on via global defaults — no warning",
			cfg: &config.Config{
				Defaults: config.DefaultsConfig{
					DisplayConfig: config.DisplayConfig{Streaming: &trueVal},
				},
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &trueVal},
					}},
				}},
			},
			wantWarning: false,
		},
		{
			name: "stream_output off — no warning",
			cfg: &config.Config{
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &falseVal},
					}},
				}},
			},
			wantWarning: false,
		},
		{
			name: "per-agent stream_output explicitly off — no warning",
			cfg: &config.Config{
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &falseVal},
					}},
				}},
			},
			wantWarning: false,
		},
		{
			name: "per-agent streaming override on — no warning",
			cfg: &config.Config{
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Defaults: config.AgentDefaultsOverride{
						DisplayConfig: config.DisplayConfig{Streaming: &trueVal},
					},
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &trueVal},
					}},
				}},
			},
			wantWarning: false,
		},
		{
			name: "global streaming on, per-agent override off — warns",
			cfg: &config.Config{
				Defaults: config.DefaultsConfig{
					DisplayConfig: config.DisplayConfig{Streaming: &trueVal},
				},
				Agents: []config.AgentConfig{{
					ID: "bot1",
					Defaults: config.AgentDefaultsOverride{
						DisplayConfig: config.DisplayConfig{Streaming: &falseVal},
					},
					Platforms: []config.PlatformConfig{{
						ID:            "telegram",
						DisplayConfig: config.DisplayConfig{StreamOutput: &trueVal},
					}},
				}},
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
