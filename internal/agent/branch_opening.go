package agent

import (
	"strings"

	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
)

// Branch openings (#2617): a depth leaf's fork ends on the parent's fan-out
// call, a synthetic result for it and the leaf's question as the agent's own
// message, so no turn runs at creation and the leaf's first turn is the user's
// reply. Each fork path writes that end its own way (the CC transcript, codex
// thread/inject_items, the API branch file); where it cannot, the question
// rides on the first user message through the orientation (Fallback).

// forkOpening converts a branch opening to the backend's form.
func forkOpening(o *session.BranchOpening) *delegator.ForkOpening {
	if o == nil {
		return nil
	}
	return &delegator.ForkOpening{CallMarker: o.CallMarker, ToolResult: o.ToolResult, AgentText: o.AgentText}
}

// withOpeningFallback returns opts with the opening's Fallback added to the
// orientation when the opening could not be written into the fork.
func withOpeningFallback(opts session.BranchOptions, written bool) session.BranchOptions {
	if opts.Opening == nil || written || opts.Opening.Fallback == "" {
		return opts
	}
	if opts.OrientationTemplate == "" {
		opts.OrientationTemplate = opts.Opening.Fallback
	} else {
		opts.OrientationTemplate += "\n\n" + opts.Opening.Fallback
	}
	return opts
}

// apiOpeningParentResult answers, in an API branch, a call of the fan-out's
// assistant message other than the fan-out itself: it ran in the parent.
const apiOpeningParentResult = "[fork boundary] This call belongs to the parent session: it ran there after this branch was taken, and its result is not visible here. Do not re-run it from this branch."

// apiOpening builds what an API branch is given after the parent's on-disk
// history: the running turn's unsaved messages (its user message through the
// assistant message whose tool call made the fork), one tool_result message
// answering every call of that assistant message, and the agent text. Nil when
// there is no opening, no unsaved turn, or its last message holds no call
// matching the marker (an approved deep fan-out runs after its turn ended).
func apiOpening(unsaved []provider.Message, o *session.BranchOpening) []provider.Message {
	if o == nil || o.CallMarker == "" || len(unsaved) == 0 {
		return nil
	}
	last := unsaved[len(unsaved)-1]
	if last.Role != "assistant" {
		return nil
	}
	var results []provider.ContentBlock
	matched := false
	for _, b := range last.Content {
		if b.Type != "tool_use" {
			continue
		}
		text := apiOpeningParentResult
		if !matched && (strings.Contains(b.Name, o.CallMarker) || strings.Contains(string(b.Input), o.CallMarker)) {
			matched = true
			text = o.ToolResult
			if text == "" {
				text = "Done."
			}
		}
		results = append(results, provider.ToolResultBlock(b.ID, text, false))
	}
	if !matched {
		return nil
	}
	out := append([]provider.Message(nil), unsaved...)
	return append(out,
		provider.Message{Role: "user", Content: results},
		provider.Message{Role: "assistant", Content: provider.TextContent(o.AgentText)},
	)
}
