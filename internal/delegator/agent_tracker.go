package delegator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SubagentTracker tracks spawned subagent (CC Agent tool) calls and emits an
// aggregated status detail string via OnStatus. Both the tmux and ccstream
// backends compose this to report running/complete subagent status.
//
// A "subagent" is a CC Agent-tool spawn — distinct from a foci "agent" (a
// personality that talks to another via send_to_session).
//
// All methods are safe for concurrent use.
type SubagentTracker struct {
	mu      sync.Mutex
	pending []TrackedSubagent
	start   time.Time

	// OnStatus is called when the subagent status changes. The argument is a
	// plain DETAIL string: the running-subagent descriptions (comma-joined) while
	// any are running, or "" when none are. It maps cleanly onto the app's
	// setSubagentDetail. Set by the backend before any tracking begins.
	OnStatus func(detail string)
}

// TrackedSubagent is a pending Agent tool_use call.
type TrackedSubagent struct {
	ID          string // tool_use ID
	Description string // short description from Agent tool input
	added       time.Time
}

// agentMaxAge bounds how long a spawn stays tracked without a completion
// signal. The tracker now survives turn boundaries (a background subagent
// outlives the turn that spawned it), so a missed completion — RemoveOne is
// FIFO, not ID-matched, in ccstream — can no longer be swept by a per-turn
// clear; this prune is the backstop so Pending() can't stay stuck > 0. Set
// well beyond any real subagent's runtime.
const agentMaxAge = 30 * time.Minute

// Add registers a new subagent spawn. Duplicate IDs are silently ignored
// (handles --include-partial-messages replays in ccstream).
func (t *SubagentTracker) Add(id, description string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	for _, ag := range t.pending {
		if ag.ID == id {
			return
		}
	}
	t.pending = append(t.pending, TrackedSubagent{ID: id, Description: description, added: time.Now()})
	if t.start.IsZero() {
		t.start = time.Now()
	}
	t.notify()
}

// Description returns the tracked description for a subagent id, or "" if the id
// isn't tracked (best-effort label for the app's collapsed trace entry).
func (t *SubagentTracker) Description(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ag := range t.pending {
		if ag.ID == id {
			return ag.Description
		}
	}
	return ""
}

// pruneLocked drops agents older than agentMaxAge. Caller holds mu; it does
// not notify (callers already do around their own mutations).
func (t *SubagentTracker) pruneLocked() {
	if len(t.pending) == 0 {
		return
	}
	cutoff := time.Now().Add(-agentMaxAge)
	kept := t.pending[:0]
	for _, ag := range t.pending {
		if ag.added.Before(cutoff) {
			continue
		}
		kept = append(kept, ag)
	}
	t.pending = kept
}

// Remove marks an agent as completed by its tool_use ID.
// Returns true if the agent was found and removed.
func (t *SubagentTracker) Remove(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, ag := range t.pending {
		if ag.ID == id {
			t.pending = append(t.pending[:i], t.pending[i+1:]...)
			t.notify()
			return true
		}
	}
	return false
}

// RemoveOne removes one pending agent (first in list). Used when exact
// ID matching isn't possible (e.g. ccstream task_notification events
// don't carry the original tool_use ID).
// Returns true if an agent was removed.
func (t *SubagentTracker) RemoveOne() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	if len(t.pending) == 0 {
		return false
	}
	t.pending = t.pending[1:]
	t.notify()
	return true
}

// ClearAll removes all pending agents and fires a completion
// notification if any were pending. Safe to call when already empty.
func (t *SubagentTracker) ClearAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return
	}
	t.pending = nil
	t.notify()
}

// Pending returns the number of agents currently tracked.
func (t *SubagentTracker) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	before := len(t.pending)
	t.pruneLocked()
	if len(t.pending) != before {
		t.notify()
	}
	return len(t.pending)
}

// notify sends the current status DETAIL via OnStatus. Must be called with mu
// held. The detail is the comma-joined running-subagent descriptions (or a
// count when none carry a description), or "" when nothing is running — so it
// maps directly onto the app's setSubagentDetail. Human-facing wording
// ("🔄 …running" / "✅ …complete") is the caller's concern.
func (t *SubagentTracker) notify() {
	if t.OnStatus == nil {
		return
	}
	if len(t.pending) == 0 {
		t.start = time.Time{}
		t.OnStatus("")
		return
	}
	var descs []string
	for _, ag := range t.pending {
		if ag.Description != "" {
			descs = append(descs, ag.Description)
		}
	}
	if len(descs) > 0 {
		t.OnStatus(strings.Join(descs, ", "))
	} else {
		t.OnStatus(fmt.Sprintf("%d subagent(s) running", len(t.pending)))
	}
}

// ExtractAgentDescription parses the "description" field from an Agent
// tool_use input JSON payload.
func ExtractAgentDescription(raw json.RawMessage) string {
	var input struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Description
	}
	return ""
}
