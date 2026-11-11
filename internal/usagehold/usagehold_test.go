package usagehold

import (
	"sync"
	"testing"
	"time"
)

func util(window string, u float64, reset time.Time) Event {
	return Event{Window: window, Util: u, HasUtil: true, ResetAt: reset}
}

func TestObserve_DefaultThreshold(t *testing.T) {
	now := time.Now()
	r := New()
	if st := r.Observe("ccstream", util("seven_day", 0.97, now.Add(12*time.Hour)), now); st.Phase != Open {
		t.Errorf("0.97 at the default threshold: phase %v, want open", st.Phase)
	}
	if st := r.Observe("ccstream", util("seven_day", 0.98, now.Add(12*time.Hour)), now); st.Phase != Hold {
		t.Errorf("0.98 at the default threshold: phase %v, want hold", st.Phase)
	}
	if st := r.State("codex", now); st.Phase != Open {
		t.Errorf("another account: phase %v, want open", st.Phase)
	}
}

func TestSetThreshold_AppliesToNextEvent(t *testing.T) {
	now := time.Now()
	r := New()
	if st := r.Observe("ccstream", util("seven_day", 0.96, now.Add(time.Hour)), now); st.Phase != Open {
		t.Fatalf("0.96 at 98%%: phase %v, want open", st.Phase)
	}
	r.SetThresholdPercent(95)
	if st := r.Observe("ccstream", util("seven_day", 0.96, now.Add(time.Hour)), now); st.Phase != Hold {
		t.Errorf("0.96 after a live edit to 95: phase %v, want hold", st.Phase)
	}
	r.SetThresholdPercent(0) // out of range: ignored
	if got := r.ThresholdPercent(); got != 95 {
		t.Errorf("threshold after an out-of-range set = %d, want 95", got)
	}
}

// TestPerWindowClearing is worked example B.
func TestPerWindowClearing(t *testing.T) {
	t0 := time.Now()
	r := New()
	r.Observe("ccstream", util("seven_day", 0.98, t0.Add(12*time.Hour)), t0)
	r.Observe("ccstream", util("five_hour", 0.98, t0.Add(3*time.Hour)), t0.Add(time.Hour))
	if st := r.Observe("ccstream", util("five_hour", 0.20, t0.Add(8*time.Hour)), t0.Add(4*time.Hour)); st.Phase != Hold {
		t.Fatalf("five-hour cleared, weekly at 98%%: phase %v, want hold", st.Phase)
	}
	if st := r.Observe("ccstream", util("seven_day", 0.99, t0.Add(12*time.Hour)), t0.Add(5*time.Hour)); st.Phase != Hold {
		t.Fatalf("weekly at 0.99: phase %v, want hold", st.Phase)
	}
	st := r.State("ccstream", t0.Add(11*time.Hour))
	if st.Phase != Hold || !st.LiftAt.Equal(t0.Add(12*time.Hour)) {
		t.Fatalf("before the weekly reset: %+v, want hold lifting at the reset", st)
	}
	if st := r.State("ccstream", t0.Add(12*time.Hour)); st.Phase != Open {
		t.Errorf("at the weekly reset: phase %v, want open", st.Phase)
	}
	// A weekly event under the threshold also clears it.
	r.Observe("ccstream", util("seven_day", 0.98, t0.Add(30*time.Hour)), t0.Add(13*time.Hour))
	if st := r.Observe("ccstream", util("seven_day", 0.50, t0.Add(30*time.Hour)), t0.Add(14*time.Hour)); st.Phase != Open {
		t.Errorf("weekly event at 0.50: phase %v, want open", st.Phase)
	}
}

func TestReachedStops(t *testing.T) {
	now := time.Now()
	r := New()
	r.Observe("ccstream", util("seven_day", 0.98, now.Add(5*time.Hour)), now)
	st := r.Observe("ccstream", Event{Window: "seven_day", Reached: true, ResetAt: now.Add(5 * time.Hour)}, now)
	if st.Phase != Stop {
		t.Fatalf("reached: phase %v, want stop", st.Phase)
	}
	// A reached window with no reset time uses the one-hour fallback.
	r2 := New()
	if st := r2.Observe("codex", Event{Window: "codex", Reached: true}, now); st.Phase != Stop || !st.LiftAt.Equal(now.Add(time.Hour)) {
		t.Errorf("reached without reset: %+v, want stop lifting in 1h", st)
	}
}

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) LoadUsageHold(a string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[a]
	return v, ok
}
func (s *memStore) SaveUsageHold(a, v string) { s.mu.Lock(); s.m[a] = v; s.mu.Unlock() }

