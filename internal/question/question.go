// Package question holds the backend-agnostic core of the AskUserQuestion
// interaction: the question/option/quiz types, JSON parsing, display
// formatting, button-choice construction, answer resolution (including the
// quiz typed-number rule), quiz validation/verdict/reveal, and the
// sequential-answer accumulator. It has zero backend dependencies so it can be
// shared by:
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
	// Quiz, when set, marks the question as a QUIZ question — one with a known
	// answer the client hides until the user reveals it (#1316). nil = a plain
	// question, presented and answered exactly as before. Validated by
	// ValidateQuiz at ask-call time.
	Quiz *Quiz `json:"quiz,omitempty"`
}

// Quiz is the quiz data of a question the agent marked as having a known
// answer (#1316): what the prompt withholds and the reveal shows. Exactly one
// arm is set — Correct (option questions) or ModelAnswer (free-recall, i.e.
// option-less questions); Explanation is optional and shown on reveal.
type Quiz struct {
	// Correct holds 0-based indexes into Question.Options — the agent authored
	// that array, so the index space is the array's, NOT the 1-based display
	// numbering FormatText renders (the reveal prints "Answer: 2. Oslo" for
	// correct=[1]). Documented wherever the input is described because the
	// off-by-one is the obvious mistake.
	Correct []int `json:"correct,omitempty"`
	// ModelAnswer is the known good answer of a free-recall (option-less)
	// question, shown on reveal; the agent judges the user's own text.
	ModelAnswer string `json:"answer,omitempty"`
	// Explanation is shown alongside the answer on reveal.
	Explanation string `json:"explanation,omitempty"`
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

// ExpiredData is the payload foci's expiry sweeps feed an unanswered foci `ask`
// in place of CancelData, so the agent is told the ask EXPIRED rather than that
// the user cancelled it (#2091). No button carries it: it is server-internal,
// consumed by the ask layer before ResolveAnswer (which would reject it as a
// non-numeric "qa:" index).
const ExpiredData = "qa:expired"

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

// ValidateQuiz checks the quiz data of one question (#1316): nil (no error) for
// a non-quiz question; a quiz question needs exactly one arm — Correct indexes
// on an option question, or a ModelAnswer on a free-recall (option-less)
// question — and every index must name one of the question's options exactly
// once. The ask tool wraps the returned error with the question's position and
// text, so each message states the rule it broke.
func ValidateQuiz(q *Question) error {
	if q.Quiz == nil {
		return nil
	}
	if len(q.Options) > 0 {
		if q.Quiz.ModelAnswer != "" {
			return fmt.Errorf("quiz answer (model answer) is only for a free-recall question with no options — set \"correct\" option indexes instead")
		}
		if len(q.Quiz.Correct) == 0 {
			return fmt.Errorf("quiz question with options needs \"correct\" — the 0-based indexes of its correct option(s)")
		}
		seen := make(map[int]bool, len(q.Quiz.Correct))
		for _, idx := range q.Quiz.Correct {
			if idx < 0 || idx >= len(q.Options) {
				return fmt.Errorf("quiz correct option 0-based index %d is out of range: question has %d options", idx, len(q.Options))
			}
			if seen[idx] {
				return fmt.Errorf("quiz correct option 0-based index %d is repeated", idx)
			}
			seen[idx] = true
		}
		return nil
	}
	if len(q.Quiz.Correct) > 0 {
		return fmt.Errorf("quiz correct option indexes are only for a question with options — give a model \"answer\" instead")
	}
	if q.Quiz.ModelAnswer == "" {
		return fmt.Errorf("quiz question needs the correct option(s) (\"correct\") or a model answer (\"answer\")")
	}
	return nil
}

// QuizVerdict reports whether answer — exactly as delivered to the agent (an
// option's label, a typed number already resolved to its label, or the user's
// typed text) — equals one of q's correct option labels. ok is false when
// there is no mechanical verdict to give: q is not a quiz option question, or
// the answer is empty. Free-recall quiz questions never get a verdict (the
// agent judges the text); non-quiz questions are untouched by this.
func QuizVerdict(q *Question, answer string) (correct, ok bool) {
	if q.Quiz == nil || len(q.Options) == 0 || answer == "" {
		return false, false
	}
	for _, idx := range q.Quiz.Correct {
		if idx >= 0 && idx < len(q.Options) && q.Options[idx].Label == answer {
			return true, true
		}
	}
	return false, true
}

// FormatReveal renders the post-answer reveal for quiz question q: the correct
// option(s) with their 1-based display numbers, or the model answer, plus the
// explanation and — for an option question — whether the user's answer was
// correct. PLAIN TEXT ONLY, no markdown markers: chat notifications are sent
// without a parse mode (e.g. Telegram's notification path), so any marker
// would reach the user literally, and `<`/`>`/`&` in the question text pass
// through as-is (#1316 round-3 finding F1). q must carry Quiz data; answer is
// the delivered answer (may be empty — then no verdict line).
func FormatReveal(q *Question, answer string) string {
	lines := []string{q.Question}
	if len(q.Options) > 0 {
		correct := make([]string, 0, len(q.Quiz.Correct))
		for _, idx := range q.Quiz.Correct {
			if idx >= 0 && idx < len(q.Options) {
				correct = append(correct, fmt.Sprintf("%d. %s", idx+1, q.Options[idx].Label))
			}
		}
		lines = append(lines, "Answer: "+strings.Join(correct, ", "))
		if right, ok := QuizVerdict(q, answer); ok {
			if right {
				lines = append(lines, "✅ Correct.")
			} else {
				lines = append(lines, fmt.Sprintf("❌ Incorrect — you answered %s.", answer))
			}
		}
	} else {
		lines = append(lines, "Model answer: "+q.Quiz.ModelAnswer)
	}
	if q.Quiz.Explanation != "" {
		lines = append(lines, "Explanation: "+q.Quiz.Explanation)
	}
	return strings.Join(lines, "\n")
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
	// A typed reply that is just a number selects that option by its DISPLAYED
	// number on a quiz option question (#1316): FormatText renders options as
	// "N. label", so "2" is easy entry for the second option. The number always
	// means the position — even when a label is itself numeric — and anything
	// out of range or non-numeric falls through to the verbatim behaviour
	// below, as does every typed number on a NON-quiz question.
	if q.Quiz != nil && len(q.Options) > 0 {
		if n, convErr := strconv.Atoi(strings.TrimSpace(choice)); convErr == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1].Label, false, nil
		}
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
