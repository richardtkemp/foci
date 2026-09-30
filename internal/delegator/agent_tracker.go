package delegator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
)

var (
	delegatedLog = log.NewComponentLogger("delegated")
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

	// MaxAge bounds how long a spawn lingers without a completion signal before
	// pruneLocked drops it. Zero → defaultAgentMaxAge. Set from config at backend
	// Start (ccstream); the prune is the unwedge backstop for the pending-work
	// gate (spec §4) — a task whose completion notification is missed can't hold
	// system injects forever.
	MaxAge time.Duration

	// OnStatus is called when the subagent status changes. The argument is a
	// plain DETAIL string: the running-subagent descriptions (comma-joined) while
	// any are running, or "" when none are. It maps cleanly onto the app's
	// setSubagentDetail. Set by the backend before any tracking begins.
	OnStatus func(detail string)

	// OnRunning is called on every status change, just before OnStatus, with
	// the structured list of what is running (nil when nothing is). It carries
	// what the detail string flattens away: each entry's id (the subagent's
	// group key, so the app can open its view), kind, model and start time
	// (#2138). Optional; nil = not reported.
	OnRunning func(running []RunningSubagent)
}

// Kinds of tracked background work (RunningSubagent.Kind).
const (
	// SubagentKindAgent is an Agent-tool subagent spawn.
	SubagentKindAgent = "agent"
	// SubagentKindCommand is a run_in_background shell command. It has no
	// subagent view, but it holds the pending-work gate like a subagent does.
	SubagentKindCommand = "command"
)

// TrackedSubagent is a pending Agent tool_use call.
type TrackedSubagent struct {
	ID           string // tool_use ID
	Description  string // short description from Agent tool input
	Kind         string // SubagentKindAgent or SubagentKindCommand ("" reads as agent)
	Model        string // requested model alias, replaced by the model actually seen; "" = unknown
	SubagentType string // the Agent tool's subagent_type, "" if none
	Command      string // a background command's full command line, "" for an agent
	added        time.Time
}

// RunningSubagent is one entry of the structured running list passed to
// OnRunning: a snapshot of a TrackedSubagent with its start time exported.
type RunningSubagent struct {
	ID           string
	Description  string
	Kind         string
	Model        string
	SubagentType string
	Command      string
	Started      time.Time
}

// defaultAgentMaxAge bounds how long a spawn stays tracked without a completion
// signal. The tracker now survives turn boundaries (a background subagent
// outlives the turn that spawned it), so a missed completion — RemoveOne is
// FIFO, not ID-matched, in ccstream — can no longer be swept by a per-turn
// clear; this prune is the backstop so Pending() can't stay stuck > 0. Set
// well beyond any real subagent's runtime: the old 30m default pruned healthy
// delegated subagents that ran 30-40 minutes and released held injects early
// (#2009). Overridable per-tracker via MaxAge
// (config [cc_backend].background_task_max_age).
const defaultAgentMaxAge = 2 * time.Hour

// maxAge resolves the effective prune threshold — the configured MaxAge, or the
// default when unset.
func (t *SubagentTracker) maxAge() time.Duration {
	if t.MaxAge > 0 {
		return t.MaxAge
	}
	return defaultAgentMaxAge
}

// Add registers a new subagent spawn. Duplicate IDs are silently ignored
// (handles --include-partial-messages replays in ccstream).
func (t *SubagentTracker) Add(id, description string) {
	t.AddEntry(TrackedSubagent{ID: id, Description: description})
}

// AddEntry registers a new spawn carrying its kind, model and subagent type.
// Add is the description-only shorthand. Duplicate IDs are ignored, as in Add.
func (t *SubagentTracker) AddEntry(e TrackedSubagent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	for _, ag := range t.pending {
		if ag.ID == e.ID {
			return
		}
	}
	if e.Kind == "" {
		e.Kind = SubagentKindAgent
	}
	e.added = time.Now()
	t.pending = append(t.pending, e)
	if t.start.IsZero() {
		t.start = time.Now()
	}
	t.notify()
}

// pruneLocked drops agents older than agentMaxAge. Caller holds mu; it does
// not notify (callers already do around their own mutations).
func (t *SubagentTracker) pruneLocked() {
	if len(t.pending) == 0 {
		return
	}
	cutoff := time.Now().Add(-t.maxAge())
	kept := t.pending[:0]
	for _, ag := range t.pending {
		if ag.added.Before(cutoff) {
			// A prune means a completion signal was never received. It's the
			// backstop, not the normal path — a run_in_background Bash whose
			// task_notification CC never emits would sit here holding system
			// injects until now. Warn so that quiet failure is visible rather
			// than a silent stall (raise background_task_max_age if it was a
			// genuinely long job).
			delegatedLog.Warnf("subagent tracker: pruned %q (id=%s) after %s with no completion signal — background work held system injects until this prune",
				ag.Description, ag.ID, time.Since(ag.added).Round(time.Second))
			continue
		}
		kept = append(kept, ag)
	}
	t.pending = kept
}

// SetModel records the model a tracked entry actually runs on, replacing any
// requested alias, and re-notifies on a change. An unknown id or a blank model
// is a no-op: a nested subagent's usage names an id that was never tracked.
func (t *SubagentTracker) SetModel(id, model string) {
	if model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.pending {
		if t.pending[i].ID == id {
			if t.pending[i].Model != model {
				t.pending[i].Model = model
				t.notify()
			}
			return
		}
	}
}

