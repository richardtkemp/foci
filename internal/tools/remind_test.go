package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
)

func testRemindTool(t *testing.T) *Tool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	return NewRemindTool(rs, "test", nil)
}

func testRemindToolWithWake(t *testing.T, fn ScheduleWakeFn) *Tool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	return NewRemindTool(rs, "test", fn)
}

// --- Passive reminder tests (wake=false, default) ---

func TestRemind(t *testing.T) {
	// Verifies that a basic passive reminder (no wake) is stored and the result confirms both the text and the trigger time.
	t.Parallel()
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "check FTS5 phrase boosting",
		"when": "next_keepalive",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "next_keepalive") {
		t.Errorf("result = %q, expected mention of next_keepalive", result.Text)
	}
	if !strings.Contains(result.Text, "check FTS5 phrase boosting") {
		t.Errorf("result = %q, expected mention of text", result.Text)
	}
}

func TestRemindTomorrow(t *testing.T) {
	// Verifies that "tomorrow" is accepted as a valid when value for passive reminders and is reflected in the result.
	t.Parallel()
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "ask about Greece",
		"when": "tomorrow",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "tomorrow") {
		t.Errorf("result = %q", result.Text)
	}
}

func TestRemindMissingText(t *testing.T) {
	// Verifies that an empty text field is rejected with an error, enforcing that reminders must have content.
	t.Parallel()
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "",
		"when": "now",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestRemindMissingWhen(t *testing.T) {
	// Verifies that an empty when field is rejected with an error, enforcing that reminders must have a trigger time.
	t.Parallel()
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "something",
		"when": "",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty when")
	}
}

// --- Wake tests (wake=true) ---

func TestRemindWakeDelay(t *testing.T) {
	// Verifies that wake=true with a duration string (e.g. "30m") calls the schedule function with the correct duration and message.
	t.Parallel()
	var gotDur time.Duration
	var gotMsg string
	fn := func(id int64, d time.Duration, msg string) error {
		gotDur = d
		gotMsg = msg
		return nil
	}

	tool := testRemindToolWithWake(t, fn)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "check inbox",
		"when": "30m",
		"wake": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotDur != 30*time.Minute {
		t.Errorf("duration = %v, want 30m", gotDur)
	}
	if gotMsg != "check inbox" {
		t.Errorf("message = %q, want %q", gotMsg, "check inbox")
	}
	if !strings.Contains(result.Text, "check inbox") {
		t.Errorf("result = %q, want message in result", result.Text)
	}
}

func TestRemindWakeDelaySeconds(t *testing.T) {
	// Verifies that sub-minute durations in seconds (e.g. "10s") are parsed and passed correctly to the schedule function.
	t.Parallel()
	var gotDur time.Duration
	fn := func(id int64, d time.Duration, msg string) error {
		gotDur = d
		return nil
	}

	tool := testRemindToolWithWake(t, fn)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "ping",
		"when": "10s",
		"wake": true,
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDur != 10*time.Second {
		t.Errorf("duration = %v, want 10s", gotDur)
	}
}

func TestRemindWakeAtTimestamp(t *testing.T) {
	// Verifies that an RFC3339 timestamp is accepted as a when value and converted to a duration that approximates the time until that point.
	t.Parallel()
	var gotDur time.Duration
	fn := func(id int64, d time.Duration, msg string) error {
		gotDur = d
		return nil
	}

	tool := testRemindToolWithWake(t, fn)
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "meeting",
		"when": future,
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDur < 1*time.Hour || gotDur > 3*time.Hour {
		t.Errorf("duration = %v, expected ~2h", gotDur)
	}
}

func TestRemindWakePastTimestamp(t *testing.T) {
	// Verifies that a timestamp in the past is rejected with an error mentioning "past", preventing nonsensical reminders.
	t.Parallel()
	fn := func(id int64, d time.Duration, msg string) error { return nil }
	tool := testRemindToolWithWake(t, fn)

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "late",
		"when": past,
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for past timestamp")
	}
	if !strings.Contains(err.Error(), "past") {
		t.Errorf("error = %q, want 'past'", err.Error())
	}
}

func TestRemindWakeEmptyText(t *testing.T) {
	// Verifies that wake reminders with empty text are rejected with a clear "text is required" error.
	t.Parallel()
	fn := func(id int64, d time.Duration, msg string) error { return nil }
	tool := testRemindToolWithWake(t, fn)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "",
		"when": "30m",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty text")
	}
	if !strings.Contains(err.Error(), "text is required") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemindWakeNilFunction(t *testing.T) {
	// Verifies that requesting wake=true when no schedule function is configured returns a "not configured" error rather than panicking.
	t.Parallel()
	tool := testRemindTool(t) // nil wake fn
	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
		"when": "30m",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil wake function")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemindWakeInvalidDuration(t *testing.T) {
	// Verifies that an unparseable when value returns a "cannot parse" error rather than silently failing.
	t.Parallel()
	fn := func(id int64, d time.Duration, msg string) error { return nil }
	tool := testRemindToolWithWake(t, fn)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
		"when": "notaduration",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for invalid when")
	}
	if !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemindWakeNegativeDelay(t *testing.T) {
	// Verifies that a negative duration (e.g. "-5m") is rejected with an error requiring a positive value.
	t.Parallel()
	fn := func(id int64, d time.Duration, msg string) error { return nil }
	tool := testRemindToolWithWake(t, fn)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
		"when": "-5m",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for negative delay")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemindWakeCallbackError(t *testing.T) {
	// Verifies that an error returned by the schedule callback is propagated back to the caller.
	t.Parallel()
	fn := func(id int64, d time.Duration, msg string) error {
		return fmt.Errorf("scheduler full")
	}

	tool := testRemindToolWithWake(t, fn)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
		"when": "30m",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !strings.Contains(err.Error(), "scheduler full") {
		t.Errorf("error = %q, want 'scheduler full'", err.Error())
	}
}

func TestRemindWakeTomorrow(t *testing.T) {
	// Verifies that "tomorrow" resolves to a duration within the next 24 hours when used as a wake trigger.
	t.Parallel()
	var gotDur time.Duration
	fn := func(id int64, d time.Duration, msg string) error {
		gotDur = d
		return nil
	}

	tool := testRemindToolWithWake(t, fn)
	params, _ := json.Marshal(map[string]interface{}{
		"text": "morning check",
		"when": "tomorrow",
		"wake": true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be between 0 and 24 hours
	if gotDur < 0 || gotDur > 24*time.Hour {
		t.Errorf("duration = %v, expected 0-24h for tomorrow", gotDur)
	}
}
