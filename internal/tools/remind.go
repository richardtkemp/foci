package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/memory"
)

// ScheduleWakeFn is a callback to schedule a wake event.
// The id is the DB row ID for cleanup when the wake fires.
// sessionKey is the originating session so the wake fires on the correct platform.
type ScheduleWakeFn func(id int64, delay time.Duration, message, sessionKey string) error

// CancelWakeFn cancels a scheduled wake by DB row ID, reporting whether a
// live timer was found. A scheduled wake lives in an in-process timer, so the
// DB row alone is not authoritative — cancelling has to go through the
// scheduler that owns the timer.
type CancelWakeFn func(id int64) bool

// SessionHeldFn reports whether a session's system input is currently held
// behind background work (wired to Agent.SystemInputHeld). The remind tool
// uses it to tell a fired wake "held behind background work" from one merely
// "waiting to run" (#2233). Nil disables the distinction (every fired wake
// reads "waiting to run").
type SessionHeldFn func(sessionKey string) bool

// RemindOption customises the remind tool at construction — optional wiring
// that would otherwise widen NewRemindTool's frozen signature.
type RemindOption func(*remindConfig)

// remindConfig is the accumulated RemindOption state.
type remindConfig struct {
	held SessionHeldFn
}

// WithSessionHeld wires the held-behind-background-work predicate the list
// and cancel paths consult for a fired wake (#2233). Nil-safe.
func WithSessionHeld(fn SessionHeldFn) RemindOption {
	return func(c *remindConfig) { c.held = fn }
}

// Wake list/cancel states (the JSON list's "state" field, #2233): a wake is
// pending until its timer fires; from firing until its turn runs it is held
// (behind background work) or fired (waiting to run); once the turn runs the
// row is dismissed and no longer listed.
// The values live in memory so the app's reminder indicator (#2600) shares
// them.
const (
	wakeStatePending = memory.WakeStatePending
	wakeStateHeld    = memory.WakeStateHeld
	wakeStateFired   = memory.WakeStateFired
)

// wakeStateOf classifies one listed row against the store's fired overlay.
func wakeStateOf(rs *memory.ReminderStore, r memory.Reminder, held SessionHeldFn) string {
	return rs.WakeState(r.ID, held)
}

func NewRemindTool(rs *memory.ReminderStore, agentID string, wakeFn ScheduleWakeFn, cancelFn CancelWakeFn, opts ...RemindOption) *Tool {
	var cfg remindConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Tool{
		Name:        "remind",
		Description: "Defer a thought for later. By default (passive) the reminder is added as context to the next message to ANY of your root chats once it is due: not at an exact time, and possibly in a different chat of yours. Use wake=true ONLY when (a) it must happen at a specific time rather than just \"later\", or (b) it must come back to THIS exact session rather than any of your chats; a wake sends this session a message to yourself at the specified time. Set list=true to show pending wakes, or cancel=<id> to cancel one.",
		ExecExport:  true,
		JSONOutput:  `list: [{"id", "due_at" (RFC 3339), "text" (full), "state" ("pending" until the timer fires, then "held" while background work delays the wake or "fired" while it waits to run)}], [] when none pending. Otherwise one object: {"set": "reminder", "when", "text"} for a passive reminder, {"set": "wake", "id", "due_at", "text"} for wake=true, {"cancelled": id, "due_at", "text"} for cancel.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "The thought or reminder text (required unless list or cancel is set)"
				},
				"when": {
					"type": "string",
					"description": "When to surface: a duration (e.g. '2h', '30m'), 'tomorrow', a date (YYYY-MM-DD), or an ISO timestamp (e.g. '2026-02-26T12:00:00Z'). Required unless list or cancel is set."
				},
				"wake": {
					"type": "boolean",
					"description": "If true, wake THIS session at the specified time with a message to yourself. Use it only when the reminder needs a specific time or this exact session; otherwise leave it false (default): a passive reminder surfaces on your next message in any of your chats once due."
				},
				"list": {
					"type": "boolean",
					"description": "If true, list pending scheduled wakes (id, due time, text) instead of setting a reminder"
				},
				"cancel": {
					"type": "integer",
					"description": "Cancel the pending scheduled wake with this id (from list). Stops the timer and removes the stored reminder."
				}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Text   string `json:"text"`
				When   string `json:"when"`
				Wake   bool   `json:"wake"`
				List   bool   `json:"list"`
				Cancel int64  `json:"cancel"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			if err := refuseCrossAgentSpawnRemind(ctx); err != nil {
				return ToolResult{}, err
			}

			if p.List {
				return remindListWakes(rs, agentID, cfg.held, WantsJSON(ctx))
			}
			if p.Cancel != 0 {
				return remindCancelWake(rs, agentID, p.Cancel, cancelFn, cfg.held, WantsJSON(ctx))
			}

			if p.Text == "" {
				return ToolResult{}, fmt.Errorf("text is required")
			}
			if p.When == "" {
				return ToolResult{}, fmt.Errorf("when is required")
			}

			if p.Wake {
				return remindWake(SessionKeyFromContext(ctx), rs, agentID, p.Text, p.When, wakeFn, WantsJSON(ctx))
			}

			// Passive reminder — store in ReminderStore. Validate `when` first: the
			// store's resolveWhen treats anything it cannot parse as "due now".
			if _, err := resolveWakeDuration(p.When); err != nil {
				return ToolResult{}, err
			}
			if err := rs.Add(agentID, p.Text, p.When); err != nil {
				return ToolResult{}, fmt.Errorf("add reminder: %w", err)
			}

			if WantsJSON(ctx) {
				return JSONResult(map[string]string{"set": "reminder", "when": p.When, "text": p.Text})
			}
			return TextResult(fmt.Sprintf("Reminder set for %s: %s", p.When, p.Text)), nil
		},
	}
}

