package warnings

import (
	"strings"
	"testing"
	"time"
)

func TestQueue_PushAndDrain(t *testing.T) {
	// Proves that pushed warnings are preserved in order and drained correctly, and that a second drain returns nil.
	q := NewQueue(0, 0)

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

func TestQueue_DrainEmpty(t *testing.T) {
	// Proves that draining a queue that has never received warnings returns nil rather than an empty slice.
	q := NewQueue(0, 0)
	if warnings := q.Drain(); warnings != nil {
		t.Errorf("Drain() on empty queue = %v, want nil", warnings)
	}
}

func TestQueue_MaxSize(t *testing.T) {
	// Proves that the queue hard-caps at maxSize entries, discarding pushes beyond that limit.
	q := NewQueue(0, 0)
	q.maxSize = 3

	for i := 0; i < 10; i++ {
		q.Push("WARN", "test", "msg")
	}

	if q.Len() != 3 {
		t.Errorf("Len() = %d, want 3 (max size)", q.Len())
	}
}

func TestQueue_Format(t *testing.T) {
	// Proves that warnings are formatted as "[LEVEL] [component] message" with no extra whitespace or reordering.
	q := NewQueue(0, 0)
	q.Push("WARN", "config", "unknown key: foo.bar")

	warnings := q.Drain()
	expected := "[WARN] [config] unknown key: foo.bar"
	if warnings[0] != expected {
		t.Errorf("format = %q, want %q", warnings[0], expected)
	}
}

// --- Normalization tests ---

func TestNormalizeWarning(t *testing.T) {
	// Proves that NormalizeWarning strips volatile tokens (multi-digit numbers, hex IDs, IP addresses) to canonical placeholders, leaving unchanged text that has no volatile parts.
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

func newTestQueue(max int, window time.Duration) (*Queue, *time.Time) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	q := NewQueue(max, window)
	q.nowFunc = func() time.Time { return now }
	return q, &now
}

func TestQueue_Dedup_AllowsUpToMax(t *testing.T) {
	// Proves that repeated identical warnings are all accepted when the count stays within the per-window max.
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

func TestQueue_Dedup_SuppressesAfterMax(t *testing.T) {
	// Proves that pushes beyond the per-window max are suppressed, and a summary "... and N more" entry is appended on drain.
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

func TestQueue_Dedup_WindowExpiry(t *testing.T) {
	// Proves that after the dedup window expires without saturation, the bucket resets and the same warning is allowed through again.
	q, now := newTestQueue(2, 5*time.Minute)

	// Fill window (not saturated — only 1 of 2 allowed)
	q.Push("WARN", "telegram", "error 42")

	// Advance past window
	*now = now.Add(6 * time.Minute)

	// Should be allowed again (non-saturated window → normal reset)
	q.Push("WARN", "telegram", "error 42")

	warnings := q.Drain()
	// 1 from first window + 1 from new window = 2
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want 2", len(warnings))
	}
}

func TestQueue_Dedup_DifferentKeysIndependent(t *testing.T) {
	// Proves that dedup buckets are keyed on (level, component, normalised message), so the same message text with a different component or level counts as a distinct warning.
	q, _ := newTestQueue(1, 5*time.Minute)

	q.Push("WARN", "telegram", "error A")
	q.Push("WARN", "telegram", "error A") // suppressed
	q.Push("WARN", "config", "error A")   // different component = different key
	q.Push("ERROR", "telegram", "error A") // different level = different key

	if q.Len() != 3 {
		t.Errorf("Len() = %d, want 3", q.Len())
	}
}

func TestQueue_Dedup_NormalizationGroups(t *testing.T) {
	// Proves that messages differing only in volatile tokens (numbers, IPs) are grouped into the same dedup bucket and collectively suppressed after the first.
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

func TestQueue_Dedup_DrainResetsSuppressed(t *testing.T) {
	// Proves that after a drain clears queued warnings, subsequent suppressed pushes within the same window accumulate a new summary that is emitted on the next drain.
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

func TestQueue_Dedup_DrainPrunesExpired(t *testing.T) {
	// Proves that draining after a window expires removes the stale bucket, so the next push starts a fresh dedup window rather than inheriting old state.
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

func TestQueue_Pending_Empty(t *testing.T) {
	// Proves that Pending returns false on a freshly created queue with no warnings.
	q := NewQueue(0, 0)
	if q.Pending() {
		t.Error("Pending() on empty queue should be false")
	}
}

func TestQueue_Pending_WithWarnings(t *testing.T) {
	// Proves that Pending returns true once at least one warning has been pushed and not yet drained.
	q := NewQueue(0, 0)
	q.Push("WARN", "test", "something happened")
	if !q.Pending() {
		t.Error("Pending() with queued warnings should be true")
	}
}

func TestQueue_Pending_SuppressedOnly(t *testing.T) {
	// Proves that Pending returns true even when all new pushes are suppressed (no queued entry), because suppressed counts create a pending summary.
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

func TestQueue_Pending_AfterDrain(t *testing.T) {
	// Proves that Pending returns false immediately after a drain empties the queue.
	q := NewQueue(0, 0)
	q.Push("WARN", "test", "something")
	q.Drain()
	if q.Pending() {
		t.Error("Pending() after Drain() should be false")
	}
}

// --- Quiet mode tests ---

func TestQueue_QuietMode_EntersAfterSaturatedWindow(t *testing.T) {
	// Proves that when a window expires having been saturated, the next push triggers quiet mode: the queued warnings plus a summary are retained but subsequent pushes are silently dropped.
	q, now := newTestQueue(3, 5*time.Minute)

	// Fill and saturate window (3 allowed + 1 suppressed)
	for i := 0; i < 4; i++ {
		q.Push("WARN", "telegram", "context deadline exceeded")
	}
	if q.Len() != 3 {
		t.Fatalf("Len() = %d, want 3 after saturation", q.Len())
	}

	// Advance past window
	*now = now.Add(6 * time.Minute)

	// Push again — saturated window expired → enters quiet mode
	q.Push("WARN", "telegram", "context deadline exceeded")

	// 3 allowed + 1 summary flushed on quiet entry = 4 queued
	// (the push itself was suppressed into quiet mode)
	if q.Len() != 4 {
		t.Fatalf("Len() = %d, want 4 (3 allowed + 1 summary)", q.Len())
	}

	warnings := q.Drain()
	// Drain skips quiet bucket within its window
	if len(warnings) != 4 {
		t.Fatalf("Drain() got %d warnings, want 4", len(warnings))
	}
	if !strings.Contains(warnings[3], "... and 1 more") {
		t.Errorf("summary = %q, want '... and 1 more'", warnings[3])
	}
}

func TestQueue_QuietMode_PendingIgnoresQuiet(t *testing.T) {
	// Proves that Pending returns false when suppressions exist only within an active quiet bucket, preventing spurious dispatch wake-ups.
	q, now := newTestQueue(1, 5*time.Minute)

	// Saturate and expire
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	*now = now.Add(6 * time.Minute)
	q.Push("WARN", "test", "error") // enters quiet mode

	// Drain to clear queued warnings
	q.Drain()

	// Push more — suppressed into quiet bucket
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error")

	// Pending should ignore quiet buckets
	if q.Pending() {
		t.Error("Pending() should be false when only quiet buckets have suppressions")
	}
}

func TestQueue_QuietMode_DrainSkipsActiveQuiet(t *testing.T) {
	// Proves that Drain returns nil when the only pending suppressions are inside an active quiet bucket, deferring them until the quiet window expires.
	q, now := newTestQueue(1, 5*time.Minute)

	// Enter quiet mode
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	*now = now.Add(6 * time.Minute)
	q.Push("WARN", "test", "error") // quiet mode

	q.Drain() // clear queued

	// Push more suppressions into quiet bucket
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error")

	// Drain should skip quiet bucket within its window
	warnings := q.Drain()
	if warnings != nil {
		t.Errorf("Drain() = %v, want nil (quiet bucket should be skipped)", warnings)
	}
}

func TestQueue_QuietMode_DrainFlushesExpiredQuiet(t *testing.T) {
	// Proves that once the quiet window expires, Drain emits a summary of all suppressed messages and then prunes the bucket so Pending returns false.
	q, now := newTestQueue(1, 5*time.Minute)

	// Enter quiet mode
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	*now = now.Add(6 * time.Minute)
	q.Push("WARN", "test", "error") // quiet mode

	q.Drain() // clear queued

	// Push more into quiet bucket
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error")

	// Advance past quiet window (12 × 5m = 60m)
	*now = now.Add(61 * time.Minute)

	warnings := q.Drain()
	if len(warnings) != 1 {
		t.Fatalf("Drain() got %d warnings, want 1 (summary)", len(warnings))
	}
	if !strings.Contains(warnings[0], "... and") {
		t.Errorf("summary = %q, want to contain '... and'", warnings[0])
	}

	// Bucket should be pruned — Pending should be false
	if q.Pending() {
		t.Error("Pending() should be false after quiet bucket pruned")
	}
}

func TestQueue_QuietMode_QuietWindowRenewal(t *testing.T) {
	// Proves that when a push arrives after the quiet window expires, the suppressed summary is flushed and quiet mode restarts for the new window.
	q, now := newTestQueue(1, 5*time.Minute)

	// Enter quiet mode
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	*now = now.Add(6 * time.Minute)
	q.Push("WARN", "test", "error") // quiet mode

	q.Drain() // clear queued

	// Push more into quiet bucket
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error")

	// Advance past quiet window
	*now = now.Add(61 * time.Minute)

	// Push again — quiet window expired → flush summary, restart quiet
	q.Push("WARN", "test", "error")

	warnings := q.Drain()
	// 1 summary flushed during Push (quiet window expiry)
	// The push itself is suppressed into renewed quiet window
	// Drain skips renewed quiet bucket
	if len(warnings) != 1 {
		t.Fatalf("Drain() got %d warnings, want 1 (summary from expiry)", len(warnings))
	}
	if !strings.Contains(warnings[0], "... and") {
		t.Errorf("summary = %q, want to contain '... and'", warnings[0])
	}
}

func TestQueue_QuietMode_Recovery(t *testing.T) {
	// Proves that when no pushes occur during a quiet window and Drain is called after expiry, the bucket is deleted and the next push starts a completely fresh non-quiet bucket.
	q, now := newTestQueue(1, 5*time.Minute)

	// Enter quiet mode
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error") // suppressed
	*now = now.Add(6 * time.Minute)
	q.Push("WARN", "test", "error") // quiet mode

	q.Drain() // clear queued

	// Advance past quiet window — no more pushes (warning stopped)
	*now = now.Add(61 * time.Minute)
	q.Drain() // flushes summary, deletes bucket

	// New push should start a fresh non-quiet bucket
	q.Push("WARN", "test", "error")

	warnings := q.Drain()
	if len(warnings) != 1 {
		t.Fatalf("Drain() got %d, want 1 (fresh bucket, no quiet)", len(warnings))
	}
	if strings.Contains(warnings[0], "... and") {
		t.Errorf("warning = %q, should not be a summary (fresh bucket)", warnings[0])
	}
}

func TestQueue_QuietMode_NonSaturatedWindowNoQuiet(t *testing.T) {
	// Proves that a window that expired without being fully saturated resets normally (no quiet mode), allowing the warning through again.
	q, now := newTestQueue(3, 5*time.Minute)

	// Push 2 (below max of 3 — not saturated)
	q.Push("WARN", "test", "error")
	q.Push("WARN", "test", "error")

	// Advance past window
	*now = now.Add(6 * time.Minute)

	// Push again — non-saturated window → normal reset, allow through
	q.Push("WARN", "test", "error")

	warnings := q.Drain()
	// 2 from first window + 1 from new window = 3, no quiet mode
	if len(warnings) != 3 {
		t.Fatalf("Drain() got %d, want 3", len(warnings))
	}
	for _, w := range warnings {
		if strings.Contains(w, "... and") {
			t.Errorf("unexpected summary: %q (non-saturated should not trigger quiet)", w)
		}
	}
}

// --- ErrorsOnly tests ---

func TestQueue_ErrorsOnly_DropsWarn(t *testing.T) {
	// Proves that when errorsOnly is set, WARN-level entries are silently dropped
	// while ERROR-level entries pass through normally.
	q := NewQueue(0, 0)
	q.SetErrorsOnly(true)

	q.Push("WARN", "config", "unknown key: foo")
	q.Push("ERROR", "telegram", "fatal: connection lost")
	q.Push("WARN", "disk", "getting full")

	warnings := q.Drain()
	if len(warnings) != 1 {
		t.Fatalf("Drain() returned %d warnings, want 1 (only ERROR)", len(warnings))
	}
	if !strings.Contains(warnings[0], "fatal: connection lost") {
		t.Errorf("warnings[0] = %q, want ERROR entry", warnings[0])
	}
}

func TestQueue_ErrorsOnly_AllowsAllWhenFalse(t *testing.T) {
	// Proves that when errorsOnly is false (default), both WARN and ERROR entries pass through.
	q := NewQueue(0, 0)

	q.Push("WARN", "config", "unknown key")
	q.Push("ERROR", "telegram", "fatal error")

	warnings := q.Drain()
	if len(warnings) != 2 {
		t.Fatalf("Drain() returned %d warnings, want 2", len(warnings))
	}
}

// --- FormatList tests ---

func TestFormatList(t *testing.T) {
	// Proves that FormatList produces a newline-separated bullet list.
	tests := []struct {
		name    string
		entries []string
		want    string
	}{
		{"single", []string{"foo"}, "- foo"},
		{"multiple", []string{"a", "b", "c"}, "- a\n- b\n- c"},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatList(tt.entries)
			if got != tt.want {
				t.Errorf("FormatList(%v) = %q, want %q", tt.entries, got, tt.want)
			}
		})
	}
}

// --- FormatDuration tests ---

func TestFormatDuration(t *testing.T) {
	// Proves that FormatDuration picks the largest whole unit (ms/s/m/h) and formats zero as "0ms".
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
			got := FormatDuration(tt.d)
			if got != tt.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
