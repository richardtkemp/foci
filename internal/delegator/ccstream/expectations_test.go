package ccstream

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// #2013: live checks on the CC behaviours foci's accounting rests on.
//
// The fixtures under testdata/ are the real stdout lines of the #2012 probe
// (CC 2.1.280, 2026-09-24, haiku): t1 is a fresh conversation's first turn, t2
// the next turn in a NEW process started with --resume. Only the init,
// assistant and result lines are kept. The *_coststate files are the two
// cost-state records CC wrote to that session's transcript: after t1 (what
// the t2 process restored, resume_baseline_test.go) and after t2.

// guardedBackend is a production-constructed Backend reporting to its own
// guard, so the shared rate limit cannot couple parallel tests.
func guardedBackend(t *testing.T) (*Backend, *delegator.ExpectationGuard) {
	t.Helper()
	be, err := newFromConfig(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	b := be.(*Backend)
	g := &delegator.ExpectationGuard{}
	b.expect = g
	return b, g
}

const probeSession = "d73108ac-6abc-4ab5-addf-1d98c7d6c1de"

// replayProbe feeds a captured CC stream through a Backend's real dispatch, as
// the reader would. It does not end the stream, so the Backend stays "running".
// Each (old, new) pair in edits is applied to every line, and must match at
// least once.
func replayProbe(t *testing.T, b *Backend, name string, edits ...string) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd := NewReader(nil, b)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	hits := make([]int, len(edits)/2)
	for sc.Scan() {
		line := sc.Text()
		for i := 0; i+1 < len(edits); i += 2 {
			if strings.Contains(line, edits[i]) {
				hits[i/2]++
				line = strings.ReplaceAll(line, edits[i], edits[i+1])
			}
		}
		rd.dispatch([]byte(line))
		n++
	}
	for i, h := range hits {
		if h == 0 {
			t.Fatalf("fixture edit %q matched nothing in %s", edits[2*i], name)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("fixture %s is empty — the test would prove nothing", name)
	}
}

// TestModelUsageMonotonic: within one process every counter only grows. The
// four snapshots are cost.go's 2026-08-05 probe; the fifth goes DOWN. A new
// process (Start) has nothing to compare its first result with.
func TestModelUsageMonotonic(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	send := func(out, cr int, cost float64) {
		b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{
			"claude-opus-5": {OutputTokens: out, CacheReadInputTokens: cr, CostUSD: cost},
		}})
	}
	send(56, 21624, 0.0099234)
	send(105, 46722, 0.0141382)
	send(141, 72545, 0.0170425)
	send(174, 98434, 0.0199124)
	if got := g.Count(expectBackend, invModelUsageMonotonic); got != 0 {
		t.Fatalf("fired %d time(s) on the probe's genuinely cumulative snapshots", got)
	}
	// CC reporting per-turn figures instead of a running sum.
	send(33, 25000, 0.003)
	if got := g.Count(expectBackend, invModelUsageMonotonic); got != 1 {
		t.Errorf("violations = %d, want 1 after output, cache_read and cost all went down in one process", got)
	}
	// A relaunch: the new process's counters start lower, which is not a drop.
	b.mu.Lock()
	b.prevModelUsage = nil
	b.mu.Unlock()
	send(10, 1000, 0.001)
	if got := g.Count(expectBackend, invModelUsageMonotonic); got != 1 {
		t.Errorf("violations = %d, want still 1: a new process's first result has nothing to compare with", got)
	}
}

// TestCacheWriteSplit: a message with cache writes must say at which TTL.
func TestCacheWriteSplit(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	b.OnAssistant(msgModel("claude-opus-5", "msg_ok", 1000, 0, 1000))
	b.OnAssistant(msgModel("claude-opus-5", "msg_nowrites", 0, 0, 0))
	if got := g.Count(expectBackend, invCacheWriteSplit); got != 0 {
		t.Fatalf("fired %d time(s) on messages that carry the split or write nothing", got)
	}
	b.OnAssistant(msgModel("claude-opus-5", "msg_bad", 1000, 0, 0))
	if got := g.Count(expectBackend, invCacheWriteSplit); got != 1 {
		t.Errorf("violations = %d, want 1 for 1000 cache writes with no cache_creation breakdown", got)
	}
}

// TestExpectationViolation_ReachesTheOperatorWithVersion checks delivery end to
// end: the report must travel the warn-hook path at ERROR (the level an
// "errors"-only notify queue still forwards) and name the CC version from init.
//
// Not parallel: log.SetWarnHook is process-global.
func TestExpectationViolation_ReachesTheOperatorWithVersion(t *testing.T) {
	var mu sync.Mutex
	var got []string
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, level.String()+" "+msg)
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b, _ := guardedBackend(t)
	// A message reporting cache writes with no TTL breakdown.
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl", `"cache_creation":{`, `"cache_creation_dropped":{`)

	mu.Lock()
	defer mu.Unlock()
	var report string
	for _, e := range got {
		if strings.Contains(e, "BACKEND EXPECTATION VIOLATED") {
			report = e
		}
	}
	if report == "" {
		t.Fatalf("no violation reached the warn hook; got %q", got)
	}
	if !strings.HasPrefix(report, "ERROR ") {
		t.Errorf("reported at %q, want ERROR", strings.SplitN(report, " ", 2)[0])
	}
	for _, want := range []string{"claude-code 2.1.280", invCacheWriteSplit, "no cache_creation breakdown"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q: %s", want, report)
		}
	}
}

// msgModel builds one completed assistant stream line with cache writes,
// split 5m/1h when either is non-zero.
func msgModel(model, id string, cacheWrite, e5m, e1h int) *AssistantMessage {
	m := &AssistantMessage{}
	m.Message.ID = id
	m.Message.Model = model
	m.Message.Usage = TokenUsage{CacheCreationInputTokens: cacheWrite}
	if e5m > 0 || e1h > 0 {
		m.Message.Usage.CacheCreation = &CacheCreationSplit{Ephemeral5m: e5m, Ephemeral1h: e1h}
	}
	stop := "end_turn"
	m.Message.StopReason = &stop
	return m
}
