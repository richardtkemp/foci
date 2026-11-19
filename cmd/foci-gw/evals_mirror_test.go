package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/evals"
	"foci/internal/log"
	"foci/internal/telemetry"
)

// fakeRubrics is a rubricSource recording the config ids it is given.
type fakeRubrics struct {
	mu      sync.Mutex
	rubrics map[string]*evals.Rubric
	ids     map[string]string
}

func newFakeRubrics(names ...string) *fakeRubrics {
	f := &fakeRubrics{rubrics: map[string]*evals.Rubric{}, ids: map[string]string{}}
	for _, n := range names {
		f.rubrics[n] = &evals.Rubric{Name: n, Type: evals.TypeBoolean}
	}
	return f
}

func (f *fakeRubrics) Get(name string) (*evals.Rubric, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rb, ok := f.rubrics[name]
	return rb, ok
}

func (f *fakeRubrics) SetConfigID(name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[name] = id
}

func (f *fakeRubrics) id(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[name]
}

func (f *fakeRubrics) list() []*evals.Rubric {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*evals.Rubric
	for _, rb := range f.rubrics {
		out = append(out, rb)
	}
	return out
}

// ensureFailing returns an ensure func that fails its first n calls with
// "connection refused" (Langfuse not up yet), then returns "cfg-<name>".
func ensureFailing(n int64, calls *atomic.Int64) func(context.Context, telemetry.ScoreConfig) (string, string, error) {
	return func(_ context.Context, c telemetry.ScoreConfig) (string, string, error) {
		if calls.Add(1) <= n {
			return "", "", errors.New("dial tcp 127.0.0.1:3100: connect: connection refused")
		}
		return "cfg-" + c.Name, "", nil
	}
}

func waitArmed(t *testing.T, clk *clock.Fake) {
	t.Helper()
	if !clk.BlockUntilArmed(1, 10*time.Second) {
		t.Fatal("the mirror never armed its retry timer")
	}
}

// TestScoreConfigMirror_RetriesUntilMirrored is the boot case (#3189): the
// first two passes hit a Langfuse that is not up, the third succeeds, and the
// rubric gets its configId. Two minutes of failure is under warnAfter, so the
// failures stay INFO.
func TestScoreConfigMirror_RetriesUntilMirrored(t *testing.T) {
	const comp = "evals-mirror-test-retry"
	logs := captureLog(t, log.INFO)
	reg := newFakeRubrics("helpful")
	clk := clock.NewFake()
	var calls atomic.Int64
	m := newScoreConfigMirror(reg, ensureFailing(2, &calls), clk, log.NewComponentLogger(comp))
	defer m.Close()

	m.Enqueue(reg.list())
	waitArmed(t, clk) // pass 1 failed
	clk.Advance(scoreMirrorRetryEvery)
	waitArmed(t, clk) // pass 2 failed
	clk.Advance(scoreMirrorRetryEvery)

	deadline := time.Now().Add(10 * time.Second)
	for reg.id("helpful") == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := reg.id("helpful"); got != "cfg-helpful" {
		t.Fatalf("configId = %q after the third pass, want cfg-helpful (calls=%d)", got, calls.Load())
	}
	if w := logs.Lines("WARN", "["+comp+"]"); len(w) != 0 {
		t.Errorf("WARN lines = %v, want none (retrying worked)", w)
	}
	if n := len(logs.Lines("INFO", "["+comp+"]", "not mirrored yet")); n != 2 {
		t.Errorf("INFO failure lines = %d, want 2", n)
	}
	if clk.BlockUntilArmed(1, 100*time.Millisecond) {
		t.Error("a retry timer is still armed after everything mirrored")
	}
}

// TestScoreConfigMirror_WarnsOnceWhenStillFailing: a Langfuse that stays
// down gets exactly one WARN, once warnAfter has passed, however long the
// retries go on.
func TestScoreConfigMirror_WarnsOnceWhenStillFailing(t *testing.T) {
	const comp = "evals-mirror-test-warn"
	logs := captureLog(t, log.INFO)
	reg := newFakeRubrics("helpful")
	clk := clock.NewFake()
	var calls atomic.Int64
	m := newScoreConfigMirror(reg, ensureFailing(1<<40, &calls), clk, log.NewComponentLogger(comp))
	defer m.Close()

	m.Enqueue(reg.list())
	for i := 0; i < 10; i++ { // ten minutes of outage
		waitArmed(t, clk)
		clk.Advance(scoreMirrorRetryEvery)
	}
	waitArmed(t, clk)

	w := logs.Lines("WARN", "["+comp+"]")
	if len(w) != 1 {
		t.Fatalf("WARN lines = %v, want exactly one", w)
	}
	if len(logs.Lines("WARN", "["+comp+"]", "[helpful] still not mirrored after 5m0s")) != 1 {
		t.Errorf("WARN = %v, want it to name the rubric and the 5m0s", w)
	}
	if reg.id("helpful") != "" {
		t.Error("configId set although every attempt failed")
	}
}

// TestScoreConfigMirror_ReloadJoinsTheLoop: a rubric reload while a retry is
// waiting runs a pass at once and leaves one retry timer, not two loops.
func TestScoreConfigMirror_ReloadJoinsTheLoop(t *testing.T) {
	reg := newFakeRubrics("helpful", "terse")
	clk := clock.NewFake()
	var calls atomic.Int64
	m := newScoreConfigMirror(reg, ensureFailing(1<<40, &calls), clk, log.NewComponentLogger("evals-mirror-test-reload"))
	defer m.Close()

	m.Enqueue(reg.list())
	waitArmed(t, clk)
	before := calls.Load()

	r, _ := reg.Get("terse")
	m.Enqueue([]*evals.Rubric{r}) // the reload: must not wait a minute, must not fork
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < before+2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	waitArmed(t, clk)
	if clk.BlockUntilArmed(2, 200*time.Millisecond) {
		t.Fatal("two retry timers armed after a reload: a second loop started")
	}
	if got := calls.Load() - before; got != 2 {
		t.Errorf("the reload's pass made %d attempts, want 2 (both pending rubrics)", got)
	}

	before = calls.Load()
	clk.Advance(scoreMirrorRetryEvery)
	waitArmed(t, clk)
	if got := calls.Load() - before; got != 2 {
		t.Errorf("one retry tick made %d attempts, want 2 (one loop, two rubrics)", got)
	}
}
