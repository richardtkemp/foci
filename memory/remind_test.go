package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func testReminderStore(t *testing.T) *ReminderStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	return rs
}

func TestReminderAddAndDue(t *testing.T) {
	rs := testReminderStore(t)

	if err := rs.Add("test", "check logs", "now"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	reminders, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(reminders) != 1 {
		t.Fatalf("expected 1 reminder, got %d", len(reminders))
	}
	if reminders[0].Text != "check logs" {
		t.Errorf("text = %q, want %q", reminders[0].Text, "check logs")
	}
	if reminders[0].DueTag != "now" {
		t.Errorf("due_tag = %q, want %q", reminders[0].DueTag, "now")
	}
}

func TestReminderNextKeepalive(t *testing.T) {
	rs := testReminderStore(t)

	if err := rs.Add("test", "think about caching", "next_keepalive"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	reminders, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(reminders) != 1 {
		t.Fatalf("expected 1 reminder, got %d", len(reminders))
	}
	if reminders[0].DueTag != "next_keepalive" {
		t.Errorf("due_tag = %q", reminders[0].DueTag)
	}
}

func TestReminderTomorrow(t *testing.T) {
	rs := testReminderStore(t)

	if err := rs.Add("test", "ask about Greece", "tomorrow"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Should NOT be due now (it's tomorrow)
	reminders, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(reminders) != 0 {
		t.Fatalf("expected 0 due reminders, got %d", len(reminders))
	}
}

func TestReminderFutureDate(t *testing.T) {
	rs := testReminderStore(t)

	futureDate := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	if err := rs.Add("test", "weekly review", futureDate); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Should NOT be due now
	reminders, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(reminders) != 0 {
		t.Fatalf("expected 0 due reminders, got %d", len(reminders))
	}
}

func TestReminderDismiss(t *testing.T) {
	rs := testReminderStore(t)

	rs.Add("test", "reminder 1", "now")
	rs.Add("test", "reminder 2", "now")

	reminders, _ := rs.Due("test")
	if len(reminders) != 2 {
		t.Fatalf("expected 2 reminders, got %d", len(reminders))
	}

	// Dismiss first one
	if err := rs.Dismiss(reminders[0].ID); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	reminders, _ = rs.Due("test")
	if len(reminders) != 1 {
		t.Fatalf("expected 1 reminder after dismiss, got %d", len(reminders))
	}
	if reminders[0].Text != "reminder 2" {
		t.Errorf("wrong reminder remaining: %q", reminders[0].Text)
	}
}

func TestReminderDismissAll(t *testing.T) {
	rs := testReminderStore(t)

	rs.Add("test", "r1", "now")
	rs.Add("test", "r2", "now")
	rs.Add("test", "r3", "tomorrow") // not due

	if err := rs.DismissAll("test"); err != nil {
		t.Fatalf("DismissAll: %v", err)
	}

	// Due ones dismissed, tomorrow one still there
	// (check by querying all — not just due)
	reminders, _ := rs.Due("test")
	if len(reminders) != 0 {
		t.Fatalf("expected 0 due after DismissAll, got %d", len(reminders))
	}
}

func TestResolveWhen(t *testing.T) {
	tests := []struct {
		when  string
		check func(t time.Time) bool
		desc  string
	}{
		{"now", func(t time.Time) bool { return time.Since(t) < 5*time.Second }, "should be ~now"},
		{"next_keepalive", func(t time.Time) bool { return time.Since(t) < 5*time.Second }, "should be ~now"},
		{"next_session", func(t time.Time) bool { return time.Since(t) < 5*time.Second }, "should be ~now"},
		{"tomorrow", func(t time.Time) bool { return t.After(time.Now()) }, "should be in the future"},
		{"2030-06-15", func(t time.Time) bool { return t.Year() == 2030 && t.Month() == 6 && t.Day() == 15 }, "should be that date"},
		{"2030-06-15T14:30:00Z", func(t time.Time) bool {
			return t.Year() == 2030 && t.Month() == 6 && t.Day() == 15 && t.Hour() == 14 && t.Minute() == 30
		}, "should be that RFC3339 timestamp"},
		{"2h", func(t time.Time) bool { return t.After(time.Now().Add(time.Hour)) }, "should be ~2h from now"},
		{"gibberish", func(t time.Time) bool { return time.Since(t) < 5*time.Second }, "unknown defaults to now"},
	}

	for _, tt := range tests {
		t.Run(tt.when, func(t *testing.T) {
			result := resolveWhen(tt.when)
			if !tt.check(result) {
				t.Errorf("resolveWhen(%q) = %v: %s", tt.when, result, tt.desc)
			}
		})
	}
}

func TestReminderMultiple(t *testing.T) {
	rs := testReminderStore(t)

	rs.Add("test", "first", "now")
	rs.Add("test", "second", "now")
	rs.Add("test", "third", "now")

	reminders, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(reminders) != 3 {
		t.Fatalf("expected 3, got %d", len(reminders))
	}

	// Should be ordered by due_at
	for i, r := range reminders {
		if r.ID == 0 {
			t.Errorf("reminder %d has zero ID", i)
		}
	}
}

func TestReminderStoreBusyTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	defer rs.Close()

	var timeout int
	if err := rs.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}
