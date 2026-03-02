package agent

import (
	"testing"

	"foci/config"
)

func TestApplyTransformsQuestion(t *testing.T) {
	rules := CompileTransforms([]config.MessageTransform{
		{
			Find:    `(?is)^((why|when|what|how|where|who|did|does|do|is|are|was|were|can|could|would|should)\b.*\?\s*)$`,
			Replace: "Questions are just requests for information.\n-------\n$1",
		},
	})

	got := ApplyTransforms(rules, "Why are you doing that?")
	want := "Questions are just requests for information.\n-------\nWhy are you doing that?"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyTransformsNoMatch(t *testing.T) {
	rules := CompileTransforms([]config.MessageTransform{
		{
			Find:    `(?i)^(why|when)\b`,
			Replace: "PREFIX: ",
		},
	})

	msg := "Deploy the thing"
	got := ApplyTransforms(rules, msg)
	if got != msg {
		t.Errorf("non-matching message was modified: got %q", got)
	}
}

func TestApplyTransformsChaining(t *testing.T) {
	rules := CompileTransforms([]config.MessageTransform{
		{Find: `foo`, Replace: `bar`},
		{Find: `bar`, Replace: `baz`},
	})

	got := ApplyTransforms(rules, "foo")
	if got != "baz" {
		t.Errorf("chaining failed: got %q, want %q", got, "baz")
	}
}

func TestApplyTransformsEmpty(t *testing.T) {
	got := ApplyTransforms(nil, "hello")
	if got != "hello" {
		t.Errorf("nil rules modified message: got %q", got)
	}
}

func TestCompileTransformsInvalidRegex(t *testing.T) {
	rules := CompileTransforms([]config.MessageTransform{
		{Find: `[invalid`, Replace: "x"},
		{Find: `valid`, Replace: "y"},
	})
	if len(rules) != 1 {
		t.Errorf("expected 1 valid rule, got %d", len(rules))
	}
}
