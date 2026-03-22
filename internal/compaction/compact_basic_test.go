package compaction

import (
	"testing"

	"foci/internal/modelinfo"
	"foci/internal/provider"
)

func TestEstimateTokens(t *testing.T) {
	// Verifies the character-based token heuristic: non-empty messages produce
	// a positive count, nil/empty messages produce exactly zero.
	tests := []struct {
		name string
		msgs []provider.Message
		min  int
	}{
		{
			name: "small conversation",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("hello world")},
				{Role: "assistant", Content: provider.TextContent("hi there!")},
			},
			min: 2,
		},
		{
			name: "nil messages",
			msgs: nil,
			min:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.msgs)
			if tt.min == 0 && got != 0 {
				t.Errorf("estimateTokens = %d, want 0", got)
			} else if got < tt.min {
				t.Errorf("estimateTokens = %d, expected >= %d", got, tt.min)
			}
		})
	}
}

func TestContextLimit(t *testing.T) {
	// Verifies Compactor.ContextLimit falls back to modelinfo registry
	// defaults when no ModelMetaFn is set: Claude (200k), Gemini 2.x (1M),
	// Gemini 1.5 (2M), unknown models (200k default).
	c := NewCompactor(nil, 0.8)
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 200_000},
		{"claude-sonnet-4-5", 200_000},
		{"claude-opus-4-6", 200_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"gemini-1.5-flash", 2_000_000},
		{"gemini-2.0-pro", 1_000_000},
		{"gemini-other", 1_000_000},
		{"unknown-model", 200_000},
		{"gpt-4", 200_000},
		{"", 200_000},
	}
	for _, tt := range tests {
		name := tt.model
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			if got := c.ContextLimit(tt.model); got != tt.want {
				t.Errorf("ContextLimit(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestContextLimitWithModelMetaFn(t *testing.T) {
	// Verifies that ModelMetaFn overrides the registry default when it
	// returns a non-zero ContextWindow, and falls back otherwise.
	c := NewCompactor(nil, 0.8)
	c.ModelMetaFn = func(model string) modelinfo.ModelMeta {
		if model == "openrouter/z-ai/glm-5-turbo" {
			return modelinfo.ModelMeta{ContextWindow: 202_000}
		}
		return modelinfo.ModelMeta{}
	}

	// Config-defined override
	if got := c.ContextLimit("openrouter/z-ai/glm-5-turbo"); got != 202_000 {
		t.Errorf("ContextLimit(glm-5-turbo) = %d, want 202000", got)
	}
	// Falls back to registry
	if got := c.ContextLimit("claude-opus-4-6"); got != 200_000 {
		t.Errorf("ContextLimit(claude-opus-4-6) = %d, want 200000", got)
	}
}

func TestShouldCompact(t *testing.T) {
	// Verifies threshold-based compaction decisions: under threshold (false),
	// over threshold (true), cache tokens count, exact boundary uses strict >,
	// and small conversations with estimated tokens don't trigger compaction.
	tests := []struct {
		name  string
		msgs  []provider.Message
		usage *provider.Usage
		want  bool
	}{
		{
			name:  "under threshold",
			usage: &provider.Usage{InputTokens: 100_000},
			want:  false,
		},
		{
			name:  "over threshold",
			usage: &provider.Usage{InputTokens: 170_000},
			want:  true,
		},
		{
			name:  "cache tokens count toward total",
			usage: &provider.Usage{InputTokens: 50_000, CacheReadInputTokens: 120_000},
			want:  true,
		},
		{
			name: "small conversation estimated",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("hello")},
				{Role: "assistant", Content: provider.TextContent("hi")},
			},
			want: false,
		},
		{
			name:  "exact threshold not triggered",
			usage: &provider.Usage{InputTokens: 160_000},
			want:  false,
		},
		{
			name:  "one over threshold",
			usage: &provider.Usage{InputTokens: 160_001},
			want:  true,
		},
	}
	c := NewCompactor(nil, 0.8)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.ShouldCompact("claude-haiku-4-5", "test/session", tt.msgs, tt.usage); got != tt.want {
				t.Errorf("ShouldCompact = %v, want %v", got, tt.want)
			}
		})
	}
}
