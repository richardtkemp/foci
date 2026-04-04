package ccstream

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// AskUserQuestion types (matching CC's tool schema)
// ---------------------------------------------------------------------------

// userQuestion is a single question in an AskUserQuestion tool invocation.
type userQuestion struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []questionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

// questionOption is one selectable option within a question.
type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askUserQuestionInput is the parsed input of the AskUserQuestion tool.
type askUserQuestionInput struct {
	Questions []userQuestion `json:"questions"`
}

// ---------------------------------------------------------------------------
// Detection and parsing
// ---------------------------------------------------------------------------

// parseAskUserQuestionInput parses and validates the AskUserQuestion tool
// input. Returns nil if the input is invalid or has no questions.
func parseAskUserQuestionInput(raw json.RawMessage) *askUserQuestionInput {
	var input askUserQuestionInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil
	}
	if len(input.Questions) == 0 {
		return nil
	}
	return &input
}

// ---------------------------------------------------------------------------
// Display formatting
// ---------------------------------------------------------------------------

// formatQuestionText formats a single question for platform display.
// index is 0-based; total is the number of questions in the sequence.
func formatQuestionText(q *userQuestion, index, total int) string {
	var b strings.Builder

	// Header line: bold header or "Question N/M" for multi-question.
	if total > 1 {
		if q.Header != "" {
			b.WriteString(fmt.Sprintf("**%s** (%d/%d)\n\n", q.Header, index+1, total))
		} else {
			b.WriteString(fmt.Sprintf("**Question %d/%d**\n\n", index+1, total))
		}
	} else if q.Header != "" {
		b.WriteString(fmt.Sprintf("**%s**\n\n", q.Header))
	}

	b.WriteString(q.Question)

	// List options with descriptions.
	if len(q.Options) > 0 {
		b.WriteString("\n")
		for i, opt := range q.Options {
			b.WriteString("\n")
			if opt.Description != "" {
				b.WriteString(fmt.Sprintf("%d. **%s** — %s", i+1, opt.Label, opt.Description))
			} else {
				b.WriteString(fmt.Sprintf("%d. **%s**", i+1, opt.Label))
			}
		}
	}

	return b.String()
}

// questionChoices creates button choices for a question's options.
// Each option gets data "qa:<index>"; a Cancel button is appended.
func questionChoices(q *userQuestion) []delegator.PromptChoice {
	choices := make([]delegator.PromptChoice, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		choices = append(choices, delegator.PromptChoice{
			Label: opt.Label,
			Data:  "qa:" + strconv.Itoa(i),
		})
	}
	choices = append(choices, delegator.PromptChoice{
		Label: "Cancel",
		Data:  "qa:cancel",
	})
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
		log.Warnf("ccstream/question", "failed to parse AskUserQuestion input, falling back to allow: req_id=%s", msg.RequestID)
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

	if b.onPermPending != nil {
		b.onPermPending()
	}

	b.presentCurrentQuestion(pp)
}

// presentCurrentQuestion sends the current question as an interactive prompt.
func (b *Backend) presentCurrentQuestion(pp *pendingPermission) {
	q := &pp.questions[pp.currentIndex]
	text := formatQuestionText(q, pp.currentIndex, len(pp.questions))
	summary := q.Header
	if summary == "" {
		summary = "Question"
	}
	choices := questionChoices(q)

	if b.permPromptFn != nil {
		b.permPromptFn(pp.requestID, text, summary, choices)
	} else if b.replyFunc != nil {
		b.replyFunc(text)
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
	pp := b.getPendingPerm(requestID)
	if pp == nil || pp.questions == nil {
		return fmt.Errorf("ccstream: no pending question with request ID %q", requestID)
	}

	q := &pp.questions[pp.currentIndex]
	var answer string

	if strings.HasPrefix(choice, "qa:") {
		suffix := strings.TrimPrefix(choice, "qa:")
		idx, err := strconv.Atoi(suffix)
		if err != nil || idx < 0 || idx >= len(q.Options) {
			return fmt.Errorf("ccstream: invalid question option index %q", suffix)
		}
		answer = q.Options[idx].Label
	} else {
		// Custom typed text — use as-is.
		answer = choice
	}

	pp.answers[q.Question] = answer
	pp.currentIndex++

	log.Debugf("ccstream/question", "answer %d/%d for req_id=%s: %q",
		pp.currentIndex, len(pp.questions), requestID, answer)

	// More questions to present?
	if pp.currentIndex < len(pp.questions) {
		b.presentCurrentQuestion(pp)
		return nil
	}

	// All questions answered — send the combined response.
	updatedInput, err := buildUpdatedInput(pp.originalInput, pp.answers)
	if err != nil {
		return fmt.Errorf("ccstream: build updatedInput: %w", err)
	}

	_, _, noMorePending := b.removePendingPerm(requestID)

	resp := &PermissionAllow{
		Behavior:               "allow",
		UpdatedInput:           updatedInput,
		ToolUseID:              pp.toolUseID,
		DecisionClassification: "user_temporary",
	}
	if err := b.writer.SendControlResponse(requestID, resp); err != nil {
		return err
	}

	if noMorePending && b.onPermCleared != nil {
		b.onPermCleared()
	}
	return nil
}

// CancelQuestion cancels a pending AskUserQuestion by sending PermissionDeny.
func (b *Backend) CancelQuestion(requestID string) error {
	pp, ok, noMorePending := b.removePendingPerm(requestID)
	if !ok {
		return fmt.Errorf("ccstream: no pending question with request ID %q", requestID)
	}

	log.Debugf("ccstream/question", "cancelling question req_id=%s (had %d/%d answers)",
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

	if noMorePending && b.onPermCleared != nil {
		b.onPermCleared()
	}
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
// input JSON, producing the updatedInput for PermissionAllow. CC expects:
//
//	{"questions": [...original...], "answers": {"question text": "answer"}}
func buildUpdatedInput(originalInput json.RawMessage, answers map[string]string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(originalInput, &m); err != nil {
		return nil, fmt.Errorf("unmarshal original input: %w", err)
	}

	answersJSON, err := json.Marshal(answers)
	if err != nil {
		return nil, fmt.Errorf("marshal answers: %w", err)
	}
	m["answers"] = json.RawMessage(answersJSON)

	return json.Marshal(m)
}
