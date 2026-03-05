package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/log"
	"foci/internal/memory"
)

// ScheduleWakeFn is a callback to schedule a wake event.
// The id is the DB row ID for cleanup when the wake fires.
type ScheduleWakeFn func(id int64, delay time.Duration, message string) error

func NewRemindTool(rs *memory.ReminderStore, agentID string, wakeFn ScheduleWakeFn) *Tool {
	return &Tool{
		Name:        "remind",
		Description: "Defer a thought for later. By default the reminder surfaces as injected context at the specified time. Set wake=true to actively wake the session (fires a message to yourself at the specified time).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "The thought or reminder text"
				},
				"when": {
					"type": "string",
					"description": "When to surface: 'next_keepalive', 'next_session', 'tomorrow', a date (YYYY-MM-DD), an ISO timestamp (e.g. '2026-02-26T12:00:00Z'), or a duration (e.g. '2h', '30m')"
				},
				"wake": {
					"type": "boolean",
					"description": "If true, actively wake the session at the specified time instead of passively injecting context (default false)"
				}
			},
			"required": ["text", "when"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text string `json:"text"`
				When string `json:"when"`
				Wake bool   `json:"wake"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}

			if p.Text == "" {
				return ToolResult{}, fmt.Errorf("text is required")
			}
			if p.When == "" {
				return ToolResult{}, fmt.Errorf("when is required")
			}

			if p.Wake {
				return remindWake(rs, agentID, p.Text, p.When, wakeFn)
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
func remindWake(rs *memory.ReminderStore, agentID, text, when string, wakeFn ScheduleWakeFn) (ToolResult, error) {
	if wakeFn == nil {
		return ToolResult{}, fmt.Errorf("wake not configured")
	}

	dur, err := resolveWakeDuration(when)
	if err != nil {
		return ToolResult{}, err
	}

	id, err := rs.AddWake(agentID, text, when)
	if err != nil {
		return ToolResult{}, fmt.Errorf("store wake: %w", err)
	}

	if err := wakeFn(id, dur, text); err != nil {
		_ = rs.Dismiss(id) // clean up DB row on schedule failure
		return ToolResult{}, fmt.Errorf("schedule wake: %w", err)
	}

	log.Debugf("remind", "scheduled wake id=%d in %v: %q", id, dur, text)
	return TextResult(fmt.Sprintf("Wake scheduled in %v: %q", dur, text)), nil
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
		now := time.Now().UTC()
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		return time.Until(tomorrow), nil
	case "next_keepalive", "next_heartbeat", "next_session", "now":
		return 0, nil
	}

	return 0, fmt.Errorf("cannot parse when %q as duration, timestamp, or date", when)
}
