package agent

import (
	"strings"
	"testing"
)

func TestWarningQueue_PushAndDrain(t *testing.T) {
	q := NewWarningQueue()

	q.Push("WARN", "config", "unknown key: foo")
	q.Push("ERROR", "telegram", "get updates failed")

	warnings := q.Drain()
	if len(warnings) != 2 {
		t.Fatalf("Drain() returned %d warnings, want 2", len(warnings))
	}
	if !strings.Contains(warnings[0], "unknown key: foo") {
		t.Errorf("warnings[0] = %q, want to contain 'unknown key: foo'", warnings[0])
	}
	if !strings.Contains(warnings[1], "get updates failed") {
		t.Errorf("warnings[1] = %q, want to contain 'get updates failed'", warnings[1])
	}

	// Drain again should be empty
	if warnings := q.Drain(); warnings != nil {
		t.Errorf("second Drain() = %v, want nil", warnings)
	}
}

func TestWarningQueue_DrainEmpty(t *testing.T) {
	q := NewWarningQueue()
	if warnings := q.Drain(); warnings != nil {
		t.Errorf("Drain() on empty queue = %v, want nil", warnings)
	}
}

func TestWarningQueue_MaxSize(t *testing.T) {
	q := NewWarningQueue()
	q.maxSize = 3

	for i := 0; i < 10; i++ {
		q.Push("WARN", "test", "msg")
	}

	if q.Len() != 3 {
		t.Errorf("Len() = %d, want 3 (max size)", q.Len())
	}
}

func TestWarningQueue_Format(t *testing.T) {
	q := NewWarningQueue()
	q.Push("WARN", "config", "unknown key: foo.bar")

	warnings := q.Drain()
	expected := "[WARN] [config] unknown key: foo.bar"
	if warnings[0] != expected {
		t.Errorf("format = %q, want %q", warnings[0], expected)
	}
}

func TestCollectWarnings_NilQueue(t *testing.T) {
	a := &Agent{}
	if got := a.collectWarnings(); got != "" {
		t.Errorf("collectWarnings() with nil queue = %q, want empty", got)
	}
}

func TestCollectWarnings_EmptyQueue(t *testing.T) {
	a := &Agent{Warnings: NewWarningQueue()}
	if got := a.collectWarnings(); got != "" {
		t.Errorf("collectWarnings() with empty queue = %q, want empty", got)
	}
}

func TestCollectWarnings_WithWarnings(t *testing.T) {
	a := &Agent{Warnings: NewWarningQueue()}
	a.Warnings.Push("WARN", "config", "unknown key: foo")
	a.Warnings.Push("ERROR", "telegram", "connection failed")

	got := a.collectWarnings()
	if !strings.Contains(got, "[system warnings]") {
		t.Errorf("collectWarnings() missing header: %q", got)
	}
	if !strings.Contains(got, "unknown key: foo") {
		t.Errorf("collectWarnings() missing first warning: %q", got)
	}
	if !strings.Contains(got, "connection failed") {
		t.Errorf("collectWarnings() missing second warning: %q", got)
	}

	// Second call should return empty (drained)
	if got := a.collectWarnings(); got != "" {
		t.Errorf("collectWarnings() after drain = %q, want empty", got)
	}
}
