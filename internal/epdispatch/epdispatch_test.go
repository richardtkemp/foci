package epdispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitQueued blocks until d has n calls waiting in total.
func waitQueued(t *testing.T, d *Dispatcher, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := d.Status()
		if s.Waiting[High]+s.Waiting[Medium]+s.Waiting[Low] == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting calls never reached %d: %+v", n, d.Status())
}

// TestDispatcher_OneAtATimeInTierOrder: while one call is in flight, calls of
// every tier wait; on each release the next goes, high before medium before
// low, first come first served within a tier, and never two at once.
func TestDispatcher_OneAtATimeInTierOrder(t *testing.T) {
	d := &Dispatcher{name: "t", limit: 1}
	first, _, err := d.Acquire(context.Background(), Low, "")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var order []string
	inFlight, maxInFlight := 0, 0
	var wg sync.WaitGroup
	start := func(name string, tier Tier, queued int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, _, err := d.Acquire(context.Background(), tier, "")
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			mu.Lock()
			order = append(order, name)
			inFlight++
			maxInFlight = max(maxInFlight, inFlight)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			rel()
		}()
		waitQueued(t, d, queued)
	}
	// Arrival order deliberately not priority order.
	start("low1", Low, 1)
	start("med1", Medium, 2)
	start("high1", High, 3)
	start("low2", Low, 4)
	start("high2", High, 5)
	start("med2", Medium, 6)

	first()
	wg.Wait()

	want := "high1 high2 med1 med2 low1 low2"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	if maxInFlight != 1 {
		t.Errorf("max in flight = %d, want 1", maxInFlight)
	}
	s := d.Status()
	if s.InFlight != [numTiers]int{} {
		t.Errorf("still busy after every release: %+v", s)
	}
	if s.Calls[High] != 2 || s.Calls[Medium] != 2 || s.Calls[Low] != 3 {
		t.Errorf("calls = %v, want high 2, medium 2, low 3", s.Calls)
	}
	if s.Held[High] != 2 || s.Held[Low] != 2 {
		t.Errorf("held = %v, want high 2, low 2 (the first low call was sent at once)", s.Held)
	}
}

// TestDispatcher_NewerKeyReplacesWaiting: a waiting medium call is replaced
// by a newer one with the same key, and by Replace; other keys are kept.
func TestDispatcher_NewerKeyReplacesWaiting(t *testing.T) {
	d := &Dispatcher{name: "t"}
	hold, _, _ := d.Acquire(context.Background(), High, "")

	type res struct {
		name string
		err  error
	}
	results := make(chan res, 4)
	run := func(name, key string, queued int) {
		go func() {
			rel, _, err := d.Acquire(context.Background(), Medium, key)
			if rel != nil {
				rel()
			}
			results <- res{name, err}
		}()
		waitQueued(t, d, queued)
	}
	run("a-old", "auto chat name|c1", 1)
	run("b", "auto chat name|c2", 2)
	run("a-new", "auto chat name|c1", 2) // replaces a-old: still 2 waiting

	got := <-results
	if got.name != "a-old" || !errors.Is(got.err, ErrReplaced) {
		t.Fatalf("first result = %+v, want a-old replaced", got)
	}
	if !d.Replace("auto chat name|c2") {
		t.Fatal("Replace found no waiting c2 call")
	}
	got = <-results
	if got.name != "b" || !errors.Is(got.err, ErrReplaced) {
		t.Fatalf("second result = %+v, want b replaced", got)
	}
	if d.Replace("auto chat name|c2") {
		t.Error("Replace replaced a call that no longer waits")
	}
	hold()
	got = <-results
	if got.name != "a-new" || got.err != nil {
		t.Fatalf("third result = %+v, want a-new sent", got)
	}
	s := d.Status()
	if s.Replaced[Medium] != 2 || s.Calls[Medium] != 1 || s.Held[Medium] != 3 {
		t.Errorf("medium stats: replaced %d calls %d held %d, want 2, 1, 3", s.Replaced[Medium], s.Calls[Medium], s.Held[Medium])
	}
}

