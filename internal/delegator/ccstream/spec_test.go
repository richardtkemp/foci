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

// TestSpec_PostCompactNeedsToolCall pins the #3093 declaration: a text-only
// first reply after a CC compaction leaves the cached tail unreadable to every
// later request (#3086), so the resume nudge must ask for a tool call. Dropping
// the field silently brings back a ~50k-token cache rewrite per compaction.
func TestSpec_PostCompactNeedsToolCall(t *testing.T) {
	if !spec.PostCompactNeedsToolCall {
		t.Error("claude-code must set PostCompactNeedsToolCall (#3086, #3093)")
	}
}
