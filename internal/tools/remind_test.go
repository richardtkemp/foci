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
	return NewRemindTool(rs, "test", nil, nil)
}

func testRemindToolWithWake(t *testing.T, fn ScheduleWakeFn) *Tool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	return NewRemindTool(rs, "test", fn, nil)
}

// testRemindToolWithCancel returns a tool sharing one store, plus the store,
// so a test can schedule wakes and then list/cancel them.
func testRemindToolWithCancel(t *testing.T, cancelFn CancelWakeFn) (*Tool, *memory.ReminderStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	noopSchedule := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
	return NewRemindTool(rs, "test", noopSchedule, cancelFn), rs
}

// scheduleWake drives the tool's own wake path so the row is created exactly
// as production creates it.
func scheduleWake(t *testing.T, tool *Tool, text, when string) {
	t.Helper()
	params, _ := json.Marshal(map[string]interface{}{"text": text, "when": when, "wake": true})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("schedule %q: %v", text, err)
	}
}

// --- list / cancel tests (#1648) ---

func TestRemindListWakesEmpty(t *testing.T) {
	// Verifies list=true on an agent with no scheduled wakes reports the empty
	// case rather than erroring or printing a bare header.
	t.Parallel()
	tool, _ := testRemindToolWithCancel(t, nil)
	params, _ := json.Marshal(map[string]interface{}{"list": true})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "No pending wakes") {
		t.Errorf("result = %q, want empty-case message", result.Text)
	}
}

func TestRemindListWakes(t *testing.T) {
	// Verifies list=true enumerates pending wakes with their ids, so the id
	// needed by cancel is actually discoverable. Without ids in the listing
	// there is no way to name a wake for cancellation.
	t.Parallel()
	tool, rs := testRemindToolWithCancel(t, nil)
	scheduleWake(t, tool, "first wake", "30m")
	scheduleWake(t, tool, "second wake", "2h")

	pending, err := rs.PendingWakes("test")
	if err != nil {
		t.Fatalf("PendingWakes: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("premise failed: %d pending wakes, want 2", len(pending))
	}

	params, _ := json.Marshal(map[string]interface{}{"list": true})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "2 pending wake(s)") {
		t.Errorf("result = %q, want count header", result.Text)
	}
	for _, r := range pending {
		if !strings.Contains(result.Text, fmt.Sprintf("id=%d", r.ID)) {
			t.Errorf("result = %q, want id=%d", result.Text, r.ID)
		}
	}
	if !strings.Contains(result.Text, "first wake") || !strings.Contains(result.Text, "second wake") {
		t.Errorf("result = %q, want both texts", result.Text)
	}
}

func TestRemindCancelWake(t *testing.T) {
	// Verifies cancel=<id> reaches the scheduler's cancel callback with that
	// exact id. The timer lives in-process, so cancelling MUST go through the
	// callback — deleting the DB row alone would leave the timer armed.
	t.Parallel()
	var cancelled []int64
	tool, rs := testRemindToolWithCancel(t, func(id int64) bool {
		cancelled = append(cancelled, id)
		return true
	})
	scheduleWake(t, tool, "doomed wake", "45m")

	pending, err := rs.PendingWakes("test")
	if err != nil || len(pending) != 1 {
		t.Fatalf("premise failed: pending=%d err=%v", len(pending), err)
	}
	id := pending[0].ID

	params, _ := json.Marshal(map[string]interface{}{"cancel": id})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cancelled) != 1 || cancelled[0] != id {
		t.Fatalf("cancel callback got %v, want [%d]", cancelled, id)
	}
	if !strings.Contains(result.Text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("result = %q, want the cancelled id", result.Text)
	}
	if !strings.Contains(result.Text, "doomed wake") {
		t.Errorf("result = %q, want the cancelled text", result.Text)
	}
}

func TestRemindCancelWakeUnknownID(t *testing.T) {
	// Verifies cancelling an id that is not pending errors instead of silently
	// reporting success — a false "cancelled" would leave a live wake armed.
	t.Parallel()
	called := false
	tool, _ := testRemindToolWithCancel(t, func(id int64) bool { called = true; return true })

	params, _ := json.Marshal(map[string]interface{}{"cancel": 9999})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error for unknown id")
	} else if !strings.Contains(err.Error(), "no pending wake with id 9999") {
		t.Errorf("error = %q", err.Error())
	}
	if called {
		t.Error("cancel callback must not fire for an unknown id")
	}
}

func TestRemindCancelWakeOtherAgent(t *testing.T) {
	// Verifies an id belonging to a DIFFERENT agent cannot be cancelled.
	// PendingWakes is agent-scoped, so the lookup is what enforces isolation.
	t.Parallel()
	called := false
	tool, rs := testRemindToolWithCancel(t, func(id int64) bool { called = true; return true })

	otherID, err := rs.AddWake("someone-else", "sk", "not yours", "1h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}

	params, _ := json.Marshal(map[string]interface{}{"cancel": otherID})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error cancelling another agent's wake")
	}
	if called {
		t.Error("cancel callback must not fire across agents")
	}
}

func TestRemindCancelWakeNoLiveTimer(t *testing.T) {
	// Verifies that a stored row with no live timer (callback returns false)
	// surfaces as an error, not a bogus success.
	t.Parallel()
	tool, rs := testRemindToolWithCancel(t, func(id int64) bool { return false })
	scheduleWake(t, tool, "stale row", "1h")

	pending, _ := rs.PendingWakes("test")
	if len(pending) != 1 {
		t.Fatalf("premise failed: %d pending", len(pending))
	}

	params, _ := json.Marshal(map[string]interface{}{"cancel": pending[0].ID})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when no live timer exists")
	}
	if !strings.Contains(err.Error(), "no live timer") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemindCancelWakeNilFunction(t *testing.T) {
	// Verifies cancel with no scheduler configured errors rather than panicking,
	// mirroring the existing nil-wakeFn behaviour.
	t.Parallel()
	tool, _ := testRemindToolWithCancel(t, nil)
	params, _ := json.Marshal(map[string]interface{}{"cancel": 1})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil cancel function")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q", err.Error())
	}
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error {
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error {
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error {
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
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
	fn := func(id int64, d time.Duration, msg, sessionKey string) error {
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

func TestRemindToolExecExport(t *testing.T) {
	// Verifies the remind tool is exec-exported so it surfaces as a foci_remind shell function in delegated backends. Without this, delegated agents (Claude Code mode) have no way to set reminders even when the tool is registered.
	t.Parallel()
	tool := testRemindTool(t)
	if !tool.ExecExport {
		t.Fatal("remind tool should have ExecExport: true")
	}
	registry := NewRegistry()
	registry.Register(tool)
	names := registry.ExportedNames()
	found := false
	for _, n := range names {
		if n == "foci_remind" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ExportedNames() = %v, want to include foci_remind", names)
	}
}

func TestRemindWakeTomorrow(t *testing.T) {
	// Verifies that "tomorrow" resolves to a duration within the next 24 hours when used as a wake trigger.
	t.Parallel()
	var gotDur time.Duration
	fn := func(id int64, d time.Duration, msg, sessionKey string) error {
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