// IDsOfKind returns the ids of the tracked entries of one kind (a
// SubagentKind* value), oldest first.
func (t *SubagentTracker) IDsOfKind(kind string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for _, ag := range t.pending {
		if ag.Kind == kind {
			ids = append(ids, ag.ID)
		}
	}
	return ids
}

// NotifyIdle re-sends the "nothing running" status to both callbacks, to clear
// a stale indicator when the tracker is already empty.
func (t *SubagentTracker) NotifyIdle() {
	if t.OnRunning != nil {
		t.OnRunning(nil)
	}
	if t.OnStatus != nil {
		t.OnStatus("")
	}
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
	if len(t.pending) == 0 {
		t.start = time.Time{}
	}
	if t.OnRunning != nil {
		var running []RunningSubagent
		for _, ag := range t.pending {
			running = append(running, RunningSubagent{
				ID: ag.ID, Description: ag.Description, Kind: ag.Kind,
				Model: ag.Model, SubagentType: ag.SubagentType, Command: ag.Command, Started: ag.added,
			})
		}
		t.OnRunning(running)
	}
	if t.OnStatus == nil {
		return
	}
	descs := make([]string, len(t.pending))
	for i, ag := range t.pending {
		descs[i] = ag.Description
	}
	t.OnStatus(FormatSubagentDetail(descs))
}

// FormatSubagentDetail flattens running descriptions into the one-line detail
// string: comma-joined, or a count when none carries a description, or "" when
// nothing runs. Shared by notify and the app, which derives the same detail
// from the structured list so the two callbacks agree.
func FormatSubagentDetail(descriptions []string) string {
	if len(descriptions) == 0 {
		return ""
	}
	var descs []string
	for _, d := range descriptions {
		if d != "" {
			descs = append(descs, d)
		}
	}
	if len(descs) > 0 {
		return strings.Join(descs, ", ")
	}
	return fmt.Sprintf("%d subagent(s) running", len(descriptions))
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

// ExtractAgentModel parses the optional "model" and "subagent_type" fields from
// an Agent tool_use input: the model alias the spawn asked for ("" when it
// inherits) and the agent type it runs as.
func ExtractAgentModel(raw json.RawMessage) (model, subagentType string) {
	var input struct {
		Model        string `json:"model"`
		SubagentType string `json:"subagent_type"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Model, input.SubagentType
	}
	return "", ""
}

// ExtractAgentPrompt parses the "prompt" field from an Agent tool_use input
// JSON payload — the main agent's instruction to the subagent for its first run
// (#1355). Shown at the top of the subagent's run view.
func ExtractAgentPrompt(raw json.RawMessage) string {
	var input struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Prompt
	}
	return ""
}

// ExtractSendMessage parses a SendMessage tool_use input into (to, message): the
// target subagent's id (== its task_id) and the message body. Used to attribute a
// reactivation prompt to the subagent being resumed (#1355). CC's SendMessage
// carries both `to`/`recipient` (id) and `message`/`content` (body); prefer the
// canonical `to`/`message`, falling back to the aliases.
func ExtractSendMessage(raw json.RawMessage) (to, message string) {
	var input struct {
		To        string `json:"to"`
		Recipient string `json:"recipient"`
		Message   string `json:"message"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return "", ""
	}
	to = input.To
	if to == "" {
		to = input.Recipient
	}
	message = input.Message
	if message == "" {
		message = input.Content
	}
	return to, message
}

// ExtractBashBackground reports whether a Bash tool_use input requests
// backgrounding (CC's native `run_in_background` parameter). A backgrounded
// Bash outlives its turn and, on completion, drives a task_notification /
// autonomous run — so it must be tracked like a subagent for the pending-work
// gate. A synchronous Bash (flag absent/false) returns false and is not tracked.
func ExtractBashBackground(raw json.RawMessage) bool {
	var input struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.RunInBackground
	}
	return false
}

// ExtractAgentBackground reports whether an Agent (Task) tool_use input requests
// backgrounding via `run_in_background`. A background subagent's text streams to
// the parent stdout stream (parent_tool_use_id-tagged assistant messages); a
// foreground one's does not, so only the foreground case needs transcript
// tailing. Same `run_in_background` field as Bash; kept separate for a clear
// call site.
func ExtractAgentBackground(raw json.RawMessage) bool {
	// TRI-STATE, and the difference from Bash is the whole point. The Agent tool
	// BACKGROUNDS BY DEFAULT — its own documentation says to pass
	// run_in_background:false only when the next action depends on the result —
	// so the field is ABSENT from an ordinary spawn. Aliasing this to
	// ExtractBashBackground read that absence as foreground and armed a
	// foreground transcript tail for every default subagent, which the Agent
	// PostToolUse then stopped at launch (#1934). Bash is genuinely
	// foreground-by-default, so the two cannot share an implementation.
	var input struct {
		RunInBackground *bool `json:"run_in_background"`
	}
	if json.Unmarshal(raw, &input) == nil && input.RunInBackground != nil {
		return *input.RunInBackground
	}
	return true
}
