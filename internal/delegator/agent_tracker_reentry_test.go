package delegator

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// Tests for #3169: the tracker ran OnRunning/OnStatus with its mutex held, and
// the app's waiting-on-other refresh read Pending from inside OnRunning. The
// mutex is not re-entrant, so the ccstream reader goroutine deadlocked on
// itself and took the gateway's app hub down with it (work Mac, 18 Nov 2026).

// reentryTimeout bounds a call that deadlocks on the broken code. It is a hang
// guard only: the passing path takes microseconds.
const reentryTimeout = 10 * time.Second

// mustReturn runs fn on a new goroutine and fails the test if it does not
// return within reentryTimeout (a deadlocked goroutine is left behind; the
// test has already failed).
func mustReturn(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(reentryTimeout):
		t.Fatalf("%s did not return within %s: a callback re-entering the tracker deadlocked", what, reentryTimeout)
	}
}

// Every method that can deliver a status change must let both callbacks call
// back into the tracker: read it (Pending, Running, IDsOfKind) and change it.
func TestSubagentTracker_CallbacksMayReenter(t *testing.T) {
	t.Parallel()
	stale := func(tr *SubagentTracker) {
		tr.mu.Lock()
		tr.pending = append(tr.pending, TrackedSubagent{ID: "old", Kind: SubagentKindAgent, added: time.Now().Add(-3 * time.Hour)})
		tr.mu.Unlock()
	}
	cases := []struct {
		name  string
		setup func(tr *SubagentTracker)
		act   func(tr *SubagentTracker)
	}{
		{"AddEntry", nil, func(tr *SubagentTracker) { tr.Add("a", "first") }},
		{"Remove", func(tr *SubagentTracker) { tr.Add("a", "first") }, func(tr *SubagentTracker) { tr.Remove("a") }},
		{"RemoveOne", func(tr *SubagentTracker) { tr.Add("a", "first") }, func(tr *SubagentTracker) { tr.RemoveOne() }},
		{"ClearAll", func(tr *SubagentTracker) { tr.Add("a", "first") }, func(tr *SubagentTracker) { tr.ClearAll() }},
		{"SetModel", func(tr *SubagentTracker) { tr.Add("a", "first") }, func(tr *SubagentTracker) { tr.SetModel("a", "opus") }},
		{"NotifyIdle", nil, func(tr *SubagentTracker) { tr.NotifyIdle() }},
		{"Pending prune", stale, func(tr *SubagentTracker) { tr.Pending() }},
	}
	for _, tc := range cases {
		for _, which := range []string{"OnRunning", "OnStatus"} {
			t.Run(tc.name+"/"+which, func(t *testing.T) {
				t.Parallel()
				tr := &SubagentTracker{}
				if tc.setup != nil {
					tc.setup(tr)
				}
				called := make(chan struct{}, 1)
				reenter := func() {
					// What the app does (Pending via AwaitingAutonomousRun), plus
					// the other reads and a write.
					_ = tr.Pending()
					_ = tr.Running()
					_ = tr.IDsOfKind(SubagentKindAgent)
					tr.SetModel("nobody", "m")
					select {
					case called <- struct{}{}:
					default:
					}
				}
				if which == "OnRunning" {
					tr.OnRunning = func([]RunningSubagent) { reenter() }
				} else {
					tr.OnStatus = func(string) { reenter() }
				}
				mustReturn(t, tc.name, func() { tc.act(tr) })
				// A Pending prune delivers off the caller's goroutine, so wait on
				// the callback itself rather than assume it already ran.
				select {
				case <-called:
				case <-time.After(reentryTimeout):
					t.Fatalf("%s: %s never called", tc.name, which)
				}
			})
		}
	}
}

// A change a callback makes is delivered after the callback returns, not lost
// and not delivered recursively inside it.
func TestSubagentTracker_ChangeInsideCallbackIsDelivered(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{}
	var got []string
	depth := 0
	tr.OnStatus = func(d string) {
		depth++
		defer func() { depth-- }()
		if depth > 1 {
			t.Errorf("OnStatus re-entered recursively (depth %d)", depth)
		}
		got = append(got, d)
		if d == "first" {
			tr.Add("b", "second")
		}
	}
	mustReturn(t, "Add", func() { tr.Add("a", "first") })
	want := []string{"first", "first, second"}
	if !slices.Equal(got, want) {
		t.Fatalf("OnStatus calls = %q, want %q", got, want)
	}
}

// Under concurrent changes the callbacks never run at once, and the last
// state they are given is the tracker's final state.
func TestSubagentTracker_ConcurrentChangesDeliverFinalState(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{}
	var mu sync.Mutex
	inside := 0
	var last []RunningSubagent
	tr.OnRunning = func(r []RunningSubagent) {
		mu.Lock()
		inside++
		if inside > 1 {
			t.Errorf("OnRunning ran concurrently (%d at once)", inside)
		}
		last = r
		mu.Unlock()
		time.Sleep(10 * time.Microsecond) // widen the window for an overlap
		mu.Lock()
		inside--
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				id := fmt.Sprintf("g%d-%d", g, i)
				tr.Add(id, id)
				if i%2 == 0 {
					tr.Remove(id)
				}
			}
		}()
	}
	wg.Wait()
	want := tr.Running()
	mu.Lock()
	defer mu.Unlock()
	if len(last) != len(want) {
		t.Fatalf("last delivered list has %d entries, tracker holds %d", len(last), len(want))
	}
	for i := range want {
		if last[i].ID != want[i].ID {
			t.Fatalf("last delivered[%d] = %s, tracker holds %s", i, last[i].ID, want[i].ID)
		}
	}
}

// A callback that panics does not wedge delivery for later changes.
func TestSubagentTracker_PanickingCallbackDoesNotWedge(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{}
	boom := true
	var got []string
	tr.OnStatus = func(d string) {
		if boom {
			boom = false
			panic("boom")
		}
		got = append(got, d)
	}
	func() {
		defer func() { _ = recover() }()
		tr.Add("a", "first")
	}()
	tr.Add("b", "second")
	if want := []string{"first, second"}; !slices.Equal(got, want) {
		t.Fatalf("after a panicking callback, OnStatus calls = %q, want %q", got, want)
	}
}
