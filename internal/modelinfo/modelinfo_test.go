package modelinfo

import (
	"math"
	"testing"
)

func TestContextWindow(t *testing.T) {
	// Proves exact matches return correct values, family fallbacks work
	// (gemini-1.5 → 2M, unknown gemini → 1M, claude/unknown → 200k),
	// and developer prefixes are stripped.
	t.Parallel()
	tests := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-6", 1_000_000},
		{"anthropic/claude-opus-4-6", 1_000_000},
		{"claude-code-tmux", 1_000_000},
		{"claude-code", 1_000_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},       // family fallback
		{"gemini-99-future", 1_000_000},      // unknown gemini fallback
		{"google/gemini-99-future", 1_000_000},
		{"claude-sonnet-99", 200_000},        // unknown claude fallback
		{"totally-unknown", 200_000},         // default fallback
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	// Proves registered models return exact capabilities, unregistered claude
	// variants fall back by family, and non-claude models return all false.
	t.Parallel()
	tests := []struct {
		model                          string
		wantEffort, wantThinking, wantSpeed bool
	}{
		{"claude-opus-4-6", true, true, true},
		{"anthropic/claude-opus-4-6", true, true, true},
		{"claude-sonnet-4-5", true, true, false},
		{"claude-haiku-4-5", false, false, false},
		{"CLAUDE-HAIKU-4-5", false, false, false},
		{"claude-sonnet-99", true, true, false},   // unknown sonnet fallback
		{"claude-opus-99", true, true, true},      // unknown opus fallback
		{"claude-haiku-99", false, false, false},   // unknown haiku fallback
		{"gemini-2.5-flash", false, false, false},
		{"gpt-4o", false, false, false},
		{"unknown", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			e, th, s := Capabilities(tt.model)
			if e != tt.wantEffort || th != tt.wantThinking || s != tt.wantSpeed {
				t.Errorf("Capabilities(%q) = (%v,%v,%v), want (%v,%v,%v)",
					tt.model, e, th, s, tt.wantEffort, tt.wantThinking, tt.wantSpeed)
			}
		})
	}
}

func TestCost(t *testing.T) {
	// Proves exact model pricing, family fallbacks (unknown gemini → flash,
	// OpenAI → approximate, unknown → haiku), and correct arithmetic.
	t.Parallel()

	// 1M tokens of each type for easy verification
	m := 1_000_000

	tests := []struct {
		model string
		want  float64 // expected cost for 1M of each token type
	}{
		{"claude-haiku-4-5", 1.00 + 5.00 + 0.10 + 1.25},
		{"claude-opus-4-6", 15.00 + 75.00 + 1.50 + 18.75},
		{"gemini-2.5-pro", 1.25 + 10.00 + 0.315 + 0},
		{"gemini-99-future", 0.15 + 0.60 + 0.0375 + 0}, // falls back to flash
		{"gpt-4o", 5.00 + 15.00 + 0 + 0},                // OpenAI fallback
		{"totally-unknown", 1.00 + 5.00 + 0.10 + 1.25},   // haiku fallback
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := Cost(tt.model, m, m, m, m)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("Cost(%q, 1M each) = %.4f, want %.4f", tt.model, got, tt.want)
			}
		})
	}
}

func TestIsOpenAI(t *testing.T) {
	// Proves OpenAI model detection for gpt-, o1/o3/o4, chatgpt- prefixes,
	// and correct rejection of non-OpenAI models. Also handles developer prefixes.
	t.Parallel()
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4", true},
		{"gpt-3.5-turbo", true},
		{"o1", true},
		{"o3", true},
		{"o4", true},
		{"chatgpt-4", true},
		{"openai/gpt-4o", true},
		{"claude-3-sonnet", false},
		{"gemini-2-flash", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := IsOpenAI(tt.model)
			if got != tt.want {
				t.Errorf("IsOpenAI(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