// remindWake stores a wake reminder in the DB, then schedules it in-memory.
func remindWake(sessionKey string, rs *memory.ReminderStore, agentID, text, when string, wakeFn ScheduleWakeFn, asJSON bool) (ToolResult, error) {
	if wakeFn == nil {
		return ToolResult{}, fmt.Errorf("wake not configured")
	}

	dur, err := resolveWakeDuration(when)
	if err != nil {
		return ToolResult{}, err
	}

	id, err := rs.AddWake(agentID, sessionKey, text, when)
	if err != nil {
		return ToolResult{}, fmt.Errorf("store wake: %w", err)
	}

	if err := wakeFn(id, dur, text, sessionKey); err != nil {
		_ = rs.Dismiss(id) // clean up DB row on schedule failure
		return ToolResult{}, fmt.Errorf("schedule wake: %w", err)
	}

	remindLog.Debugf("session=%s scheduled wake id=%d in %v: %q", sessionKey, id, dur, text)
	if asJSON {
		return JSONResult(struct {
			Set   string `json:"set"`
			ID    int64  `json:"id"`
			DueAt string `json:"due_at"`
			Text  string `json:"text"`
		}{"wake", id, time.Now().Add(dur).Format(time.RFC3339), text})
	}
	return TextResult(fmt.Sprintf("Wake scheduled in %v (id=%d): %q", dur, id, text)), nil
}

