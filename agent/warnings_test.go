package agent

import (
	"strings"
	"testing"
	"time"
)

func TestWarningQueue_PushAndDrain(t *testing.T) {
	q := NewWarningQueue(0, 0)

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
	q := NewWarningQueue(0, 0)
	if warnings := q.Drain(); warnings != nil {
		t.Errorf("Drain() on empty queue = %v, want nil", warnings)
	}
}

func TestWarningQueue_MaxSize(t *testing.T) {
	q := NewWarningQueue(0, 0)
	q.maxSize = 3

	for i := 0; i < 10; i++ {
		q.Push("WARN", "test", "msg")
	}

	if q.Len() != 3 {
		t.Errorf("Len() = %d, want 3 (max size)", q.Len())
	}
}

func TestWarningQueue_Format(t *testing.T) {
	q := NewWarningQueue(0, 0)
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
	a := &Agent{Warnings: NewWarningQueue(0, 0)}
	if got := a.collectWarnings(); got != "" {
		t.Errorf("collectWarnings() with empty queue = %q, want empty", got)
	}
}

func TestCollectWarnings_WithWarnings(t *testing.T) {
	a := &Agent{Warnings: NewWarningQueue(0, 0)}
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

// --- Normalization tests ---

func TestNormalizeWarning(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"digits", "retry in 30 seconds", "retry in <N> seconds"},
		{"hex", "request id abc123def456789a failed", "request id <HEX> failed"},
		{"ip", "connection to 192.168.1.100 refused", "connection to <IP> refused"},
		{"mixed", "host 10.0.0.1 error 42 ref abcdef12", "host <IP> error <N> ref <HEX>"},
		{"no_change", "something went wrong", "something went wrong"},
		{"single_digit", "retry 5 times", "retry 5 times"}, // single digit not replaced
		{"short_hex", "code abc12", "code abc<N>"},          // too short for hex
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeWarning(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeWarning(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- Rate-limiting tests ---

func newTestQueue(max int, window time.Duration) (*WarningQueue, *time.Time) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	q := NewWarningQueue(max, window)
	q.nowFunc = func() time.Time { return now }
	return q, &now
}

func TestWarningQueue_Dedup_AllowsUpToMax(t *testing.T) {
	q, _ := newTestQueue(3, 5*time.Minute)

	for i := 0; i < 3; i++ {
		q.Push("WARN", "telegram", "context deadline exceeded")
	}

	warnings := q.Drain()
	if len(warnings) != 3 {
		t.Fatalf("got %d warnings, want 3", len(warnings))
	}
	for _, w := range warnings {
		if !strings.Contains(w, "context deadline exceeded") {
			t.Errorf("unexpected warning: %q", w)
		}
	}
}

func TestWarningQueue_Dedup_SuppressesAfterMax(t *testing.T) {
	q, _ := newTestQueue(2, 5*time.Minute)

	for i := 0; i < 10; i++ {
		q.Push("WARN", "telegram", "context deadline exceeded")
	}

	if q.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 (suppressed 8)", q.Len())
	}

	warnings := q.Drain()
	// 2 allowed + 1 summary
	if len(warnings) != 3 {
		t.Fatalf("got %d warnings, want 3 (2 allowed + 1 summary)", len(warnings))
	}
	if !strings.Contains(warnings[2], "... and 8 more") {
		t.Errorf("summary = %q, want to contain '... and 8 more'", warnings[2])
	}
}

func TestWarningQueue_Dedup_WindowExpiry(t *testing.T) {
	q, now := newTestQueue(2, 5*time.Minute)

	// Fill window
	q.Push("WARN", "telegram", "error 42")
	q.Push("WARN", "telegram", "error 42")
	q.Push("WARN", "telegram", "error 42") // suppressed

	// Advance past window
	*now = now.Add(6 * time.Minute)

	// Should be allowed again (new window)
	q.Push("WARN", "telegram", "error 42")

	warnings := q.Drain()
	// 2 from first window + 1 summary + 1 from new window = 4
	if len(warnings) != 4 {
		t.Fatalf("got %d warnings, want 4", len(warnings))
	}
}

func TestWarningQueue_Dedup_DifferentKeysIndependent(t *testing.T) {
	q, _ := newTestQueue(1, 5*time.Minute)

	q.Push("WARN", "telegram", "error A")
	q.Push("WARN", "telegram", "error A") // suppressed
	q.Push("WARN", "config", "error A")   // different component = different key
	q.Push("ERROR", "telegram", "error A") // different level = different key

	if q.Len() != 3 {
		t.Errorf("Len() = %d, want 3", q.Len())
	}
}

func TestWarningQueue_Dedup_NormalizationGroups(t *testing.T) {
	q, _ := newTestQueue(1, 5*time.Minute)

	// These should all normalize to the same key
	q.Push("WARN", "telegram", "timeout after 30s on 192.168.1.1")
	q.Push("WARN", "telegram", "timeout after 60s on 10.0.0.1")    // suppressed (same normalized)
	q.Push("WARN", "telegram", "timeout after 120s on 172.16.0.5") // suppressed

	if q.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (normalization should group these)", q.Len())
	}

	warnings := q.Drain()
	// 1 allowed + 1 summary
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want 2", len(warnings))
	}
	if !strings.Contains(warnings[1], "... and 2 more") {
		t.Errorf("summary = %q, want '... and 2 more'", warnings[1])
	}
}

