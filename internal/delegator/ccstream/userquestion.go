package ccstream

import (
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/delegator"
	"foci/internal/question"
)

// ---------------------------------------------------------------------------
// AskUserQuestion types (matching CC's tool schema)
// ---------------------------------------------------------------------------

// userQuestion is an alias for the shared question type. The AskUserQuestion
// core (types, parse, format, choices, answer resolution, merge) lives in
// internal/question so it is shared with the foci-native `ask`/`foci_ask` tool;
// ccstream keeps only the CC-specific control-response wiring. The alias (not a
// new type) keeps existing call sites unchanged.
type userQuestion = question.Question

// askUserQuestionInput is the parsed input of the AskUserQuestion tool.
type askUserQuestionInput struct {
	Questions []userQuestion
}

// ---------------------------------------------------------------------------
// Detection and parsing
// ---------------------------------------------------------------------------

// parseAskUserQuestionInput parses and validates the AskUserQuestion tool
// input via the shared parser. Returns nil if the input is invalid or has no
// questions (CC's tool path treats both as "nothing to present").
func parseAskUserQuestionInput(raw json.RawMessage) *askUserQuestionInput {
	qs, err := question.Parse(raw)
	if err != nil || len(qs) == 0 {
		return nil
	}
	return &askUserQuestionInput{Questions: qs}
}

// ---------------------------------------------------------------------------
// Display formatting
// ---------------------------------------------------------------------------

// formatQuestionText formats a single question for platform display, delegating
// to the shared formatter. index is 0-based; total is the sequence length.
func formatQuestionText(q *userQuestion, index, total int) string {
	return question.FormatText(q, index, total)
}

