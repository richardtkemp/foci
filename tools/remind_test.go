package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/memory"
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
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "check FTS5 phrase boosting",
		"when": "next_keepalive",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "next_keepalive") {
		t.Errorf("result = %q, expected mention of next_keepalive", result)
	}
	if !strings.Contains(result, "check FTS5 phrase boosting") {
		t.Errorf("result = %q, expected mention of text", result)
	}
}

func TestRemindTomorrow(t *testing.T) {
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "ask about Greece",
		"when": "tomorrow",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "tomorrow") {
		t.Errorf("result = %q", result)
	}
}

func TestRemindMissingText(t *testing.T) {
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
	if !strings.Contains(result, "check inbox") {
		t.Errorf("result = %q, want message in result", result)
	}
}

func TestRemindWakeDelaySeconds(t *testing.T) {
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
