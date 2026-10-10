package opencode

import (
	"testing"

	"foci/internal/delegator"
)

// TestSpec_QuestionPromptCapability pins the #2275 declaration: opencode
// presents its question-type permissions through SetQuestionPromptFunc, so
// the gateway can route them to the asking session's chat instead of the
// owner's default chat. Dropping the row silently reverts every backend
// question into an approval prompt.
func TestSpec_QuestionPromptCapability(t *testing.T) {
	if !spec.Supports(delegator.CapQuestionPrompt) {
		t.Error("opencode must declare CapQuestionPrompt Yes: question-type permissions go through SetQuestionPromptFunc (#2275)")
	}
}