// questionChoices creates button choices for a question's options, adapting the
// shared question.Choice values to delegator.PromptChoice for the prompt fn.
func questionChoices(q *userQuestion) []delegator.PromptChoice {
	shared := question.Choices(q)
	choices := make([]delegator.PromptChoice, len(shared))
	for i, c := range shared {
		choices[i] = delegator.PromptChoice{Label: c.Label, Data: c.Data}
	}
	return choices
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleUserQuestion is called when a can_use_tool request is for the
// AskUserQuestion tool. It parses the questions, stores a pending permission
// with question state, and presents the first question interactively.
func (b *Backend) handleUserQuestion(msg *PermissionRequest) {
	input := parseAskUserQuestionInput(msg.Request.Input)
	if input == nil {
		b.logger().Warnf("failed to parse AskUserQuestion input, falling back to allow: req_id=%s", msg.RequestID)
		// Can't present questions — auto-allow with empty answers so CC
		// gets an empty result rather than hanging.
		resp := &PermissionAllow{
			Behavior:               "allow",
			UpdatedInput:           msg.Request.Input,
			ToolUseID:              msg.Request.ToolUseID,
			DecisionClassification: "user_temporary",
		}
		_ = b.writer.SendControlResponse(msg.RequestID, resp)
		return
	}

	pp := &pendingPermission{
		requestID:     msg.RequestID,
		toolName:      msg.Request.ToolName,
		toolUseID:     msg.Request.ToolUseID,
		createdAt:     time.Now(),
		questions:     input.Questions,
		currentIndex:  0,
		answers:       make(map[string]string),
		originalInput: msg.Request.Input,
	}

	b.storePendingPerm(pp)
	b.outstanding.Register(msg.RequestID, delegator.OutstandingPermission)

	b.presentQuestion(pp, 0)
}

// presentQuestion sends question idx of pp as an interactive prompt. idx is
// passed in rather than read from pp.currentIndex, which an answer may be
// advancing concurrently under permMu. A question is the agent asking its own
// user: it goes through the question function when the gateway set one, else
// the permission function (#2275).
func (b *Backend) presentQuestion(pp *pendingPermission, idx int) {
	q := &pp.questions[idx]
	text := formatQuestionText(q, idx, len(pp.questions))
	summary := q.Header
	if summary == "" {
		summary = "Question"
	}
	choices := questionChoices(q)

	if fn := b.questionOrPermPromptFn(); fn != nil {
		fn(pp.requestID, text, summary, "", choices)
	} else {
		b.logger().Warnf("prompt fn nil for question req_id=%s, not displayed", pp.requestID)
	}
}

// ---------------------------------------------------------------------------
// Response handling
// ---------------------------------------------------------------------------

// RespondToQuestion handles a user's answer to a pending AskUserQuestion.
// choice is either:
//   - "qa:<index>" — a button click selecting option at that index
//   - "qa:cancel"  — user cancelled the question
//   - any other string — custom typed text answer
//
// For sequential multi-question flows, intermediate answers are accumulated
// and the next question is presented. The final answer sends PermissionAllow
// with all answers in updatedInput.
func (b *Backend) RespondToQuestion(requestID, choice string) error {
	pp, next, done, err := b.recordQuestionAnswer(requestID, choice)
	if err != nil {
		return err
	}
	if !done {
		b.presentQuestion(pp, next)
		return nil
	}

	// All questions answered, and this call claimed pp, so nothing else reads or
	// writes it any more: send the combined response.
	updatedInput, err := buildUpdatedInput(pp.originalInput, pp.answers)
	if err != nil {
		return fmt.Errorf("ccstream: build updatedInput: %w", err)
	}

	resp := &PermissionAllow{
		Behavior:               "allow",
		UpdatedInput:           updatedInput,
		ToolUseID:              pp.toolUseID,
		DecisionClassification: "user_temporary",
	}
	if err := b.writer.SendControlResponse(requestID, resp); err != nil {
		return err
	}

	b.outstanding.Resolve(requestID)
	return nil
}

// recordQuestionAnswer records choice as the answer to requestID's current
// question and advances it, all under permMu (#2070). A button click, a typed
// answer, a /stop cancel and CC's own control_cancel can arrive on different
// goroutines for the same request, so the lookup, the accumulator writes and —
// on the last answer — the claim (removal from pendingPerms) are one critical
// section. done=true means this call claimed pp and alone may respond to CC;
// otherwise next is the question to present.
func (b *Backend) recordQuestionAnswer(requestID, choice string) (pp *pendingPermission, next int, done bool, err error) {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	pp = b.pendingPerms[requestID]
	if pp == nil || pp.questions == nil {
		return nil, 0, false, fmt.Errorf("ccstream: no pending question with request ID %q", requestID)
	}

	q := &pp.questions[pp.currentIndex]
	answer, cancelled, err := question.ResolveAnswer(q, choice)
	if err != nil {
		return nil, 0, false, fmt.Errorf("ccstream: %w", err)
	}
	if cancelled {
		// The permission layer routes qa:cancel to CancelQuestion, so a cancel
		// reaching here is a stray; reject it rather than record it as an answer.
		return nil, 0, false, fmt.Errorf("ccstream: cancel is not a valid answer for request %q", requestID)
	}

	pp.answers[q.Question] = answer
	pp.currentIndex++
	b.logger().Debugf("answer %d/%d for req_id=%s: %q",
		pp.currentIndex, len(pp.questions), requestID, answer)

	if pp.currentIndex < len(pp.questions) {
		return pp, pp.currentIndex, false, nil
	}
	delete(b.pendingPerms, requestID)
	return pp, 0, true, nil
}

// CancelQuestion cancels a pending AskUserQuestion by sending PermissionDeny.
func (b *Backend) CancelQuestion(requestID string) error {
	pp, ok := b.removePendingPerm(requestID)
	if !ok {
		return fmt.Errorf("ccstream: no pending question with request ID %q", requestID)
	}

	b.logger().Debugf("cancelling question req_id=%s (had %d/%d answers)",
		requestID, len(pp.answers), len(pp.questions))

	resp := &PermissionDeny{
		Behavior:               "deny",
		Message:                "User cancelled question",
		Interrupt:              false,
		ToolUseID:              pp.toolUseID,
		DecisionClassification: "user_reject",
	}
	if err := b.writer.SendControlResponse(requestID, resp); err != nil {
		return err
	}

	b.outstanding.Resolve(requestID)
	return nil
}

// HasPendingQuestion returns the request ID of a pending AskUserQuestion,
// or empty string if none. Used by the text interception path to detect
// when typed text should be treated as a question answer.
func (b *Backend) HasPendingQuestion() string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	for _, pp := range b.pendingPerms {
		if pp.questions != nil {
			return pp.requestID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Response construction
// ---------------------------------------------------------------------------

// buildUpdatedInput merges the answers map into the original AskUserQuestion
// input JSON via the shared merge, producing the updatedInput for
// PermissionAllow. CC expects:
//
//	{"questions": [...original...], "answers": {"question text": "answer"}}
func buildUpdatedInput(originalInput json.RawMessage, answers map[string]string) (json.RawMessage, error) {
	return question.MergeAnswers(originalInput, answers)
}
