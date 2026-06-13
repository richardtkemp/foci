package testharness

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"

	"golang.org/x/sync/semaphore"
)

// Integration tests are throttled by COST, not by a binary class. Each test
// declares a weight against one shared budget; a test runs only once its
// weight can be admitted. This bounds total concurrent load with a single
// governor (set -parallel = budget so the flag never binds first).
//
// Why weights and not just a high -parallel: tests fall into two natural
// costs. Wait-bound tests spend almost all their wall-clock asleep on timers
// (cron ticks, fixed observation windows), burn negligible CPU, and make no
// latency-sensitive assertion — contention only slows them, it can't fail
// them, so they're cheap (weight 1) and many run at once. Heavy tests drive
// real agent turns / multi-step round-trips with internal deadlines
// (ReadyTimeout, message-wait loops) that CPU starvation can trip, turning a
// slowdown into a failure — so they're expensive (weight heavyLoad) and only
// ~NumCPU run together. ParallelWeight covers anything genuinely in between.
//
// Buckets would be this with two hardcoded weights; the weighted budget is
// the same idea generalized, with one knob instead of (-parallel + a
// separate heavy cap) and room for a middle weight when the differential
// audit finds one.

const (
	// loadPerCPU is the budget granted per core in units of light tests.
	// budget = loadPerCPU * NumCPU, so up to loadPerCPU wait-bound tests run
	// per core; it scales with the host. Tuned to 4 on a 4-core box: even
	// "light" tests have brief CPU bursts during session bootstrap, so past
	// ~4/core the overlapping bursts both regress wall-clock (measured: full
	// suite 313s at budget 16 vs 372s at 24) and start tripping the
	// deadline-sensitive tests' margins.
	loadPerCPU = 4
	// heavyLoad is a heavy test's weight: it costs as much as heavyLoad
	// light tests. Chosen so maxHeavy = budget/heavyLoad = NumCPU.
	heavyLoad = loadPerCPU
)

// budget is the total admissible weight at any instant.
var budget = loadPerCPU * max(1, runtime.NumCPU())

var sem = semaphore.NewWeighted(int64(budget))

func init() {
	// Fail fast: a weight exceeding the budget can never be admitted and
	// would block its test forever — a footgun a fixed-cap channel doesn't
	// have. The lightest host (1 core) still has budget = loadPerCPU, so
	// heavyLoad <= loadPerCPU is the invariant to hold.
	if heavyLoad > budget {
		panic(fmt.Sprintf("testharness: heavyLoad %d exceeds budget %d", heavyLoad, budget))
	}
}

// bucketAuditEnv silences the in-run audit when set to "off". The audit is
// the cheap, correct-direction half of weight detection (see auditWeight);
// the thorough detector is the differential `make bucket-audit` target,
// which is the only thing that sees contention sensitivity directly.
const bucketAuditEnv = "FOCI_BUCKET_AUDIT"

var (
	weightsMu sync.Mutex
	weights   = map[string]int{}
)

func setWeight(name string, w int) {
	weightsMu.Lock()
	weights[name] = w
	weightsMu.Unlock()
}

func weightOf(name string) (int, bool) {
	weightsMu.Lock()
	defer weightsMu.Unlock()
	w, ok := weights[name]
	return w, ok
}

// acquire is the shared body: record the declared weight, hand control back
// to the runtime via t.Parallel (so the acquire happens in the parallel
// phase, not while the test is parked), draw the weight from the budget, and
// return it at test end.
func acquire(t *testing.T, w int) {
	t.Helper()
	setWeight(t.Name(), w)
	t.Parallel()
	if err := sem.Acquire(context.Background(), int64(w)); err != nil {
		t.Fatalf("acquire parallel budget (weight %d): %v", w, err)
	}
	t.Cleanup(func() { sem.Release(int64(w)) })
}

// ParallelWait marks a wait-bound integration test (weight 1). Use it when
// the test's wall-clock is dominated by fixed sleeps / timer waits and it
// makes no assertion that could fail purely from running slower under load.
// This is the default for L2 tests.
func ParallelWait(t *testing.T) {
	t.Helper()
	acquire(t, 1)
}

// ParallelHeavy marks a CPU/deadline-sensitive integration test (weight
// heavyLoad), so at most ~NumCPU run together. Use it when the test drives
// real agent turns / round-trips, or asserts on anything with an internal
// timeout that CPU starvation could trip.
func ParallelHeavy(t *testing.T) {
	t.Helper()
	acquire(t, heavyLoad)
}

// ParallelWeight is the escape hatch for a test that is heavier than wait but
// lighter than full heavy (e.g. one the differential audit shows inflates
// under load without failing). Weight must be in [1, heavyLoad].
func ParallelWeight(t *testing.T, w int) {
	t.Helper()
	if w < 1 || w > heavyLoad {
		t.Fatalf("ParallelWeight: weight %d out of range [1, %d]", w, heavyLoad)
	}
	acquire(t, w)
}

// auditWeight flags the one mis-weighting an in-run check can catch cheaply
// and correctly: a lightest-weight (ParallelWait) test that FAILED under the
// high-parallelism shipping config. Wait-bound tests assert on events after
// generous fixed sleeps, so they shouldn't fail from running slower — if one
// does, the likeliest cause is a contention-sensitive deadline, i.e. it wants
// a heavier weight. (The inverse error — an over-heavy weight on a truly
// cheap test — only wastes budget and can't be seen from a single run; the
// differential `make bucket-audit` target catches it.)
//
// Advisory by default; FOCI_BUCKET_AUDIT=off silences it. We deliberately do
// NOT key off CPU: measurement showed heavy and wait tests consume
// indistinguishable gateway CPU (both are light), so CPU misclassifies.
func auditWeight(t *testing.T) {
	if os.Getenv(bucketAuditEnv) == "off" {
		return
	}
	w, ok := weightOf(t.Name())
	if !ok || !t.Failed() {
		return
	}
	if w == 1 {
		t.Logf("weight audit: %s is ParallelWait (weight 1) but failed under high "+
			"parallelism — if this is a timeout/contention flake rather than a logic "+
			"bug, give it a heavier weight (ParallelHeavy or ParallelWeight)", t.Name())
	}
}