// remindListWakes renders this agent's pending scheduled wakes, newest due
// first. A wake whose timer has fired but whose turn has not run reads as
// "fired" — held behind background work when held says so, otherwise waiting
// to run — never as "overdue" (#2233); the JSON list carries the same
// classification in each row's "state".
func remindListWakes(rs *memory.ReminderStore, agentID string, held SessionHeldFn, asJSON bool) (ToolResult, error) {
	pending, err := rs.PendingWakes(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("list wakes: %w", err)
	}
	if asJSON {
		type wake struct {
			ID    int64  `json:"id"`
			DueAt string `json:"due_at"`
			Text  string `json:"text"`
			State string `json:"state"`
		}
		out := make([]wake, 0, len(pending))
		for _, r := range pending {
			out = append(out, wake{r.ID, r.DueAt.Format(time.RFC3339), r.Text, wakeStateOf(rs, r, held)})
		}
		return JSONResult(out)
	}
	if len(pending) == 0 {
		return TextResult("No pending wakes."), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d pending wake(s):\n", len(pending))
	now := time.Now()
	for _, r := range pending {
		var when string
		switch wakeStateOf(rs, r, held) {
		case wakeStateHeld:
			when = "fired, held behind background work"
		case wakeStateFired:
			when = "fired, waiting to run"
		default:
			until := r.DueAt.Sub(now).Round(time.Second)
			when = fmt.Sprintf("in %v", until)
			if until < 0 {
				when = "overdue"
			}
		}
		fmt.Fprintf(&b, "  id=%d  due %s (%s)  %q\n",
			r.ID, r.DueAt.Format(time.RFC3339), when, truncateWakeText(r.Text))
	}
	return TextResult(strings.TrimRight(b.String(), "\n")), nil
}

// remindCancelWake stops a pending wake's timer and removes its stored row.
// The id must belong to this agent — PendingWakes is agent-scoped, so an id
// from another agent reads as not found rather than cancelling across agents.
// A wake that already fired is NOT cancellable (#2036 claim rule): the error
// says it fired and whether it is held behind background work (it will run
// when that work ends) or merely waiting to run (#2233).
func remindCancelWake(rs *memory.ReminderStore, agentID string, id int64, cancelFn CancelWakeFn, held SessionHeldFn, asJSON bool) (ToolResult, error) {
	if cancelFn == nil {
		return ToolResult{}, fmt.Errorf("wake not configured")
	}

	pending, err := rs.PendingWakes(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("look up wake: %w", err)
	}
	var target *memory.Reminder
	for i := range pending {
		if pending[i].ID == id {
			target = &pending[i]
			break
		}
	}
	if target == nil {
		return ToolResult{}, fmt.Errorf("no pending wake with id %d", id)
	}

	// The timer owns the DB row: cancelling it makes the scheduler dismiss the
	// row itself. Deleting here as well would race that cleanup for no gain.
	if !cancelFn(id) {
		// One classifier for list and cancel alike (wakeStateOf), so the
		// fired/held wording of the two paths cannot drift (#2233).
		switch wakeStateOf(rs, *target, held) {
		case wakeStateHeld:
			return ToolResult{}, fmt.Errorf("wake id %d has fired and is held behind background work; it will run when that work ends", id)
		case wakeStateFired:
			return ToolResult{}, fmt.Errorf("wake id %d has fired and is waiting to run", id)
		}
		return ToolResult{}, fmt.Errorf("wake id %d is stored but has no live timer — it may have already fired", id)
	}

	remindLog.Debugf("cancelled wake id=%d for agent %s", id, agentID)
	if asJSON {
		return JSONResult(struct {
			Cancelled int64  `json:"cancelled"`
			DueAt     string `json:"due_at"`
			Text      string `json:"text"`
		}{id, target.DueAt.Format(time.RFC3339), target.Text})
	}
	return TextResult(fmt.Sprintf("Cancelled wake id=%d (was due %s): %q",
		id, target.DueAt.Format(time.RFC3339), truncateWakeText(target.Text))), nil
}

// truncateWakeText shortens reminder text so a list stays scannable; wake
// prompts are routinely multi-paragraph.
func truncateWakeText(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 70
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// resolveWakeDuration converts a when string to a duration from now.
// Supports Go durations ("30m", "2h"), ISO timestamps, dates, and
// the same human tags as the passive reminder path.
func resolveWakeDuration(when string) (time.Duration, error) {
	// Try Go duration first (most common for wake)
	if d, err := time.ParseDuration(when); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("delay must be positive")
		}
		return d, nil
	}

	// Try ISO timestamp
	if t, err := time.Parse(time.RFC3339, when); err == nil {
		dur := time.Until(t)
		if dur < 0 {
			return 0, fmt.Errorf("timestamp is in the past")
		}
		return dur, nil
	}

	// Try date
	if t, err := time.Parse("2006-01-02", when); err == nil {
		dur := time.Until(t)
		if dur < 0 {
			return 0, fmt.Errorf("date is in the past")
		}
		return dur, nil
	}

	// Human tags
	switch when {
	case "tomorrow":
		now := time.Now()
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		return time.Until(tomorrow), nil
	case "now":
		return 0, nil
	case "next_keepalive", "next_heartbeat", "next_session":
		// Removed (Dick 2026-10-28): all three meant "due now" and surfaced on the
		// next message to any root chat, not at a keepalive or a new session.
		return 0, fmt.Errorf("when %q is no longer supported: use a duration (e.g. '30m'), 'tomorrow', a date (YYYY-MM-DD) or an ISO timestamp", when)
	}

	return 0, fmt.Errorf("cannot parse when %q: use a duration (e.g. '30m'), 'tomorrow', a date (YYYY-MM-DD) or an ISO timestamp", when)
}
