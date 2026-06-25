// Package question holds the backend-agnostic core of the AskUserQuestion
// interaction: the question/option types, JSON parsing, display formatting,
// button-choice construction, answer resolution, and the sequential-answer
// accumulator. It has zero backend dependencies so it can be shared by:
//
//   - internal/delegator/ccstream — presents CC's AskUserQuestion tool calls
//     (blocking: the answer returns to CC as a control-response), and
//   - internal/tools (the foci-native `ask`/`foci_ask` tool) — async: the
//     answer batch is delivered later as a normal inbound user message.
//
// Both consume the same parse, format, choices, resolve, and merge so the two
// surfaces cannot drift. The choice-button data convention ("qa:<index>" plus a
// "qa:cancel" sentinel) also lives here, since both backends encode/decode it.
package question

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Question is a single question in an AskUserQuestion-shaped input.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect"`
	// ID is an optional opaque identifier supplied by the agent. It is never
	// shown to the user; it is preserved in the answer output (keyed under
	// "answers_by_id") so the agent can correlate answers deterministically
	// without matching on question text.
	ID string `json:"id,omitempty"`
}

// Option is one selectable option within a question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Choice is a presentation-agnostic button choice. Backends adapt it to their
// own button type (e.g. delegator.PromptChoice).
type Choice struct {
	Label string
	Data  string
}

// input is the wire shape of an AskUserQuestion tool input.
type input struct {
	Questions []Question `json:"questions"`
}

// CancelData is the button-data sentinel for the Cancel choice.
const CancelData = "qa:cancel"

// dataPrefix prefixes per-option button data: "qa:<index>".
const dataPrefix = "qa:"

// Parse parses an AskUserQuestion-shaped input into its questions. It returns
// an error for malformed JSON and an empty slice (nil error) for a well-formed
// input with no questions — callers decide how to treat "no questions". There
// is intentionally NO cap on the number of questions or options: the 4-item
// limit only ever lived in Claude Code's tool schema, never the parser, so the
// foci-native tool simply omits it and ccstream's (<=4) inputs are unaffected.
func Parse(raw json.RawMessage) ([]Question, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("parse questions: %w", err)
	}
	return in.Questions, nil
}

// FormatText formats a single question for platform display. index is 0-based;
// total is the number of questions in the sequence.
func FormatText(q *Question, index, total int) string {
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

	// List options with descriptions. With no options the question is
	// typed-answer-only, so hint that the user should reply by typing.
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
	} else {
		b.WriteString("\n\n_Reply with your answer._")
	}

	return b.String()
}

// OptionData returns the button-data token for the option at index i ("qa:<i>").
// The batched-app presenter uses it to build option-only choices (no Cancel),
// keeping the "qa:" convention in one place.
func OptionData(i int) string { return dataPrefix + strconv.Itoa(i) }

// Choices builds button choices for a question's options. Each option gets data
// "qa:<index>"; a Cancel button (CancelData) is appended.
func Choices(q *Question) []Choice {
	choices := make([]Choice, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		choices = append(choices, Choice{
			Label: opt.Label,
			Data:  dataPrefix + strconv.Itoa(i),
		})
	}
	choices = append(choices, Choice{Label: "Cancel", Data: CancelData})
	return choices
}

// ResolveAnswer maps a raw choice string to a concrete answer label. The choice
// is one of:
//   - "qa:<index>"  — a button click selecting the option at that index
//   - "qa:cancel"   — the Cancel button (cancelled=true, answer empty)
//   - any other text — a custom typed ("Other") answer, used verbatim
//
// It returns an error for a "qa:" button whose index is non-numeric or out of
// range. Both ccstream and foci_ask route button/typed responses through this
// so the data convention lives in exactly one place.
func ResolveAnswer(q *Question, choice string) (answer string, cancelled bool, err error) {
	if choice == CancelData {
		return "", true, nil
	}
	if strings.HasPrefix(choice, dataPrefix) {
		suffix := strings.TrimPrefix(choice, dataPrefix)
		idx, convErr := strconv.Atoi(suffix)
		if convErr != nil || idx < 0 || idx >= len(q.Options) {
			return "", false, fmt.Errorf("invalid question option index %q", suffix)
		}
		return q.Options[idx].Label, false, nil
	}
	// Custom typed text — use as-is.
	return choice, false, nil
}

// MergeAnswers merges an answers map into the original AskUserQuestion input
// JSON, producing the combined result both surfaces emit:
//
//	{"questions": [...original...], "answers": {"question text": "answer"}}
//
// Any extra top-level fields in originalInput are preserved.
func MergeAnswers(originalInput json.RawMessage, answers map[string]string) (json.RawMessage, error) {
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

// Accumulator drives a sequential, one-question-at-a-time answer flow. It tracks
// which question is current and collects answers keyed by question text. It is
// not safe for concurrent use; callers serialise access (ccstream under its
// perm mutex, foci_ask under its pending-ask registry lock).
type Accumulator struct {
	questions []Question
	idx       int
	answers   map[string]string
}

// NewAccumulator returns an Accumulator over the given questions, positioned at
// the first one.
func NewAccumulator(questions []Question) *Accumulator {
	return &Accumulator{
		questions: questions,
		answers:   make(map[string]string),
	}
}

// NewAccumulatorAt rebuilds an Accumulator positioned at idx with answers already
// collected — used to restore a persisted, partially-answered flow after a
// restart. A nil answers map is replaced with an empty one so Record stays safe.
func NewAccumulatorAt(questions []Question, idx int, answers map[string]string) *Accumulator {
	if answers == nil {
		answers = make(map[string]string)
	}
	return &Accumulator{
		questions: questions,
		idx:       idx,
		answers:   answers,
	}
}

// Current returns the question currently awaiting an answer, or nil if all
// questions have been answered (Done).
func (a *Accumulator) Current() *Question {
	if a.idx < 0 || a.idx >= len(a.questions) {
		return nil
	}
	return &a.questions[a.idx]
}

// Index returns the 0-based index of the current question.
func (a *Accumulator) Index() int { return a.idx }

// Total returns the number of questions.
func (a *Accumulator) Total() int { return len(a.questions) }

// Questions returns the underlying questions slice (for merging into the result).
func (a *Accumulator) Questions() []Question { return a.questions }

// Record stores an answer for the current question and advances to the next.
// It is a no-op if already Done.
func (a *Accumulator) Record(answer string) {
	q := a.Current()
	if q == nil {
		return
	}
	a.answers[q.Question] = answer
	a.idx++
}

// Done reports whether every question has been answered.
func (a *Accumulator) Done() bool { return a.idx >= len(a.questions) }

// Answers returns the accumulated answers keyed by question text.
func (a *Accumulator) Answers() map[string]string { return a.answers }
