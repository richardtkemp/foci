package app

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/fap"
	"foci/internal/question"
	"foci/internal/tools"
)

// Late answers and dead prompts (#2080/#1868, Dick's 2026-09-28 ruling).
//
// When an answer arrives for a prompt foci no longer has registered — the ask
// expired, was cancelled, or was dropped across a restart — two things happen:
//
//  1. The answer is DELIVERED to the agent as a marked late answer (which
//     question, when it was asked, what was chosen). Nothing is dropped.
//  2. The prompt DISAPPEARS from the app (fap.InteractiveRemove). No "expired"
//     state, no toast.
//
// The ask layer has forgotten the ask by then, so the question has to come from
// what the app transport still holds: the `interactive` frame it sent, kept in
// the durable frame store (30 days by default) and the binding's in-memory replay
// buffer. No new persistence — the frame IS the record of what the user saw.

// promptHistory is what the app transport still knows about one prompt.
type promptHistory struct {
	asked   *fap.Interactive // the frame that put the question on screen; nil = aged out
	askedAt time.Time        // when that frame was sent; zero if unknown
	// answered: the prompt's last recorded resolution was a real answer (a "✅"
	// edit, or a Done progress edit with no cancel). A second answer to it — a
	// double tap, another device submitting the same form — is a duplicate, not
	// a late answer, and must neither reach the agent twice nor remove the
	// prompt's answered marker.
	answered bool
	// removed: the last recorded resolution already removed the prompt from the
	// app, so a further removal frame would be noise in the reliable stream.
	removed bool
}