// TestDispatcher_CancelWhileWaiting: a call whose context ends while it waits
// leaves the queue and does not take the slot; the next one gets it.
func TestDispatcher_CancelWhileWaiting(t *testing.T) {
	d := &Dispatcher{name: "t", limit: 1}
	hold, _, _ := d.Acquire(context.Background(), High, "")

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := d.Acquire(ctx, High, "")
		errc <- err
	}()
	waitQueued(t, d, 1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}
	if s := d.Status(); s.Waiting[High] != 0 {
		t.Fatalf("cancelled call still queued: %+v", s)
	}

	next := make(chan struct{})
	go func() {
		rel, _, err := d.Acquire(context.Background(), Low, "")
		if err == nil {
			rel()
		}
		close(next)
	}()
	waitQueued(t, d, 1)
	hold()
	select {
	case <-next:
	case <-time.After(5 * time.Second):
		t.Fatal("the slot was not passed on after a cancelled wait")
	}
	if s := d.Status(); s.InFlight != [numTiers]int{} {
		t.Errorf("busy after all releases: %+v", s)
	}
}

// TestDispatcher_ReleaseTwiceIsOnce: a second release does not free the slot
// another call now holds.
func TestDispatcher_ReleaseTwiceIsOnce(t *testing.T) {
	d := &Dispatcher{name: "t"}
	rel1, _, _ := d.Acquire(context.Background(), High, "")
	rel1()
	rel2, _, _ := d.Acquire(context.Background(), High, "")
	rel1() // stale
	if s := d.Status(); s.InFlight[High] != 1 {
		t.Fatal("a repeated release freed the slot of the next call")
	}
	rel2()
}

func TestApplies(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		urls  []string
		want  bool
	}{
		{"local", 1, []string{"http://localhost:11434/v1"}, true},
		{"mac", 2, []string{"", "http://192.168.1.5:11434/v1"}, true},
		{"local", 0, []string{"http://localhost:11434/v1"}, false}, // the default: not dispatched
		{"anthropic", 0, nil, false},
		{"anthropic", 1, nil, true}, // opted in by hand
		{"openrouter", 1, []string{"https://openrouter.ai/api/v1"}, false},
		{"OpenRouter", 1, nil, false},
		{"myrouter", 1, []string{"", "https://openrouter.ai/api/v1"}, false}, // OpenRouter by URL
	}
	for _, c := range cases {
		if got := Applies(c.name, c.limit, c.urls...); got != c.want {
			t.Errorf("Applies(%q, %d, %q) = %v, want %v", c.name, c.limit, c.urls, got, c.want)
		}
	}
}

// TestEngage: an endpoint at max_in_flight 0 gets no dispatcher; turned on,
// Engage sets the cap; turned off, waiting calls are sent.
func TestEngage(t *testing.T) {
	name := fmt.Sprintf("engage-test-%d", time.Now().UnixNano())
	if d := Engage(name, 0, "http://localhost:11434/v1"); d != nil {
		t.Fatal("max_in_flight 0 engaged a dispatcher")
	}
	d := Engage(name, 1, "http://localhost:11434/v1")
	if d == nil || d.Status().Limit != 1 {
		t.Fatalf("max_in_flight 1: dispatcher %v", d)
	}
	rel, _, _ := d.Acquire(context.Background(), High, "")
	done := make(chan struct{})
	go func() {
		r, _, _ := d.Acquire(context.Background(), High, "")
		r()
		close(done)
	}()
	waitQueued(t, d, 1)
	SetLimits(map[string]int{name: 0}) // the edit that turns it off
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turning max_in_flight off left a call waiting")
	}
	rel()
}

func TestReport(t *testing.T) {
	name := fmt.Sprintf("report-test-%d", time.Now().UnixNano()) // For is process-wide; -count reruns
	off := fmt.Sprintf("report-off-%d", time.Now().UnixNano())
	r0, _, _ := For(off).Acquire(context.Background(), High, "") // limit 0: not dispatched
	r0()
	d := Engage(name, 2, "http://localhost:11434/v1")
	hi, _, _ := d.Acquire(context.Background(), High, "")
	done := make(chan struct{})
	go func() {
		rel, _, _ := d.Acquire(context.Background(), Medium, "")
		rel()
		close(done)
	}()
	waitQueued(t, d, 1)
	rep := Report()
	if !strings.Contains(rep, name+" (max_in_flight 2): in flight high 1, medium 0, low 0; waiting high 0, medium 1, low 0") {
		t.Errorf("report:\n%s", rep)
	}
	if strings.Contains(rep, off) {
		t.Errorf("an endpoint with max_in_flight 0 is reported:\n%s", rep)
	}
	hi()
	<-done
	if rep := Report(); !strings.Contains(rep, "medium: sent 1, held 1") {
		t.Errorf("report after the wait:\n%s", rep)
	}
}

