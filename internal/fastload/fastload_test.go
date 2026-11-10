package fastload

import (
	"strings"
	"testing"
	"time"
)

// fakeClock is a settable clock for the recorder.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestRecorder() (*Recorder, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	r := New()
	r.SetClock(c.now)
	return r, c
}

func loadFor(t *testing.T, r *Recorder, window time.Duration, target string) Load {
	t.Helper()
	for _, l := range r.Loads(window) {
		if l.Target == target {
			return l
		}
	}
	t.Fatalf("no load for %q in %v", target, r.Loads(window))
	return Load{}
}

// TestLoads_FourFigures drives a known pattern through the recorder and
// checks Dick's four figures: runs, in-use share of active time, queued share
// and queue length.
func TestLoads_FourFigures(t *testing.T) {
	r, c := newTestRecorder()
	tgt := Target("qwen3.6", "local")

	// Minute 0: a chat message, then one 20 s call alone.
	r.MarkActive()
	_, end1 := r.Begin(tgt, "auto chat name")
	c.add(20 * time.Second)
	end1(false)

	// Minute 2: three overlapping calls. A at 0 s, B at 5 s (1 ahead), C at
	// 10 s (2 ahead); all end at 30 s. Busy 30 s.
	c.t = time.Date(2026, 11, 10, 12, 2, 0, 0, time.UTC)
	_, a := r.Begin(tgt, "chat sections")
	c.add(5 * time.Second)
	_, b := r.Begin(tgt, "chat sections")
	c.add(5 * time.Second)
	_, cc := r.Begin(tgt, "auto chat name")
	if n := r.InFlight(tgt); n != 3 {
		t.Fatalf("in flight = %d, want 3", n)
	}
	c.add(20 * time.Second)
	a(false)
	b(true)
	cc(false)

	// Minute 4: a chat message with no call: active time, no busy time.
	c.t = time.Date(2026, 11, 10, 12, 4, 30, 0, time.UTC)
	r.MarkActive()
	c.t = time.Date(2026, 11, 10, 12, 5, 0, 0, time.UTC)

	l := loadFor(t, r, time.Hour, tgt)
	if l.Runs != 4 || l.ByTask["auto chat name"] != 2 || l.ByTask["chat sections"] != 2 {
		t.Errorf("runs = %d by task %v, want 4 (2 + 2)", l.Runs, l.ByTask)
	}
	if l.Failed != 1 {
		t.Errorf("failed = %d, want 1", l.Failed)
	}
	if l.Busy != 50*time.Second {
		t.Errorf("busy = %v, want 50s (20 s + the 30 s overlap counted once)", l.Busy)
	}
	if l.ActiveTime != 3*time.Minute {
		t.Errorf("active = %v, want 3m (minutes 0, 2, 4)", l.ActiveTime)
	}
	if got, want := l.InUsePct(), 100*50.0/180; got < want-0.01 || got > want+0.01 {
		t.Errorf("in use = %.2f%%, want %.2f%%", got, want)
	}
	if l.Queued != 2 || l.QueuedPct() != 50 {
		t.Errorf("queued = %d (%.0f%%), want 2 (50%%)", l.Queued, l.QueuedPct())
	}
	if l.QueueP50 != 1 || l.QueueMax != 2 {
		t.Errorf("queue length typical %d max %d, want 1 and 2", l.QueueP50, l.QueueMax)
	}
	if l.InFlight != 0 {
		t.Errorf("in flight now = %d, want 0", l.InFlight)
	}

	rep := r.Report()
	for _, want := range []string{"qwen3.6 @ local", "Runs: 4 | 4", "Queued: 50% | 50%", "typical 1 | 1, max 2 | 2"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
}

// TestLoads_Window: a call older than the window is not a run in it, and the
// record forgets everything after Retention.
func TestLoads_Window(t *testing.T) {
	r, c := newTestRecorder()
	tgt := Target("m", "e")
	_, end := r.Begin(tgt, "pin summary")
	c.add(time.Second)
	end(false)

	c.add(2 * time.Hour)
	if got := r.Loads(time.Hour); len(got) != 0 {
		t.Errorf("last hour = %v, want nothing", got)
	}
	if l := loadFor(t, r, Retention, tgt); l.Runs != 1 {
		t.Errorf("24h runs = %d, want 1", l.Runs)
	}
	c.add(Retention)
	if got := r.Loads(Retention); len(got) != 0 {
		t.Errorf("after retention = %v, want nothing", got)
	}
	if r.Report() != "" {
		t.Errorf("report = %q, want empty", r.Report())
	}
}

// TestLoads_InFlightCall: a call still running counts as busy up to now and
// as in flight, and a second end call is ignored.
func TestLoads_InFlightCall(t *testing.T) {
	r, c := newTestRecorder()
	tgt := Target("m", "e")
	_, end := r.Begin(tgt, "chat sections")
	c.add(10 * time.Second)
	l := loadFor(t, r, time.Hour, tgt)
	if l.InFlight != 1 || l.Busy != 10*time.Second {
		t.Errorf("in flight %d busy %v, want 1 and 10s", l.InFlight, l.Busy)
	}
	end(false)
	end(true)
	if n := r.InFlight(tgt); n != 0 {
		t.Errorf("in flight after end = %d, want 0", n)
	}
	if l := loadFor(t, r, time.Hour, tgt); l.Failed != 0 {
		t.Errorf("failed = %d: the second end call was not ignored", l.Failed)
	}
}

// TestLoads_PerHourAfterRestart: runs per hour divide by the time the record
// covers, not the whole window, after a restart.
func TestLoads_PerHourAfterRestart(t *testing.T) {
	r, c := newTestRecorder()
	tgt := Target("m", "e")
	for range 6 {
		_, end := r.Begin(tgt, "pin summary")
		c.add(10 * time.Minute)
		end(false)
	}
	l := loadFor(t, r, Retention, tgt)
	if got := l.PerHour(); got < 5.99 || got > 6.01 {
		t.Errorf("per hour = %.2f, want 6 (6 runs in the hour since start)", got)
	}
}

// TestLoads_Held: held calls (#3019) are counted per window with their
// typical and longest wait and the replaced ones; a target whose only calls
// were replaced is still reported.
func TestLoads_Held(t *testing.T) {
	r, c := newTestRecorder()
	tgt := Target("qwen3.8", "local")
	other := Target("qwen3.6", "local")

	r.Held(tgt, "auto chat name", 10*time.Second, false)
	_, end := r.Begin(tgt, "auto chat name")
	c.add(time.Second)
	end(false)
	r.Held(tgt, "chat sections", 30*time.Second, true)
	r.Held(tgt, "chat sections", 2*time.Second, false)
	r.Held(other, "chat sections", 5*time.Second, true)

	l := loadFor(t, r, time.Hour, tgt)
	if l.Held != 3 || l.Replaced != 1 || l.HoldP50 != 10*time.Second || l.HoldMax != 30*time.Second {
		t.Errorf("held %d replaced %d p50 %v max %v, want 3, 1, 10s, 30s", l.Held, l.Replaced, l.HoldP50, l.HoldMax)
	}
	if o := loadFor(t, r, time.Hour, other); o.Runs != 0 || o.Held != 1 || o.Replaced != 1 {
		t.Errorf("replaced-only target: %+v", o)
	}
	rep := r.Report()
	if !strings.Contains(rep, "Held by foci: 3 | 3; wait typical 10s | 10s, max 30s | 30s; replaced 1 | 1") {
		t.Errorf("report:\n%s", rep)
	}

	c.add(2 * time.Hour) // out of the hour window, still in the day
	if l := loadFor(t, r, Retention, tgt); l.Held != 3 {
		t.Errorf("24h held = %d, want 3", l.Held)
	}
	for _, l := range r.Loads(time.Hour) {
		if l.Held != 0 {
			t.Errorf("hour window still holds %d held calls of %s", l.Held, l.Target)
		}
	}
	c.add(Retention)
	if got := r.Loads(Retention); len(got) != 0 {
		t.Errorf("after retention: %+v", got)
	}
}
