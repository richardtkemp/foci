package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestScheduleWakeDelay(t *testing.T) {
	var gotDur time.Duration
	var gotMsg string

	fn := func(d time.Duration, msg string) error {
		gotDur = d
		gotMsg = msg
		return nil
	}

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "30m",
		"message": "check inbox",
	})

	result, err := scheduleWakeExecute(context.Background(), params, fn)
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

func TestScheduleWakeDelaySeconds(t *testing.T) {
	var gotDur time.Duration
	fn := func(d time.Duration, msg string) error {
		gotDur = d
		return nil
	}

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "10s",
		"message": "ping",
	})
	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDur != 10*time.Second {
		t.Errorf("duration = %v, want 10s", gotDur)
	}
}

func TestScheduleWakeAt(t *testing.T) {
	var gotDur time.Duration
	fn := func(d time.Duration, msg string) error {
		gotDur = d
		return nil
	}

	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	params, _ := json.Marshal(map[string]interface{}{
		"at":      future,
		"message": "meeting",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be approximately 2 hours (within 5 seconds of setup time)
	if gotDur < 1*time.Hour || gotDur > 3*time.Hour {
		t.Errorf("duration = %v, expected ~2h", gotDur)
	}
}

func TestScheduleWakePastTimestamp(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	params, _ := json.Marshal(map[string]interface{}{
		"at":      past,
		"message": "late",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for past timestamp")
	}
	if !strings.Contains(err.Error(), "past") {
		t.Errorf("error = %q, want 'past'", err.Error())
	}
}

func TestScheduleWakeBothDelayAndAt(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "30m",
		"at":      "2030-01-01T00:00:00Z",
		"message": "both",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for both delay and at")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %q, want 'not both'", err.Error())
	}
}

func TestScheduleWakeNeitherDelayNorAt(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"message": "nothing",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for neither delay nor at")
	}
	if !strings.Contains(err.Error(), "either delay or at") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeEmptyMessage(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "30m",
		"message": "",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for empty message")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeNilFunction(t *testing.T) {
	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "30m",
		"message": "hello",
	})

	_, err := scheduleWakeExecute(context.Background(), params, nil)
	if err == nil {
		t.Fatal("expected error for nil wake function")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeInvalidDelay(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "notaduration",
		"message": "hello",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for invalid delay")
	}
	if !strings.Contains(err.Error(), "parse delay") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeInvalidTimestamp(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"at":      "not-a-timestamp",
		"message": "hello",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for invalid timestamp")
	}
	if !strings.Contains(err.Error(), "parse timestamp") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeNegativeDelay(t *testing.T) {
	fn := func(d time.Duration, msg string) error { return nil }

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "-5m",
		"message": "hello",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error for negative delay")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestScheduleWakeCallbackError(t *testing.T) {
	fn := func(d time.Duration, msg string) error {
		return fmt.Errorf("scheduler full")
	}

	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "30m",
		"message": "hello",
	})

	_, err := scheduleWakeExecute(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !strings.Contains(err.Error(), "scheduler full") {
		t.Errorf("error = %q, want 'scheduler full'", err.Error())
	}
}

func TestNewScheduleWakeToolNoArgs(t *testing.T) {
	tool := NewScheduleWakeTool()
	if tool.Name != "schedule_wake" {
		t.Errorf("name = %q", tool.Name)
	}

	// Calling with no wake function should error
	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "1m",
		"message": "test",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil wake function")
	}
}

func TestNewScheduleWakeToolWithFn(t *testing.T) {
	called := false
	fn := func(d time.Duration, msg string) error {
		called = true
		return nil
	}

	tool := NewScheduleWakeTool(fn)
	params, _ := json.Marshal(map[string]interface{}{
		"delay":   "1m",
		"message": "test",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("wake function was not called")
	}
}
