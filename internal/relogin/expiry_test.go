package relogin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// recorder is a send func that records notices and answers with ok.
type recorder struct {
	ok      bool
	notices []string
}

func (r *recorder) send(n string) bool {
	r.notices = append(r.notices, n)
	return r.ok
}

func newWarner(t *testing.T) (*ExpiryWarner, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	return NewExpiryWarner(path), path
}

// TestExpiryWarner_Stages: nothing outside the window, one notice on entering
// it, one more when the sign-in can end at any time, and no repeats.
func TestExpiryWarner_Stages(t *testing.T) {
	t.Parallel()
	w, _ := newWarner(t)
	r := &recorder{ok: true}
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

	w.Observe(exp, exp.Add(-11*day), r.send)
	if len(r.notices) != 0 {
		t.Fatalf("notice outside the window: %q", r.notices)
	}
	w.Observe(exp, exp.Add(-9*day), r.send)
	if len(r.notices) != 1 {
		t.Fatalf("got %d notices on entering the window, want 1", len(r.notices))
	}
	for _, want := range []string{"2026-11-28", "2026-12-01", "6 days", "/login"} {
		if !strings.Contains(r.notices[0], want) {
			t.Errorf("first notice lacks %q: %s", want, r.notices[0])
		}
	}
	w.Observe(exp, exp.Add(-9*day+time.Hour), r.send)
	w.Observe(exp, exp.Add(-5*day), r.send)
	if len(r.notices) != 1 {
		t.Fatalf("repeat notice in the same stage: %q", r.notices)
	}
	w.Observe(exp, exp.Add(-3*day-time.Hour), r.send)
	if len(r.notices) != 2 {
		t.Fatalf("got %d notices at the urgent stage, want 2", len(r.notices))
	}
	if !strings.Contains(r.notices[1], "any time") {
		t.Errorf("urgent notice: %s", r.notices[1])
	}
	w.Observe(exp, exp.Add(-time.Hour), r.send)
	w.Observe(exp, exp.Add(time.Hour), r.send)
	if len(r.notices) != 2 {
		t.Fatalf("repeat notice after the urgent stage: %q", r.notices)
	}
}

// TestExpiryWarner_FirstSightLate: first seen inside the urgent stage, or past
// the date, it sends one notice, not one per stage it skipped.
func TestExpiryWarner_FirstSightLate(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"urgent", exp.Add(-2 * day), "any time"},
		{"past", exp.Add(2 * day), "may have ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, _ := newWarner(t)
			r := &recorder{ok: true}
			w.Observe(exp, tc.now, r.send)
			w.Observe(exp, tc.now.Add(time.Hour), r.send)
			if len(r.notices) != 1 || !strings.Contains(r.notices[0], tc.want) {
				t.Errorf("notices %q, want one containing %q", r.notices, tc.want)
			}
		})
	}
}

// TestExpiryWarner_ZeroDateNoNotice: no date means no notice and no state.
func TestExpiryWarner_ZeroDateNoNotice(t *testing.T) {
	t.Parallel()
	w, path := newWarner(t)
	r := &recorder{ok: true}
	w.Observe(time.Time{}, time.Now(), r.send)
	if len(r.notices) != 0 {
		t.Errorf("notice for a zero date: %q", r.notices)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file written for a zero date: %v", err)
	}
}

// TestExpiryWarner_UndeliveredRetries: a notice nobody received is not recorded,
// so the next observation tries again (a chat can still be connecting at the
// startup probe).
func TestExpiryWarner_UndeliveredRetries(t *testing.T) {
	t.Parallel()
	w, path := newWarner(t)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	fail := &recorder{ok: false}
	w.Observe(exp, exp.Add(-9*day), fail.send)
	if len(fail.notices) != 1 {
		t.Fatalf("got %d send attempts, want 1", len(fail.notices))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state recorded for an undelivered notice: %v", err)
	}
	r := &recorder{ok: true}
	w.Observe(exp, exp.Add(-9*day), r.send)
	if len(r.notices) != 1 {
		t.Errorf("no retry after a failed delivery: %q", r.notices)
	}
}

