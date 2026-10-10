package ccstream

import (
	"testing"

	"foci/internal/delegator"
)

// TestSpec_QuestionPromptCapability pins the #2275 declaration: ccstream
// presents its own questions (AskUserQuestion, elicitation) through
// SetQuestionPromptFunc, so the gateway can route them to the asking
// session's chat instead of the owner's default chat. Dropping the row
// silently reverts every backend question into an approval prompt.
func TestSpec_QuestionPromptCapability(t *testing.T) {
	if !spec.Supports(delegator.CapQuestionPrompt) {
		t.Error("claude-code must declare CapQuestionPrompt Yes: AskUserQuestion and elicitation go through SetQuestionPromptFunc (#2275)")
	}
}
