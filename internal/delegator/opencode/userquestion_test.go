//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/question"
)

// ---------------------------------------------------------------------------
// parseAskUserQuestionInput
// ---------------------------------------------------------------------------

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestParseAskUserQuestionInput_SingleQuestion(t *testing.T) {
// 	// Verifies parsing of a valid single-question input with multiple options.
// 	t.Parallel()
//
// 	raw := json.RawMessage(`{
// 		"questions": [{
// 			"question": "Which approach?",
// 			"header": "Approach",
// 			"options": [
// 				{"label": "Option A", "description": "First approach"},
// 				{"label": "Option B", "description": "Second approach"}
// 			],
// 			"multiSelect": false
// 		}]
// 	}`)
//
// 	got := parseAskUserQuestionInput(raw)
// 	if got == nil {
// 		t.Fatal("expected non-nil result")
// 	}
// 	if len(got.Questions) != 1 {
// 		t.Fatalf("got %d questions, want 1", len(got.Questions))
// 	}
// 	q := got.Questions[0]
// 	if q.Question != "Which approach?" {
// 		t.Errorf("question = %q, want %q", q.Question, "Which approach?")
// 	}
// 	if q.Header != "Approach" {
// 		t.Errorf("header = %q, want %q", q.Header, "Approach")
// 	}
// 	if len(q.Options) != 2 {
// 		t.Fatalf("got %d options, want 2", len(q.Options))
// 	}
// 	if q.Options[0].Label != "Option A" {
// 		t.Errorf("option[0].label = %q, want %q", q.Options[0].Label, "Option A")
// 	}
// 	if q.MultiSelect {
// 		t.Error("multiSelect should be false")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestParseAskUserQuestionInput_MultipleQuestions(t *testing.T) {
// 	// Verifies parsing of multiple questions in a single input.
// 	t.Parallel()
//
// 	raw := json.RawMessage(`{
// 		"questions": [
// 			{"question": "Q1?", "header": "H1", "options": [{"label": "A"}]},
// 			{"question": "Q2?", "header": "H2", "options": [{"label": "B"}]}
// 		]
// 	}`)
//
// 	got := parseAskUserQuestionInput(raw)
// 	if got == nil {
// 		t.Fatal("expected non-nil result")
// 	}
// 	if len(got.Questions) != 2 {
// 		t.Fatalf("got %d questions, want 2", len(got.Questions))
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestParseAskUserQuestionInput_InvalidJSON(t *testing.T) {
// 	// Verifies that malformed JSON returns nil.
// 	t.Parallel()
//
// 	got := parseAskUserQuestionInput(json.RawMessage(`{invalid`))
// 	if got != nil {
// 		t.Error("expected nil for invalid JSON")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestParseAskUserQuestionInput_EmptyQuestions(t *testing.T) {
// 	// Verifies that an empty questions array returns nil.
// 	t.Parallel()
//
// 	got := parseAskUserQuestionInput(json.RawMessage(`{"questions": []}`))
// 	if got != nil {
// 		t.Error("expected nil for empty questions")
// 	}
// }

// ---------------------------------------------------------------------------
// formatQuestionText
// ---------------------------------------------------------------------------

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestFormatQuestionText_Single(t *testing.T) {
// 	// Verifies formatting of a single question with header and options.
// 	t.Parallel()
//
// 	q := &userQuestion{
// 		Question: "Which library?",
// 		Header:   "Library",
// 		Options: []question.Option{
// 			{Label: "React", Description: "UI framework"},
// 			{Label: "Vue", Description: "Progressive framework"},
// 		},
// 	}
//
// 	text := formatQuestionText(q, 0, 1)
//
// 	if !strings.Contains(text, "**Library**") {
// 		t.Error("should contain bold header")
// 	}
// 	if !strings.Contains(text, "Which library?") {
// 		t.Error("should contain question text")
// 	}
// 	if !strings.Contains(text, "1. **React** — UI framework") {
// 		t.Errorf("should contain option 1 with description, got:\n%s", text)
// 	}
// 	if !strings.Contains(text, "2. **Vue** — Progressive framework") {
// 		t.Errorf("should contain option 2 with description, got:\n%s", text)
// 	}
// 	// Single question should NOT show numbering like "(1/1)".
// 	if strings.Contains(text, "1/1") {
// 		t.Error("single question should not show position numbering")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestFormatQuestionText_MultiQuestion(t *testing.T) {
// 	// Verifies that multi-question formatting includes position numbering.
// 	t.Parallel()
//
// 	q := &userQuestion{
// 		Question: "Pick one",
// 		Header:   "Step",
// 		Options:  []question.Option{{Label: "X"}},
// 	}
//
// 	text := formatQuestionText(q, 1, 3)
//
// 	if !strings.Contains(text, "**Step** (2/3)") {
// 		t.Errorf("should show header with position, got:\n%s", text)
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestFormatQuestionText_NoHeader(t *testing.T) {
// 	// Verifies fallback formatting when header is empty.
// 	t.Parallel()
//
// 	q := &userQuestion{
// 		Question: "Pick one",
// 		Options:  []question.Option{{Label: "X"}},
// 	}
//
// 	// Single question, no header — text should start directly with the question.
// 	text := formatQuestionText(q, 0, 1)
// 	if !strings.HasPrefix(text, "Pick one") {
// 		t.Errorf("should start with question text (no header line), got:\n%s", text)
// 	}
//
// 	// Multi-question, no header — fallback to "Question N/M".
// 	text = formatQuestionText(q, 0, 2)
// 	if !strings.Contains(text, "**Question 1/2**") {
// 		t.Errorf("should show fallback numbering, got:\n%s", text)
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestFormatQuestionText_NoDescription(t *testing.T) {
// 	// Verifies option without description shows just the label.
// 	t.Parallel()
//
// 	q := &userQuestion{
// 		Question: "Pick",
// 		Options:  []question.Option{{Label: "Opt"}},
// 	}
//
// 	text := formatQuestionText(q, 0, 1)
// 	if !strings.Contains(text, "1. **Opt**") {
// 		t.Errorf("should show option without dash, got:\n%s", text)
// 	}
// 	if strings.Contains(text, "—") {
// 		t.Error("should not contain description separator when no description")
// 	}
// }

// ---------------------------------------------------------------------------
// questionChoices
// ---------------------------------------------------------------------------

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestQuestionChoices(t *testing.T) {
// 	// Verifies that choices are generated for each option plus a Cancel button.
// 	t.Parallel()
//
// 	q := &userQuestion{
// 		Options: []question.Option{
// 			{Label: "Alpha"},
// 			{Label: "Beta"},
// 			{Label: "Gamma"},
// 		},
// 	}
//
// 	choices := questionChoices(q)
//
// 	// 3 options + 1 Cancel = 4 choices.
// 	if len(choices) != 4 {
// 		t.Fatalf("got %d choices, want 4", len(choices))
// 	}
//
// 	for i, opt := range q.Options {
// 		if choices[i].Label != opt.Label {
// 			t.Errorf("choice[%d].Label = %q, want %q", i, choices[i].Label, opt.Label)
// 		}
// 		wantData := "qa:" + string(rune('0'+i))
// 		if choices[i].Data != wantData {
// 			t.Errorf("choice[%d].Data = %q, want %q", i, choices[i].Data, wantData)
// 		}
// 	}
//
// 	cancel := choices[len(choices)-1]
// 	if cancel.Label != "Cancel" || cancel.Data != "qa:cancel" {
// 		t.Errorf("last choice = {%q, %q}, want {Cancel, qa:cancel}", cancel.Label, cancel.Data)
// 	}
// }

// ---------------------------------------------------------------------------
// buildUpdatedInput
// ---------------------------------------------------------------------------

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestBuildUpdatedInput(t *testing.T) {
// 	// Verifies that answers are merged into the original input JSON,
// 	// preserving existing fields.
// 	t.Parallel()
//
// 	original := json.RawMessage(`{"questions":[{"question":"Q1?"}],"extra":"kept"}`)
// 	answers := map[string]string{"Q1?": "Answer 1"}
//
// 	result, err := buildUpdatedInput(original, answers)
// 	if err != nil {
// 		t.Fatalf("buildUpdatedInput: %v", err)
// 	}
//
// 	var m map[string]json.RawMessage
// 	if err := json.Unmarshal(result, &m); err != nil {
// 		t.Fatalf("unmarshal result: %v", err)
// 	}
//
// 	// "questions" preserved.
// 	if _, ok := m["questions"]; !ok {
// 		t.Error("missing 'questions' key")
// 	}
// 	// "extra" preserved.
// 	if _, ok := m["extra"]; !ok {
// 		t.Error("missing 'extra' key")
// 	}
// 	// "answers" added.
// 	var gotAnswers map[string]string
// 	if err := json.Unmarshal(m["answers"], &gotAnswers); err != nil {
// 		t.Fatalf("unmarshal answers: %v", err)
// 	}
// 	if gotAnswers["Q1?"] != "Answer 1" {
// 		t.Errorf("answers[Q1?] = %q, want %q", gotAnswers["Q1?"], "Answer 1")
// 	}
// }

// ---------------------------------------------------------------------------
// End-to-end response flow
// ---------------------------------------------------------------------------

// testBackend creates a minimal Backend suitable for question tests.
// It captures control responses written to the Writer.
func testBackend(t *testing.T) (*Backend, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})
	b := &Backend{
		writer:       w,
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	return b, &buf
}

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_SingleQuestion(t *testing.T) {
// 	// Verifies end-to-end single-question flow: button click → PermissionAllow
// 	// with updatedInput containing the answer.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
//
// 	var promptCalls []struct {
// 		reqID   string
// 		text    string
// 		summary string
// 		choices []delegator.PromptChoice
// 	}
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		promptCalls = append(promptCalls, struct {
// 			reqID   string
// 			text    string
// 			summary string
// 			choices []delegator.PromptChoice
// 		}{reqID, text, summary, choices})
// 	}
//
// 	var clearedCount int
// 	b.SetOnPromptsCleared(func() { clearedCount++ })
//
// 	// Simulate CC sending AskUserQuestion.
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-q1",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-1",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red","description":"Warm"},{"label":"Blue","description":"Cool"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// Should have stored a pending perm and registered it in the registry.
// 	if !b.outstanding.Has("req-q1") {
// 		t.Error("outstanding registry should contain req-q1 after handleUserQuestion")
// 	}
// 	if len(promptCalls) != 1 {
// 		t.Fatalf("permPromptFn called %d times, want 1", len(promptCalls))
// 	}
// 	if promptCalls[0].summary != "Color" {
// 		t.Errorf("summary = %q, want %q", promptCalls[0].summary, "Color")
// 	}
//
// 	// User clicks "Red" (index 0).
// 	if err := b.RespondToQuestion("req-q1", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion: %v", err)
// 	}
//
// 	// Should have sent a control response.
// 	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
// 	if len(lines) != 1 {
// 		t.Fatalf("got %d response lines, want 1", len(lines))
// 	}
//
// 	var resp struct {
// 		Type     string `json:"type"`
// 		Response struct {
// 			Response struct {
// 				Behavior     string          `json:"behavior"`
// 				UpdatedInput json.RawMessage `json:"updatedInput"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
// 		t.Fatalf("unmarshal response: %v", err)
// 	}
// 	if resp.Response.Response.Behavior != "allow" {
// 		t.Errorf("behavior = %q, want %q", resp.Response.Response.Behavior, "allow")
// 	}
//
// 	// Check updatedInput has the answer.
// 	var updated map[string]json.RawMessage
// 	if err := json.Unmarshal(resp.Response.Response.UpdatedInput, &updated); err != nil {
// 		t.Fatalf("unmarshal updatedInput: %v", err)
// 	}
// 	var answers map[string]string
// 	if err := json.Unmarshal(updated["answers"], &answers); err != nil {
// 		t.Fatalf("unmarshal answers: %v", err)
// 	}
// 	if answers["Which color?"] != "Red" {
// 		t.Errorf("answers[Which color?] = %q, want %q", answers["Which color?"], "Red")
// 	}
//
// 	// Permission should be cleared.
// 	if clearedCount != 1 {
// 		t.Errorf("onPromptsCleared called %d times, want 1", clearedCount)
// 	}
// 	if b.PendingPermissions() != 0 {
// 		t.Errorf("pending permissions = %d, want 0", b.PendingPermissions())
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_SequentialMultiQuestion(t *testing.T) {
// 	// Verifies sequential multi-question flow: answer Q1 → prompt Q2 →
// 	// answer Q2 → PermissionAllow with both answers.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
//
// 	var promptCalls int
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		promptCalls++
// 	}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-multi",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-2",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q1?","header":"H1","options":[{"label":"A1"}]},{"question":"Q2?","header":"H2","options":[{"label":"A2"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	if promptCalls != 1 {
// 		t.Fatalf("after handleUserQuestion: prompt called %d times, want 1", promptCalls)
// 	}
//
// 	// Answer Q1.
// 	if err := b.RespondToQuestion("req-multi", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q1: %v", err)
// 	}
//
// 	// Should have prompted Q2 (no control response yet).
// 	if promptCalls != 2 {
// 		t.Fatalf("after Q1 answer: prompt called %d times, want 2", promptCalls)
// 	}
// 	if buf.Len() != 0 {
// 		t.Error("should not have sent control response after Q1")
// 	}
//
// 	// Answer Q2.
// 	if err := b.RespondToQuestion("req-multi", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q2: %v", err)
// 	}
//
// 	// Now control response should be sent with both answers.
// 	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
// 	if len(lines) != 1 {
// 		t.Fatalf("got %d response lines, want 1", len(lines))
// 	}
//
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				UpdatedInput json.RawMessage `json:"updatedInput"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	var updated map[string]json.RawMessage
// 	if err := json.Unmarshal(resp.Response.Response.UpdatedInput, &updated); err != nil {
// 		t.Fatalf("unmarshal updatedInput: %v", err)
// 	}
// 	var answers map[string]string
// 	if err := json.Unmarshal(updated["answers"], &answers); err != nil {
// 		t.Fatalf("unmarshal answers: %v", err)
// 	}
// 	if answers["Q1?"] != "A1" {
// 		t.Errorf("answers[Q1?] = %q, want %q", answers["Q1?"], "A1")
// 	}
// 	if answers["Q2?"] != "A2" {
// 		t.Errorf("answers[Q2?] = %q, want %q", answers["Q2?"], "A2")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_CustomText(t *testing.T) {
// 	// Verifies that a custom text answer (no "qa:" prefix) is stored
// 	// as the literal answer text.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-text",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-3",
// 			Input:     json.RawMessage(`{"questions":[{"question":"What next?","header":"Next","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// User types a custom answer.
// 	if err := b.RespondToQuestion("req-text", "my custom answer"); err != nil {
// 		t.Fatalf("RespondToQuestion: %v", err)
// 	}
//
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				UpdatedInput json.RawMessage `json:"updatedInput"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	var updated map[string]json.RawMessage
// 	if err := json.Unmarshal(resp.Response.Response.UpdatedInput, &updated); err != nil {
// 		t.Fatalf("unmarshal updatedInput: %v", err)
// 	}
// 	var answers map[string]string
// 	if err := json.Unmarshal(updated["answers"], &answers); err != nil {
// 		t.Fatalf("unmarshal answers: %v", err)
// 	}
// 	if answers["What next?"] != "my custom answer" {
// 		t.Errorf("answers = %q, want %q", answers["What next?"], "my custom answer")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestCancelQuestion(t *testing.T) {
// 	// Verifies that cancelling a question sends PermissionDeny and clears
// 	// the pending state.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-cancel",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-4",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	if err := b.CancelQuestion("req-cancel"); err != nil {
// 		t.Fatalf("CancelQuestion: %v", err)
// 	}
//
// 	// Should have sent a deny response.
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				Behavior string `json:"behavior"`
// 				Message  string `json:"message"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	if resp.Response.Response.Behavior != "deny" {
// 		t.Errorf("behavior = %q, want %q", resp.Response.Response.Behavior, "deny")
// 	}
//
// 	// Pending state should be cleared.
// 	if b.PendingPermissions() != 0 {
// 		t.Errorf("pending = %d, want 0", b.PendingPermissions())
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestCancelQuestion_MidSequence(t *testing.T) {
// 	// Verifies that cancelling mid-sequence discards accumulated answers.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-mid",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-5",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q1?","header":"H1","options":[{"label":"A"}]},{"question":"Q2?","header":"H2","options":[{"label":"B"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// Answer Q1.
// 	if err := b.RespondToQuestion("req-mid", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q1: %v", err)
// 	}
//
// 	// Cancel before answering Q2.
// 	if err := b.CancelQuestion("req-mid"); err != nil {
// 		t.Fatalf("CancelQuestion: %v", err)
// 	}
//
// 	// Should send deny, not allow with partial answers.
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				Behavior string `json:"behavior"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	if resp.Response.Response.Behavior != "deny" {
// 		t.Errorf("behavior = %q, want %q", resp.Response.Response.Behavior, "deny")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestHasPendingQuestion(t *testing.T) {
// 	// Verifies HasPendingQuestion returns the correct request ID.
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	// No pending question initially.
// 	if got := b.HasPendingQuestion(); got != "" {
// 		t.Errorf("HasPendingQuestion() = %q, want empty", got)
// 	}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-has",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-6",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	if got := b.HasPendingQuestion(); got != "req-has" {
// 		t.Errorf("HasPendingQuestion() = %q, want %q", got, "req-has")
// 	}
//
// 	// Regular permission should not be returned.
// 	b.storePendingPerm(&pendingPermission{requestID: "req-perm", toolName: "Bash"})
// 	if got := b.HasPendingQuestion(); got != "req-has" {
// 		t.Errorf("HasPendingQuestion() = %q, want %q (should ignore non-question)", got, "req-has")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_AlreadyAnswered(t *testing.T) {
// 	// Verifies graceful error when responding to a question that was already
// 	// resolved (e.g., button click after text answer consumed the question).
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-race",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-7",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// First answer succeeds.
// 	if err := b.RespondToQuestion("req-race", "qa:0"); err != nil {
// 		t.Fatalf("first RespondToQuestion: %v", err)
// 	}
//
// 	// Second answer should fail gracefully.
// 	err := b.RespondToQuestion("req-race", "qa:0")
// 	if err == nil {
// 		t.Error("expected error for already-answered question")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_InvalidOptionIndex(t *testing.T) {
// 	// Verifies error on out-of-range option index.
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-bad-idx",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-8",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"Only"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	err := b.RespondToQuestion("req-bad-idx", "qa:5")
// 	if err == nil {
// 		t.Error("expected error for out-of-range index")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestHandleToolRequest_DetectsAskUserQuestion(t *testing.T) {
// 	// Verifies that handleToolRequest routes AskUserQuestion to the question
// 	// handler instead of the standard permission flow.
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
//
// 	var questionHandled bool
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		// If this is the question handler, text should NOT contain "Permission Required".
// 		if !strings.Contains(text, "Permission Required") {
// 			questionHandled = true
// 		}
// 	}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-detect",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-9",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Pick?","header":"Pick","options":[{"label":"X"}]}]}`),
// 		},
// 	}
// 	b.handleToolRequest(msg)
//
// 	if !questionHandled {
// 		t.Error("AskUserQuestion should be handled by question handler, not permission handler")
// 	}
//
// 	// Verify it stored as a question (has questions field).
// 	pp := b.getPendingPerm("req-detect")
// 	if pp == nil {
// 		t.Fatal("expected pending perm to be stored")
// 	}
// 	if pp.questions == nil {
// 		t.Error("expected questions field to be set")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestHandleToolRequest_RegularPermission(t *testing.T) {
// 	// Verifies that non-AskUserQuestion tools go through the standard
// 	// permission flow (shows "Permission Required").
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
//
// 	var gotText string
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		gotText = text
// 	}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-perm",
// 		Request: PermissionRequestPayload{
// 			Subtype:     "can_use_tool",
// 			ToolName:    "Bash",
// 			ToolUseID:   "tu-10",
// 			Input:       json.RawMessage(`{"command":"ls"}`),
// 			DisplayName: "Bash",
// 		},
// 	}
// 	b.handleToolRequest(msg)
//
// 	if !strings.Contains(gotText, "Permission Required") {
// 		t.Errorf("regular permission should show 'Permission Required', got:\n%s", gotText)
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_ConcurrentAccess(t *testing.T) {
// 	// Verifies that concurrent RespondToQuestion/HasPendingQuestion calls
// 	// don't race (detected by -race flag).
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-conc",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-11",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	var wg sync.WaitGroup
// 	wg.Add(2)
//
// 	go func() {
// 		defer wg.Done()
// 		_ = b.HasPendingQuestion()
// 	}()
// 	go func() {
// 		defer wg.Done()
// 		_ = b.RespondToQuestion("req-conc", "qa:0")
// 	}()
//
// 	wg.Wait()
// }
