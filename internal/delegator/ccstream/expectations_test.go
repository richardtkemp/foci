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
// the t2 process restored) and after t2.

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

// seedBaseline does what Start does for a --resume (#2012): seeds
// lastModelUsage from a transcript's last cost-state record, via the fix's own
// reader.
func seedBaseline(t *testing.T, b *Backend, name string) {
	t.Helper()
	base, err := resumeBaseline("testdata/"+name, probeSession)
	if err != nil || len(base) == 0 {
		t.Fatalf("resumeBaseline(%s) = %v, %v — the seed would prove nothing", name, base, err)
	}
	b.mu.Lock()
	b.lastModelUsage = base
	b.mu.Unlock()
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

// TestFreshProcessUsage_ResumeWithBaselineIsQuiet is fixed main: CC 2.1.280
// restores the t1 cost-state record into the t2 process, Start seeds that same
// record, and the delta is exactly t2's own work (45,769 - 22,766 = 23,003).
func TestFreshProcessUsage_ResumeWithBaselineIsQuiet(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	seedBaseline(t, b, "resume_usage_2.1.280_t1_coststate.jsonl")
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl")
	for _, inv := range []string{invFreshProcessUsage, invModelUsageMonotonic, invCacheWriteSplit} {
		if got := g.Count(expectBackend, inv); got != 0 {
			t.Errorf("%q fired %d time(s) on a correctly baselined resume", inv, got)
		}
	}
}

// TestFreshProcessUsage_ResumeWithoutBaselineFires is #2012 unfixed: CC
// restored t1's totals but foci seeded nothing, so the first turn carries the
// whole conversation. Also what a missing or unreadable baseline looks like.
func TestFreshProcessUsage_ResumeWithoutBaselineFires(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl")

	if got := g.Count(expectBackend, invFreshProcessUsage); got != 1 {
		t.Fatalf("fresh-process violations = %d, want 1: t2's ModelUsage holds 45,769 cache tokens against "+
			"23,003 on its own stream, and that gap is t1 restored by --resume (#2012)", got)
	}
	for _, inv := range []string{invModelUsageMonotonic, invCacheWriteSplit} {
		if got := g.Count(expectBackend, inv); got != 0 {
			t.Errorf("%q fired %d time(s) on the same traffic — only the fresh-process check should", inv, got)
		}
	}
}

// TestFreshProcessUsage_BaselineAheadOfRestoreFires: foci seeded a LATER record
// (t2's) than the one CC restored (t1's). The delta is then 0 while 23,003 was
// seen, and modelUsageDelta's per-field clamp would hide that.
func TestFreshProcessUsage_BaselineAheadOfRestoreFires(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	seedBaseline(t, b, "resume_usage_2.1.280_t2_coststate.jsonl")
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl")
	if got := g.Count(expectBackend, invFreshProcessUsage); got != 1 {
		t.Errorf("fresh-process violations = %d, want 1 for a baseline ahead of what CC restored", got)
	}
}

// TestFreshProcessUsage_CCStopsRestoringFires: foci seeded t1's record as the
// fix expects, but CC (as before 2.1.280) started the process at zero, so its
// first ModelUsage is t2's own work alone and the delta goes NEGATIVE.
func TestFreshProcessUsage_CCStopsRestoringFires(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	seedBaseline(t, b, "resume_usage_2.1.280_t1_coststate.jsonl")
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl",
		`"cacheReadInputTokens":36457`, `"cacheReadInputTokens":22766`,
		`"cacheCreationInputTokens":9312`, `"cacheCreationInputTokens":237`)
	if got := g.Count(expectBackend, invFreshProcessUsage); got != 1 {
		t.Errorf("fresh-process violations = %d, want 1 when CC stops restoring", got)
	}
	// The drop below the seeded baseline is not a within-process regression.
	if got := g.Count(expectBackend, invModelUsageMonotonic); got != 0 {
		t.Errorf("monotonic check fired %d time(s) against a seeded baseline", got)
	}
}

// TestFreshProcessUsage_FreshConversationIsQuiet is the normal case: a process
// that is NOT resuming reports exactly what its stream showed.
func TestFreshProcessUsage_FreshConversationIsQuiet(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	replayProbe(t, b, "resume_usage_2.1.280_t1.jsonl")

	for _, inv := range []string{invFreshProcessUsage, invModelUsageMonotonic, invCacheWriteSplit} {
		if got := g.Count(expectBackend, inv); got != 0 {
			t.Errorf("%q fired %d time(s) on a fresh conversation's first turn", inv, got)
		}
	}
	// Premise: the replay really produced a priced result, or "quiet" is vacuous.
	b.mu.Lock()
	n := len(b.lastModelUsage)
	b.mu.Unlock()
	if n == 0 {
		t.Fatal("no ModelUsage snapshot taken — the result line never reached OnResult")
	}
}

// TestFreshProcessUsage_OnlyTheFirstResultIsJudged: a later result in the same
// process is cumulative BY DESIGN (#1674) and must not be read as history.
func TestFreshProcessUsage_OnlyTheFirstResultIsJudged(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	replayProbe(t, b, "resume_usage_2.1.280_t1.jsonl")
	// A second turn in the SAME process: ModelUsage now carries t1 + this
	// turn, while the stream shows only this turn's message.
	b.noteAssistantUsage(msgModel("claude-haiku-4-5", "msg_second", "", 237, 0, 237))
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{
		"claude-haiku-4-5": {InputTokens: 20, OutputTokens: 228, CacheReadInputTokens: 36457, CacheCreationInputTokens: 9312},
	}})
	if got := g.Count(expectBackend, invFreshProcessUsage); got != 0 {
		t.Errorf("fresh-process check fired on a second result in the same process (%d)", got)
	}
}

// TestFreshProcessUsage_StandsDownAfterCompaction: a compaction's own API call
// is billed into ModelUsage but was never verified to reach the stream.
func TestFreshProcessUsage_StandsDownAfterCompaction(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	b.OnSystem("compact_boundary", []byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":1}}`))
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl")
	if got := g.Count(expectBackend, invFreshProcessUsage); got != 0 {
		t.Errorf("fired after a compaction (%d); it cannot tell compaction spend from restored history", got)
	}
}

// TestModelUsageMonotonic: within one process every counter only grows. The
// four snapshots are cost.go's 2026-08-05 probe; the fifth goes DOWN.
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
}

// TestCacheWriteSplit: a message with cache writes must say at which TTL.
func TestCacheWriteSplit(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_ok", "", 1000, 0, 1000))
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_nowrites", "", 0, 0, 0))
	if got := g.Count(expectBackend, invCacheWriteSplit); got != 0 {
		t.Fatalf("fired %d time(s) on messages that carry the split or write nothing", got)
	}
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_bad", "", 1000, 0, 0))
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
	replayProbe(t, b, "resume_usage_2.1.280_t2.jsonl") // unseeded: #2012's shape

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
	for _, want := range []string{"claude-code 2.1.280", invFreshProcessUsage, "d73108ac-6abc-4ab5-addf-1d98c7d6c1de"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q: %s", want, report)
		}
	}
}
