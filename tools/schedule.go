package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"clod/log"
)

// ScheduleWakeFn is a callback to schedule a wake event
type ScheduleWakeFn func(delay time.Duration, message string) error

var scheduleWakeFn ScheduleWakeFn

// SetScheduleWakeFn registers the callback for scheduling wakes.
// This is set by main.go before the tools are used.
func SetScheduleWakeFn(fn ScheduleWakeFn) {
	scheduleWakeFn = fn
}

func NewScheduleWakeTool() *Tool {
	return &Tool{
		Name:        "schedule_wake",
		Description: "Schedule a message to be sent to the session at a specified time or delay",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"delay": {
					"type": "string",
					"description": "Duration delay (e.g. '30m', '2h', '1d'). Either delay or at required."
				},
				"at": {
					"type": "string",
					"description": "ISO 8601 timestamp (e.g. '2026-02-21T15:30:00Z'). Either delay or at required."
				},
				"message": {
					"type": "string",
					"description": "Message to send to the session"
				}
			},
			"required": ["message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return scheduleWakeExecute(ctx, params)
		},
	}
}

func scheduleWakeExecute(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Delay   string `json:"delay"`
		At      string `json:"at"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	if p.Message == "" {
		return "", fmt.Errorf("message is required")
	}

	if p.Delay == "" && p.At == "" {
		return "", fmt.Errorf("either delay or at is required")
	}

	if p.Delay != "" && p.At != "" {
		return "", fmt.Errorf("specify only one of delay or at, not both")
	}

	if scheduleWakeFn == nil {
		return "", fmt.Errorf("schedule_wake not configured")
	}

	var dur time.Duration
	var err error

	if p.Delay != "" {
		// Parse duration string (e.g. "30m", "2h")
		dur, err = time.ParseDuration(p.Delay)
		if err != nil {
			return "", fmt.Errorf("parse delay: %w", err)
		}
		if dur < 0 {
			return "", fmt.Errorf("delay must be positive")
		}
	} else {
		// Parse ISO timestamp
		t, err := time.Parse(time.RFC3339, p.At)
		if err != nil {
			return "", fmt.Errorf("parse timestamp: %w", err)
		}
		dur = time.Until(t)
		if dur < 0 {
			return "", fmt.Errorf("timestamp is in the past")
		}
	}

	if err := scheduleWakeFn(dur, p.Message); err != nil {
		return "", fmt.Errorf("schedule wake: %w", err)
	}

	log.Debugf("schedule_wake", "scheduled wake in %v: %q", dur, p.Message)

	return fmt.Sprintf("Wake scheduled in %v: %q", dur, p.Message), nil
}
