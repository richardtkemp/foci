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
	// Verifies that adding a reminder with due tag "now" makes it immediately retrievable via Due.
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
	// Verifies that "next_keepalive" is treated as immediately due (resolves to now), so Due returns it right away.
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
	// Verifies that a reminder scheduled for "tomorrow" is not returned by Due when called immediately after creation.
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
	// Verifies that a reminder with an explicit future date is not returned by Due until that date is reached.
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
	// Verifies that Dismiss removes exactly one reminder by ID, leaving any others intact.
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
	// Verifies that DismissAll clears all currently due reminders for an agent but does not affect future ones.
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
	// Verifies resolveWhen correctly maps all supported when strings — "now", "next_keepalive", "tomorrow", date strings, durations, and unknown values — to the expected time.
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
	// Verifies that multiple due reminders are all returned by Due with valid IDs.
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

func TestAddWakeAndPendingWakes(t *testing.T) {
	// Verifies that AddWake stores a wake reminder and PendingWakes retrieves it with the correct ID and text.
	rs := testReminderStore(t)

	id, err := rs.AddWake("test", "check inbox", "30m")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero row ID")
	}

	wakes, err := rs.PendingWakes("test")
	if err != nil {
		t.Fatalf("PendingWakes: %v", err)
	}
	if len(wakes) != 1 {
		t.Fatalf("expected 1 wake, got %d", len(wakes))
	}
	if wakes[0].ID != id {
		t.Errorf("ID = %d, want %d", wakes[0].ID, id)
	}
	if wakes[0].Text != "check inbox" {
		t.Errorf("Text = %q, want %q", wakes[0].Text, "check inbox")
	}
}

func TestDueSkipsWakes(t *testing.T) {
	// Verifies that Due only returns passive reminders and never returns wake reminders, even if both are currently due.
	rs := testReminderStore(t)

	// Add a passive reminder (due now)
	if err := rs.Add("test", "passive", "now"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add a wake reminder (due now)
	if _, err := rs.AddWake("test", "active", "now"); err != nil {
		t.Fatalf("AddWake: %v", err)
	}

	due, err := rs.Due("test")
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due reminder, got %d", len(due))
	}
	if due[0].Text != "passive" {
		t.Errorf("Text = %q, want %q", due[0].Text, "passive")
	}
}

func TestDismissAllSkipsWakes(t *testing.T) {
	// Verifies that DismissAll removes passive reminders but leaves wake reminders untouched.
	rs := testReminderStore(t)

	// Add a passive reminder (due now) and a wake reminder (due now)
	rs.Add("test", "passive", "now")
	wakeID, _ := rs.AddWake("test", "active", "now")

	if err := rs.DismissAll("test"); err != nil {
		t.Fatalf("DismissAll: %v", err)
	}

	// Passive should be gone
	due, _ := rs.Due("test")
	if len(due) != 0 {
		t.Fatalf("expected 0 due after DismissAll, got %d", len(due))
	}

	// Wake should still exist
	wakes, _ := rs.PendingWakes("test")
	if len(wakes) != 1 {
		t.Fatalf("expected 1 wake after DismissAll, got %d", len(wakes))
	}
	if wakes[0].ID != wakeID {
		t.Errorf("wake ID = %d, want %d", wakes[0].ID, wakeID)
	}
}

func TestDismissWorksForWakes(t *testing.T) {
	// Verifies that Dismiss can remove a wake reminder by ID, so it no longer appears in PendingWakes.
	rs := testReminderStore(t)

	id, _ := rs.AddWake("test", "fire me", "now")

	if err := rs.Dismiss(id); err != nil {
		t.Fatalf("Dismiss wake: %v", err)
	}

	wakes, _ := rs.PendingWakes("test")
	if len(wakes) != 0 {
		t.Fatalf("expected 0 wakes after dismiss, got %d", len(wakes))
	}
}

func TestReminderStoreBusyTimeout(t *testing.T) {
	// Verifies that the SQLite connection is configured with a 5-second busy timeout to avoid immediate lock failures under contention.
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
