package askgw

import (
	"strings"
	"testing"
)

// #2740: an askgw prompt is always shown with one button per option, so the
// text must not repeat the options as a numbered list ("1. **Approve** —
// Execute the command as root"). Dick: "the text does not need to say
// approve/deny. The buttons already say that."
func TestAskgwQuestionTextOmitsOptionList(t *testing.T) {
	q := &AskQuestion{
		Key:      "decision",
		Header:   "Sudo",
		Question: "Run as root?\n```\nls /root\n```\n\nUser: rich",
		Options: []AskOption{
			{Label: "Approve", Description: "Execute the command as root"},
			{Label: "Deny", Description: "Reject the request"},
		},
	}
	got := askgwQuestionText(q, 0, 1)
	for _, banned := range []string{"Approve", "Deny", "Execute the command as root", "Reject the request", "1. "} {
		if strings.Contains(got, banned) {
			t.Errorf("text repeats the options (%q):\n%s", banned, got)
		}
	}
	if !strings.Contains(got, q.Question) {
		t.Errorf("text lost the question:\n%s", got)
	}
	if !strings.Contains(got, "**Sudo**") {
		t.Errorf("text lost the header:\n%s", got)
	}
}

// A multi-question ask keeps its "(N/M)" header.
func TestAskgwQuestionTextKeepsMultiQuestionHeader(t *testing.T) {
	q := &AskQuestion{Key: "q", Header: "Second", Question: "Pick again:", Options: []AskOption{{Label: "C"}, {Label: "D"}}}
	got := askgwQuestionText(q, 1, 2)
	if !strings.HasPrefix(got, "**Second** (2/2)\n\nPick again:") || strings.Contains(got, "**C**") {
		t.Errorf("got:\n%s", got)
	}
}

// With no options there are no buttons to carry the answer, so the typed-
// answer hint stays.
func TestAskgwQuestionTextNoOptionsKeepsReplyHint(t *testing.T) {
	q := &AskQuestion{Key: "q", Question: "Name?"}
	if got := askgwQuestionText(q, 0, 1); !strings.Contains(got, "Reply with your answer") {
		t.Errorf("got:\n%s", got)
	}
}
