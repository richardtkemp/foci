package modelinfo

import (
	"testing"
)

func TestCalculateCost(t *testing.T) {
	// 1M input tokens on Haiku = $1.00
	cost := Cost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("1M input haiku = %f, want 1.0", cost)
	}

	// 1M output tokens on Haiku = $5.00
	cost = Cost("claude-haiku-4-5", 0, 1_000_000, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M output haiku = %f, want 5.0", cost)
	}

	// 1M cache read on Haiku = $0.10
	cost = Cost("claude-haiku-4-5", 0, 0, 1_000_000, 0)
	if cost != 0.1 {
		t.Errorf("1M cache read haiku = %f, want 0.1", cost)
	}

	// 1M cache write on Haiku = $1.25
	cost = Cost("claude-haiku-4-5", 0, 0, 0, 1_000_000)
	if cost != 1.25 {
		t.Errorf("1M cache write haiku = %f, want 1.25", cost)
	}

	// Mixed: realistic request
	cost = Cost("claude-haiku-4-5", 500, 100, 2000, 1000)
	expected := 500.0/1e6*1.0 + 100.0/1e6*5.0 + 2000.0/1e6*0.1 + 1000.0/1e6*1.25
	if cost != expected {
		t.Errorf("mixed cost = %f, want %f", cost, expected)
	}

	// Unknown model uses haiku pricing
	cost = Cost("unknown-model", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("unknown model = %f, want 1.0 (haiku fallback)", cost)
	}
}

func TestCalculateCostOpus(t *testing.T) {
	// Verifies Opus input pricing is $15/M tokens.
	cost := Cost("claude-opus-4-6", 1_000_000, 0, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M input opus = %f, want 15.0", cost)
	}
}

func TestCalculateCostByFamily(t *testing.T) {
	cases := []struct {
		model string
		want  float64 // 1M input cost
	}{
		{"claude-opus-4-8", 15.0},            // newer opus, not in registry → opus family
		{"anthropic/claude-opus-4-9", 15.0},  // provider prefix + future version
		{"claude-sonnet-4-6", 3.0},           // newer sonnet → sonnet family
		{"claude-fable-6", 10.0},             // newer fable → fable family
		{"claude-haiku-9-9", 1.0},            // newer haiku → haiku family
		{"gemini-3.0-pro", 0.15},             // unknown gemini → flash family
	}
	for _, c := range cases {
		if got := Cost(c.model, 1_000_000, 0, 0, 0); got != c.want {
			t.Errorf("Cost(%q) 1M input = %f, want %f", c.model, got, c.want)
		}
	}
}

func TestCalculateCostGemini(t *testing.T) {
	// 1M input on gemini-2.5-flash = $0.15
	cost := Cost("gemini-2.5-flash", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("1M input flash = %f, want 0.15", cost)
	}

	// 1M output on gemini-2.5-pro = $10.00
	cost = Cost("gemini-2.5-pro", 0, 1_000_000, 0, 0)
	if cost != 10.0 {
		t.Errorf("1M output pro = %f, want 10.0", cost)
	}

	// Unknown gemini model uses flash pricing
	cost = Cost("gemini-3.0-ultra", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("unknown gemini = %f, want 0.15 (flash fallback)", cost)
	}
}

func TestCalculateCostOpenAIFallback(t *testing.T) {
	// 1M input tokens on unknown OpenAI model = $5.00
	cost := Cost("gpt-5-turbo", 1_000_000, 0, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M input unknown openai = %f, want 5.0", cost)
	}

	// 1M output tokens on unknown OpenAI model = $15.00
	cost = Cost("o4-mini", 0, 1_000_000, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M output unknown openai = %f, want 15.0", cost)
	}
}
