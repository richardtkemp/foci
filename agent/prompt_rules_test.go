package agent

import (
	"testing"

	"foci/config"
)

func TestApplyPromptRulesQuestion(t *testing.T) {
	rules := CompilePromptRules([]config.PromptRule{
		{
			Find:    `(?is)^((why|when|what|how|where|who|did|does|do|is|are|was|were|can|could|would|should)\b.*\?\s*)$`,
			Replace: "Questions are just requests for information.\n-------\n$1",
		},
	})

	got := ApplyPromptRules(rules, "Why are you doing that?")
	want := "Questions are just requests for information.\n-------\nWhy are you doing that?"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyPromptRulesNoMatch(t *testing.T) {
	rules := CompilePromptRules([]config.PromptRule{
		{
			Find:    `(?i)^(why|when)\b`,
			Replace: "PREFIX: ",
		},
	})

	msg := "Deploy the thing"
	got := ApplyPromptRules(rules, msg)
	if got != msg {
		t.Errorf("non-matching message was modified: got %q", got)
	}
}

func TestApplyPromptRulesChaining(t *testing.T) {
	rules := CompilePromptRules([]config.PromptRule{
		{Find: `foo`, Replace: `bar`},
		{Find: `bar`, Replace: `baz`},
	})

	got := ApplyPromptRules(rules, "foo")
	if got != "baz" {
		t.Errorf("chaining failed: got %q, want %q", got, "baz")
	}
}

func TestApplyPromptRulesEmpty(t *testing.T) {
	got := ApplyPromptRules(nil, "hello")
	if got != "hello" {
		t.Errorf("nil rules modified message: got %q", got)
	}
}

func TestCompilePromptRulesInvalidRegex(t *testing.T) {
	rules := CompilePromptRules([]config.PromptRule{
		{Find: `[invalid`, Replace: "x"},
		{Find: `valid`, Replace: "y"},
	})
	if len(rules) != 1 {
		t.Errorf("expected 1 valid rule, got %d", len(rules))
	}
}