func TestStateSurvivesRestart(t *testing.T) {
	now := time.Now()
	store := &memStore{m: map[string]string{}}
	r := New()
	r.SetStore(store)
	r.Observe("ccstream", util("seven_day", 0.98, now.Add(12*time.Hour)), now)

	restarted := New()
	restarted.SetStore(store)
	if st := restarted.State("ccstream", now.Add(time.Hour)); st.Phase != Hold || st.LiftAt.Sub(now.Add(12*time.Hour)).Abs() > time.Second {
		t.Errorf("after a restart: %+v, want hold until the reset", st)
	}
}

func TestSubscribersSeeEachChangeOnce(t *testing.T) {
	now := time.Now()
	r := New()
	var changes []Change
	r.Subscribe("ccstream", func(c Change) { changes = append(changes, c) })
	r.Observe("ccstream", util("seven_day", 0.98, now.Add(time.Hour)), now)
	r.Observe("ccstream", util("seven_day", 0.99, now.Add(time.Hour)), now)
	r.Observe("ccstream", Event{Window: "seven_day", Reached: true, ResetAt: now.Add(time.Hour)}, now)
	r.Tick(now.Add(2 * time.Hour))
	want := []Phase{Hold, Stop, Open}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %v", changes, want)
	}
	for i, p := range want {
		if changes[i].To.Phase != p {
			t.Errorf("change %d = %v, want %v", i, changes[i].To.Phase, p)
		}
	}
}

// TestClear_LiftsAndPersists is the forced lift (#3043): it clears every
// window at once, tells the subscribers, and the cleared state survives a
// restart.
func TestClear_LiftsAndPersists(t *testing.T) {
	now := time.Now()
	store := &memStore{m: map[string]string{}}
	r := New()
	r.SetStore(store)
	var changes []Change
	r.Subscribe("ccstream", func(c Change) { changes = append(changes, c) })
	r.Observe("ccstream", util("seven_day", 0.98, now.Add(12*time.Hour)), now)
	r.Observe("ccstream", Event{Window: "five_hour", Reached: true, ResetAt: now.Add(2 * time.Hour)}, now)

	from, st := r.Clear("ccstream", now)
	if from != Stop || st.Phase != Open || len(st.Windows) != 0 {
		t.Fatalf("Clear = (%v, %+v), want (stop, open with no windows)", from, st)
	}
	if n := len(changes); n == 0 || changes[n-1].To.Phase != Open || changes[n-1].From != Stop {
		t.Fatalf("subscribers saw %+v, want a last change stop -> open", changes)
	}
	if v := store.m["ccstream"]; v != "" {
		t.Errorf("stored state after Clear = %q, want empty", v)
	}
	restarted := New()
	restarted.SetStore(store)
	if st := restarted.State("ccstream", now); st.Phase != Open {
		t.Errorf("after a restart: phase %v, want open", st.Phase)
	}

	// Clearing an open account changes nothing and tells nobody.
	n := len(changes)
	if from, st := r.Clear("ccstream", now); from != Open || st.Phase != Open {
		t.Errorf("Clear of an open account = (%v, %v), want (open, open)", from, st.Phase)
	}
	if len(changes) != n {
		t.Errorf("Clear of an open account notified subscribers: %+v", changes[n:])
	}
}

// TestClear_CallsResetSubscribers (#3044): every Clear reaches the reset
// subscribers of that account, hold or not, after the lift; another
// account's are not called.
func TestClear_CallsResetSubscribers(t *testing.T) {
	now := time.Now()
	r := New()
	var calls int
	var phaseAtCall Phase
	r.SubscribeReset("ccstream", func() { calls++; phaseAtCall = r.State("ccstream", now).Phase })
	other := 0
	r.SubscribeReset("codex", func() { other++ })
	r.Observe("ccstream", Event{Window: "five_hour", Reached: true, ResetAt: now.Add(time.Hour)}, now)
	r.Clear("ccstream", now)
	if calls != 1 || phaseAtCall != Open {
		t.Fatalf("after a Clear of a stop: calls=%d phase seen=%v, want 1 call seeing open", calls, phaseAtCall)
	}
	r.Clear("ccstream", now)
	if calls != 2 {
		t.Errorf("a Clear of an open account: calls=%d, want 2", calls)
	}
	if other != 0 {
		t.Errorf("codex reset subscriber called %d times, want 0", other)
	}
	unsub := r.SubscribeReset("ccstream", func() { t.Error("ended subscription called") })
	unsub()
	r.Clear("ccstream", now)
}