// promptHistory collects every frame b carried for promptID, from the durable
// store and the in-memory buffer (the store write is async, so a resolution sent
// milliseconds ago may only be in the buffer), and reads them in seq order.
func (h *Hub) promptHistory(b *convBinding, promptID string) promptHistory {
	needle := `"promptId":"` + promptID + `"`
	type fr struct {
		wire   string
		sentMs int64
	}
	bySeq := map[int64]fr{}
	for _, f := range h.frames.PromptFrames(b.convID, needle) {
		bySeq[f.seq] = fr{f.wire, f.sentMs}
	}
	b.mu.Lock()
	for _, f := range b.buffer {
		if strings.Contains(f.wire, needle) {
			bySeq[f.seq] = fr{f.wire, f.sent.UnixMilli()}
		}
	}
	b.mu.Unlock()
	seqs := make([]int64, 0, len(bySeq))
	for s := range bySeq {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	var ph promptHistory
	for _, s := range seqs {
		f := bySeq[s]
		var env fap.Envelope
		if json.Unmarshal([]byte(f.wire), &env) != nil {
			continue
		}
		switch env.T {
		case fap.TypeInteractive:
			if ph.asked != nil {
				continue // a re-present of the same prompt: keep the original ask time
			}
			var in fap.Interactive
			if json.Unmarshal(env.D, &in) == nil && in.PromptID == promptID {
				ph.asked = &in
				ph.askedAt = time.UnixMilli(f.sentMs)
			}
		default:
			if r, ok := resolutionOf(f.wire, promptID); ok {
				ph.answered, ph.removed = r.answered, r.removed
			}
		}
	}
	return ph
}

// lastResolution classifies promptID by its newest resolution frame only: the
// in-memory buffer walked newest-first (it stops at the first hit, normally at
// the tail), else the durable store. Used where the question itself is not
// needed, so a burst of dead submits does not rescan the whole buffer each time.
func (h *Hub) lastResolution(b *convBinding, promptID string) promptHistory {
	needle := `"promptId":"` + promptID + `"`
	b.mu.Lock()
	for i := len(b.buffer) - 1; i >= 0; i-- {
		if !strings.Contains(b.buffer[i].wire, needle) {
			continue
		}
		if ph, ok := resolutionOf(b.buffer[i].wire, promptID); ok {
			b.mu.Unlock()
			return ph
		}
	}
	b.mu.Unlock()
	frames := h.frames.PromptFrames(b.convID, needle)
	for i := len(frames) - 1; i >= 0; i-- {
		if ph, ok := resolutionOf(frames[i].wire, promptID); ok {
			return ph
		}
	}
	return promptHistory{}
}

// resolutionOf reads one frame as a resolution of promptID: ok is false for any
// other frame (the ask itself, another prompt's frame, a mid-form progress edit).
func resolutionOf(wire, promptID string) (promptHistory, bool) {
	var env fap.Envelope
	if json.Unmarshal([]byte(wire), &env) != nil {
		return promptHistory{}, false
	}
	switch env.T {
	case fap.TypeInteractiveEdit:
		var e fap.InteractiveEdit
		if json.Unmarshal(env.D, &e) == nil && e.PromptID == promptID {
			return promptHistory{answered: strings.HasPrefix(e.Text, "✅")}, true
		}
	case fap.TypeInteractiveProgressEdit:
		var e fap.InteractiveProgressEdit
		if json.Unmarshal(env.D, &e) == nil && e.PromptID == promptID && e.Done {
			return promptHistory{answered: !containsCancel(e.Answers)}, true
		}
	case fap.TypeInteractiveRemove:
		var e fap.InteractiveRemove
		if json.Unmarshal(env.D, &e) == nil && e.PromptID == promptID {
			return promptHistory{removed: true}, true
		}
	}
	return promptHistory{}, false
}

func containsCancel(answers []string) bool {
	for _, a := range answers {
		if a == question.CancelData {
			return true
		}
	}
	return false
}

// lateQuestions rebuilds the ask-layer questions from the frame that showed
// them. A batched form carries them structured; a single prompt carries one
// question as text plus buttons, the last of which is the ask's Cancel.
func lateQuestions(in *fap.Interactive) []question.Question {
	if in == nil {
		return nil
	}
	if len(in.Questions) > 0 {
		qs := make([]question.Question, len(in.Questions))
		for i, fq := range in.Questions {
			qs[i] = question.Question{Question: fq.Text, Header: fq.Header, Options: optionsOf(fq.Choices)}
		}
		return qs
	}
	return []question.Question{{Question: in.Text, Header: in.Header, Options: optionsOf(singleOptions(in.Choices))}}
}

// singleOptions drops a single prompt's trailing Cancel button, which
// question.Choices appends after the options.
func singleOptions(choices []fap.Choice) []fap.Choice {
	if n := len(choices); n > 0 && choices[n-1].Label == "Cancel" {
		return choices[:n-1]
	}
	return choices
}

func optionsOf(choices []fap.Choice) []question.Option {
	opts := make([]question.Option, len(choices))
	for i, c := range choices {
		opts[i] = question.Option{Label: c.Label, Description: c.Description}
	}
	return opts
}

// singleRawAnswer maps a single prompt's click token "<promptID>:<index>" to the
// ask layer's raw answer ("qa:<index>", or the cancel sentinel for the trailing
// Cancel button). ok is false when the token does not parse.
func singleRawAnswer(in *fap.Interactive, data string) (string, bool) {
	cut := strings.LastIndexByte(data, ':')
	if cut < 0 {
		return "", false
	}
	idx, err := strconv.Atoi(data[cut+1:])
	if err != nil || idx < 0 {
		return "", false
	}
	if in != nil && idx >= len(singleOptions(in.Choices)) && idx < len(in.Choices) {
		return question.CancelData, true
	}
	return question.OptionData(idx), true
}

// resolveDeadPrompt handles an answer for a prompt nobody has registered any more.
// Unless the prompt was already answered (a duplicate submit), the answer goes to
// the agent as a late answer and the prompt is removed from every client. raw is
// the ask-layer answer set; nil when the click could not be decoded, in which
// case the prompt is still removed. A duplicate of a recorded answer is left as it is.
func (h *Hub) resolveDeadPrompt(b *convBinding, promptID string, ph promptHistory, raw []string) {
	if ph.answered {
		appLog.Infof("answer for already-answered prompt %s (conv=%s) — duplicate, ignored", promptID, b.convID)
		return
	}
	h.deletePrompt(promptID)
	if !ph.removed {
		b.send(fap.InteractiveRemove{ConversationID: b.convID, PromptID: promptID})
	}

	delivered := false
	if conn := h.PrimaryBot(b.agentID); conn != nil && conn.routeLateAnswer != nil && raw != nil {
		delivered = conn.routeLateAnswer(tools.LateAnswer{
			PromptID:   promptID,
			SessionKey: b.sessionKey,
			AskedAt:    ph.askedAt,
			Questions:  lateQuestions(ph.asked),
			Answers:    raw,
		})
	}
	appLog.Infof("answer for unregistered prompt %s (conv=%s): removed from the app; late answer delivered to the agent=%v",
		promptID, b.convID, delivered)
}