func TestWarningQueue_Dedup_DrainResetsSuppressed(t *testing.T) {
	q, _ := newTestQueue(1, 5*time.Minute)

	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	q.Push("WARN", "test", "error") // suppressed

	warnings := q.Drain()
	if len(warnings) != 2 { // 1 allowed + 1 summary
		t.Fatalf("first drain: got %d, want 2", len(warnings))
	}

	// Push more in same window — bucket still active, already at max
	q.Push("WARN", "test", "error") // suppressed
	q.Push("WARN", "test", "error") // suppressed

	warnings = q.Drain()
	if len(warnings) != 1 { // just 1 summary (no new allowed)
		t.Fatalf("second drain: got %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], "... and 2 more") {
		t.Errorf("summary = %q, want '... and 2 more'", warnings[0])
	}
}

func TestWarningQueue_Dedup_DrainPrunesExpired(t *testing.T) {
	q, now := newTestQueue(1, 5*time.Minute)

	q.Push("WARN", "test", "error")

	// Advance past window
	*now = now.Add(6 * time.Minute)

	q.Drain() // should prune the expired bucket

	// Verify: new push should work as a fresh bucket
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed

	warnings := q.Drain()
	// 1 allowed + 1 summary
	if len(warnings) != 2 {
		t.Fatalf("got %d, want 2 (bucket should have been pruned and recreated)", len(warnings))
	}
}

// --- Pending() tests ---

func TestWarningQueue_Pending_Empty(t *testing.T) {
	q := NewWarningQueue(0, 0)
	if q.Pending() {
		t.Error("Pending() on empty queue should be false")
	}
}

func TestWarningQueue_Pending_WithWarnings(t *testing.T) {
	q := NewWarningQueue(0, 0)
	q.Push("WARN", "test", "something happened")
	if !q.Pending() {
		t.Error("Pending() with queued warnings should be true")
	}
}

func TestWarningQueue_Pending_SuppressedOnly(t *testing.T) {
	q, _ := newTestQueue(1, 5*time.Minute)

	// One allowed, two suppressed
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	q.Push("WARN", "test", "error") // suppressed

	// Drain the queued warning (but not summaries — Drain handles both)
	// After drain, suppressed count resets, so Pending should be false.
	warnings := q.Drain()
	if len(warnings) != 2 { // 1 allowed + 1 summary
		t.Fatalf("Drain() got %d, want 2", len(warnings))
	}

	// Push more suppressed (within same window, bucket still at max)
	q.Push("WARN", "test", "error") // suppressed
	if !q.Pending() {
		t.Error("Pending() should be true when only suppressed warnings exist")
	}
}

func TestWarningQueue_Pending_AfterDrain(t *testing.T) {
	q := NewWarningQueue(0, 0)
	q.Push("WARN", "test", "something")
	q.Drain()
	if q.Pending() {
		t.Error("Pending() after Drain() should be false")
	}
}

// --- LastUserMessageTime tests ---

func TestLastUserMessageTime_Default(t *testing.T) {
	a := &Agent{}
	got := a.LastUserMessageTime("test-session")
	if !got.IsZero() {
		t.Errorf("LastUserMessageTime for new session = %v, want zero", got)
	}
}

func TestLastUserMessageTime_AfterSeed(t *testing.T) {
	a := &Agent{}
	sm := a.getSessionMeta("test-session")
	now := time.Now()
	sm.lastMessageTime = now

	got := a.LastUserMessageTime("test-session")
	if !got.Equal(now) {
		t.Errorf("LastUserMessageTime = %v, want %v", got, now)
	}
}

func TestIsSystemMessage_ProactiveWarnings(t *testing.T) {
	if !isSystemMessage("[proactive system warnings]\n- [WARN] disk full") {
		t.Error("isSystemMessage should recognize proactive system warnings prefix")
	}
	if isSystemMessage("Hello, how are you?") {
		t.Error("isSystemMessage should not match regular messages")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{0, "0ms"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
