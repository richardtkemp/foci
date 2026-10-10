package codex

import (
	"testing"

	"foci/internal/delegator"
)

// TestSpec_QuestionPromptCapability pins the #2275 declaration: codex has no
// question prompts, so the row must stay an EXPLAINED No — the "why not"
// text is what docs/BACKENDS.md renders for the user. Dropping the row leaves
// the capability undeclared instead of deliberately unsupported.
func TestSpec_QuestionPromptCapability(t *testing.T) {
	if spec.Supports(delegator.CapQuestionPrompt) {
		t.Fatal("codex must declare CapQuestionPrompt No: the app-server has no question prompts")
	}
	if spec.Caps[delegator.CapQuestionPrompt].Reason == "" {
		t.Error("codex CapQuestionPrompt No must carry its reason (rendered in docs/BACKENDS.md)")
	}
}
