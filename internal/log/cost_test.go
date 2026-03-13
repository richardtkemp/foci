package log

import (
	"testing"
)

func TestCalculateCost(t *testing.T) {
	// 1M input tokens on Haiku = $1.00
	cost := CalculateCost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("1M input haiku = %f, want 1.0", cost)
	}

	// 1M output tokens on Haiku = $5.00
	cost = CalculateCost("claude-haiku-4-5", 0, 1_000_000, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M output haiku = %f, want 5.0", cost)
	}

	// 1M cache read on Haiku = $0.10
	cost = CalculateCost("claude-haiku-4-5", 0, 0, 1_000_000, 0)
	if cost != 0.1 {
		t.Errorf("1M cache read haiku = %f, want 0.1", cost)
	}

	// 1M cache write on Haiku = $1.25
	cost = CalculateCost("claude-haiku-4-5", 0, 0, 0, 1_000_000)
	if cost != 1.25 {
		t.Errorf("1M cache write haiku = %f, want 1.25", cost)
	}

	// Mixed: realistic request
	cost = CalculateCost("claude-haiku-4-5", 500, 100, 2000, 1000)
	expected := 500.0/1e6*1.0 + 100.0/1e6*5.0 + 2000.0/1e6*0.1 + 1000.0/1e6*1.25
	if cost != expected {
		t.Errorf("mixed cost = %f, want %f", cost, expected)
	}

	// Unknown model uses haiku pricing
	cost = CalculateCost("unknown-model", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("unknown model = %f, want 1.0 (haiku fallback)", cost)
	}
}

func TestCalculateCostOpus(t *testing.T) {
	// Verifies Opus input pricing is $15/M tokens.
	cost := CalculateCost("claude-opus-4-6", 1_000_000, 0, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M input opus = %f, want 15.0", cost)
	}
}

func TestCalculateCostGemini(t *testing.T) {
	// 1M input on gemini-2.5-flash = $0.15
	cost := CalculateCost("gemini-2.5-flash", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("1M input flash = %f, want 0.15", cost)
	}

	// 1M output on gemini-2.5-pro = $10.00
	cost = CalculateCost("gemini-2.5-pro", 0, 1_000_000, 0, 0)
	if cost != 10.0 {
		t.Errorf("1M output pro = %f, want 10.0", cost)
	}

	// Unknown gemini model uses flash pricing
	cost = CalculateCost("gemini-3.0-ultra", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("unknown gemini = %f, want 0.15 (flash fallback)", cost)
	}
}

func TestCalculateCostOpenAIFallback(t *testing.T) {
	// 1M input tokens on unknown OpenAI model = $5.00
	cost := CalculateCost("gpt-5-turbo", 1_000_000, 0, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M input unknown openai = %f, want 5.0", cost)
	}

	// 1M output tokens on unknown OpenAI model = $15.00
	cost = CalculateCost("o4-mini", 0, 1_000_000, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M output unknown openai = %f, want 15.0", cost)
	}
}