// TestExpiryWarner_SurvivesRestart: a new process does not repeat a stage, and
// still sends the next one.
func TestExpiryWarner_SurvivesRestart(t *testing.T) {
	t.Parallel()
	w, path := newWarner(t)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := &recorder{ok: true}
	w.Observe(exp, exp.Add(-9*day), r.send)

	again := NewExpiryWarner(path)
	r2 := &recorder{ok: true}
	again.Observe(exp, exp.Add(-8*day), r2.send)
	if len(r2.notices) != 0 {
		t.Errorf("repeat after restart: %q", r2.notices)
	}
	again.Observe(exp, exp.Add(-2*day), r2.send)
	if len(r2.notices) != 1 {
		t.Errorf("next stage after restart: got %q", r2.notices)
	}
}

// TestExpiryWarner_CorruptState: an unreadable state file means no memory, not
// a failure.
func TestExpiryWarner_CorruptState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewExpiryWarner(path)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := &recorder{ok: true}
	w.Observe(exp, exp.Add(-9*day), r.send)
	if len(r.notices) != 1 {
		t.Errorf("notices %q, want 1", r.notices)
	}
}

// TestExpiryWarner_NewLogin: a later date is a new sign-in, so its stages start
// again; a process still reporting the old date afterwards stays quiet.
func TestExpiryWarner_NewLogin(t *testing.T) {
	t.Parallel()
	w, _ := newWarner(t)
	oldExp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	newExp := oldExp.Add(30 * day)
	r := &recorder{ok: true}
	w.Observe(oldExp, oldExp.Add(-2*day), r.send)
	if len(r.notices) != 1 {
		t.Fatalf("setup: %q", r.notices)
	}
	now := oldExp.Add(-day)
	w.Observe(newExp, now, r.send)
	if len(r.notices) != 1 {
		t.Fatalf("notice for a date far ahead: %q", r.notices)
	}
	w.Observe(oldExp, now, r.send)
	if len(r.notices) != 1 {
		t.Fatalf("notice for the old date after a new login: %q", r.notices)
	}
	w.Observe(newExp, newExp.Add(-9*day), r.send)
	if len(r.notices) != 2 {
		t.Fatalf("got %d notices for the new login's window, want 2 in all", len(r.notices))
	}
}

// TestExpiryWarner_SmallDriftIsSameLogin: a date that moves by minutes is the
// same sign-in and does not start the stages again.
func TestExpiryWarner_SmallDriftIsSameLogin(t *testing.T) {
	t.Parallel()
	w, _ := newWarner(t)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	r := &recorder{ok: true}
	w.Observe(exp, exp.Add(-9*day), r.send)
	w.Observe(exp.Add(10*time.Minute), exp.Add(-9*day), r.send)
	w.Observe(exp.Add(-10*time.Minute), exp.Add(-9*day), r.send)
	if len(r.notices) != 1 {
		t.Errorf("notices %q, want 1", r.notices)
	}
}

// TestExpiryWarner_ConcurrentObserversSendOnce: every agent shares the
// credential, so results can report the date while a notice is on its way. They
// must neither send a second one nor wait for a slow send: this runs on the
// path that reads Claude Code's stdout.
func TestExpiryWarner_ConcurrentObserversSendOnce(t *testing.T) {
	t.Parallel()
	w, _ := newWarner(t)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	now := exp.Add(-9 * day)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Observe(exp, now, func(string) bool {
			close(entered)
			<-release
			return true
		})
	}()
	<-entered
	second := &recorder{ok: true}
	w.Observe(exp, now, second.send) // would hang here if it waited for the first
	close(release)
	<-done
	w.Observe(exp, now, second.send)
	if len(second.notices) != 0 {
		t.Errorf("second observer sent %q", second.notices)
	}
}
