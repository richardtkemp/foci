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

func NewRemindTool(rs *memory.ReminderStore, agentID string, wakeFn ScheduleWakeFn, cancelFn CancelWakeFn) *Tool {
	return &Tool{
		Name:        "remind",
		Description: "Defer a thought for later. By default the reminder surfaces as injected context at the specified time. Set wake=true to actively wake the session (fires a message to yourself at the specified time). Set list=true to show pending wakes, or cancel=<id> to cancel one.",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "The thought or reminder text (required unless list or cancel is set)"
				},
				"when": {
					"type": "string",
					"description": "When to surface: 'next_keepalive', 'next_session', 'tomorrow', a date (YYYY-MM-DD), an ISO timestamp (e.g. '2026-02-26T12:00:00Z'), or a duration (e.g. '2h', '30m'). Required unless list or cancel is set."
				},
				"wake": {
					"type": "boolean",
					"description": "If true, actively wake the session at the specified time instead of passively injecting context (default false)"
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

			if p.List {
				return remindListWakes(rs, agentID)
			}
			if p.Cancel != 0 {
				return remindCancelWake(rs, agentID, p.Cancel, cancelFn)
			}

			if p.Text == "" {
				return ToolResult{}, fmt.Errorf("text is required")
			}
			if p.When == "" {
				return ToolResult{}, fmt.Errorf("when is required")
			}

			if p.Wake {
				return remindWake(SessionKeyFromContext(ctx), rs, agentID, p.Text, p.When, wakeFn)
			}

			// Passive reminder — store in ReminderStore
			if err := rs.Add(agentID, p.Text, p.When); err != nil {
				return ToolResult{}, fmt.Errorf("add reminder: %w", err)
			}

			return TextResult(fmt.Sprintf("Reminder set for %s: %s", p.When, p.Text)), nil
		},
	}
}

// remindWake stores a wake reminder in the DB, then schedules it in-memory.
func remindWake(sessionKey string, rs *memory.ReminderStore, agentID, text, when string, wakeFn ScheduleWakeFn) (ToolResult, error) {
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
	return TextResult(fmt.Sprintf("Wake scheduled in %v (id=%d): %q", dur, id, text)), nil
}

// remindListWakes renders this agent's pending scheduled wakes, newest due first.
func remindListWakes(rs *memory.ReminderStore, agentID string) (ToolResult, error) {
	pending, err := rs.PendingWakes(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("list wakes: %w", err)
	}
	if len(pending) == 0 {
		return TextResult("No pending wakes."), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d pending wake(s):\n", len(pending))
	now := time.Now()
	for _, r := range pending {
		until := r.DueAt.Sub(now).Round(time.Second)
		when := fmt.Sprintf("in %v", until)
		if until < 0 {
			when = "overdue"
		}
		fmt.Fprintf(&b, "  id=%d  due %s (%s)  %q\n",
			r.ID, r.DueAt.Format(time.RFC3339), when, truncateWakeText(r.Text))
	}
	return TextResult(strings.TrimRight(b.String(), "\n")), nil
}

// remindCancelWake stops a pending wake's timer and removes its stored row.
// The id must belong to this agent — PendingWakes is agent-scoped, so an id
// from another agent reads as not found rather than cancelling across agents.
func remindCancelWake(rs *memory.ReminderStore, agentID string, id int64, cancelFn CancelWakeFn) (ToolResult, error) {
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
		return ToolResult{}, fmt.Errorf("wake id %d is stored but has no live timer — it may have already fired", id)
	}

	remindLog.Debugf("cancelled wake id=%d for agent %s", id, agentID)
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
	case "next_keepalive", "next_heartbeat", "next_session", "now":
		return 0, nil
	}

	return 0, fmt.Errorf("cannot parse when %q as duration, timestamp, or date", when)
}