// TestDispatcher_TierRulesUnderTheCap: while the cap is not reached, High is
// never held; Medium waits for High and runs one at a time; Low waits for
// High, Medium in flight and Medium waiting, but not for another Low.
func TestDispatcher_TierRulesUnderTheCap(t *testing.T) {
	d := &Dispatcher{name: "t", limit: 10}
	ctx := context.Background()
	acquireNow := func(tier Tier) func() {
		t.Helper()
		rel, wait, err := d.Acquire(ctx, tier, "")
		if err != nil || wait != 0 {
			t.Fatalf("%s call: wait %v err %v, want sent at once", tier, wait, err)
		}
		return rel
	}
	type got struct {
		name string
		rel  func()
	}
	sent := make(chan got, 8)
	park := func(name string, tier Tier, queued int) {
		go func() {
			rel, _, err := d.Acquire(ctx, tier, "")
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			sent <- got{name, rel}
		}()
		waitQueued(t, d, queued)
	}
	expect := func(name string) func() {
		t.Helper()
		select {
		case g := <-sent:
			if g.name != name {
				t.Fatalf("sent %s, want %s", g.name, name)
			}
			return g.rel
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not sent", name)
		}
		return nil
	}
	expectNone := func() {
		t.Helper()
		select {
		case g := <-sent:
			t.Fatalf("%s was sent, want nothing", g.name)
		case <-time.After(20 * time.Millisecond):
		}
	}

	l1 := acquireNow(Low)
	l2 := acquireNow(Low) // Low does not hold Low back
	h1 := acquireNow(High)
	h2 := acquireNow(High) // High is never held, with Low and High in flight
	park("m1", Medium, 1)  // High in flight
	park("l3", Low, 2)     // High in flight, Medium waiting
	h1()
	expectNone() // h2 still runs
	h2()
	m1 := expect("m1")
	expectNone() // l3: Medium in flight
	park("m2", Medium, 2)
	m1()
	m2 := expect("m2") // before l3, which also waits on a waiting Medium
	expectNone()
	h3 := acquireNow(High) // High is never held, with a Medium in flight
	m2()
	expectNone() // l3: High in flight
	h3()
	l3 := expect("l3")
	l1()
	l2()
	l3()
	if s := d.Status(); s.InFlight != [numTiers]int{} || s.Waiting != [numTiers]int{} {
		t.Errorf("not idle at the end: %+v", s)
	}
}

// TestDispatcher_MaxInFlight: with a limit of 2, two calls run at once and a
// third waits; lowering the limit to 1 sends nothing more until one call is
// left; raising it sends waiting calls at once.
func TestDispatcher_MaxInFlight(t *testing.T) {
	d := &Dispatcher{name: "t"}
	d.SetLimit(2)
	r1, _, _ := d.Acquire(context.Background(), Low, "")
	r2, w, _ := d.Acquire(context.Background(), Low, "")
	if w != 0 {
		t.Fatalf("second call waited with limit 2")
	}
	got := make(chan string, 3)
	go func() {
		rel, _, _ := d.Acquire(context.Background(), High, "")
		got <- "third"
		defer rel()
	}()
	waitQueued(t, d, 1)

	d.SetLimit(1)
	r1() // one still in flight: at the limit, the third must keep waiting
	select {
	case <-got:
		t.Fatal("a call was sent while the lowered limit was full")
	case <-time.After(20 * time.Millisecond):
	}
	d.SetLimit(3) // raised: the waiting call goes at once
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("raising the limit did not send the waiting call")
	}
	r2()
	if s := d.Status(); s.Limit != 3 {
		t.Errorf("limit = %d, want 3", s.Limit)
	}

	SetLimits(map[string]int{"limits-test": 4})
	if l := For("limits-test").Status().Limit; l != 4 {
		t.Errorf("SetLimits: limit = %d, want 4", l)
	}
	SetLimits(map[string]int{"limits-test": 0})
	if l := For("limits-test").Status().Limit; l != DefaultMaxInFlight {
		t.Errorf("SetLimits 0: limit = %d, want %d", l, DefaultMaxInFlight)
	}
}
