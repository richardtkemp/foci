package delegator

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/clock"
)

// Backend-expectation guards (#2013).
//
// foci's accounting rests on facts about how Claude Code and codex behave —
// "ModelUsage is cumulative per process", "tokenUsage.total is a running sum of
// every last" — that were probe-verified once and then written into code
// comments as if they were permanent. Both backends auto-update. When one of
// those facts stops being true, nothing in the data looks wrong: the numbers
// are the right shape, only their meaning changed. #2012 is the instance that
// motivated this: CC 2.1.280 started restoring cumulative ModelUsage on
// --resume, costs were overstated by up to 128x for ~36h, and the divergence
// check stayed silent because both of its sides were inflated the same way.
//
// So each such fact that is cheap to check against data foci already holds
// gets a live check, and a failed check is reported here. One implementation
// for every backend, for the same reason CostDivergenceChecker is shared: a
// report should mean exactly one thing wherever it fires.
//
// DELIVERY. A violation is logged at ERROR. That is not decoration: ERROR is
// what log.SetWarnHook forwards to BOTH notify.inject_chat_warnings (the
// operator's chat) and notify.inject_agent_warnings, including when either is
// set to "errors" — a WARN would be dropped by an "errors" queue. A violated
// expectation means foci is writing wrong numbers right now, which is what
// ERROR is for.
//
// RATE LIMIT. A changed behaviour is violated on EVERY turn, and every CC
// process is a new Backend, so the limit lives here — process-wide — keyed by
// backend and invariant. The first violation reports immediately; repeats are
// counted and folded into the next report at most once per
// ExpectationRepeatInterval. A violation under a backend version the last
// report did not name reports immediately, because a new version is new
// information.

// ExpectationRepeatInterval is the minimum gap between two reports of the same
// standing violation (same backend, same invariant, same version).
const ExpectationRepeatInterval = time.Hour

// ExpectationLogger is the logging surface the guard needs.
// *log.ComponentLogger satisfies it.
type ExpectationLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// ExpectationGuard rate-limits backend-expectation violation reports and
// tracks backend versions. The zero value is ready to use; production code
// shares Expectations.
type ExpectationGuard struct {
	// Clock defaults to the wall clock when nil. Tests inject a clock.Fake.
	Clock clock.Clock

	mu       sync.Mutex
	state    map[string]*expectationState
	versions map[string]map[string]bool // backend -> every version seen
	current  map[string]string          // backend -> most recently seen version
}

type expectationState struct {
	lastReport time.Time
	version    string
	suppressed int
	total      int
}

// Expectations is the process-wide guard every backend reports to. Shared
// deliberately: the rate limit has to span Backend instances, since CC gets a
// fresh one per process and a standing violation fires on each.
var Expectations = &ExpectationGuard{}

func (g *ExpectationGuard) now() time.Time {
	if g.Clock != nil {
		return g.Clock.Now()
	}
	return time.Now()
}

// Violated reports that backend's behaviour contradicted an expectation foci
// relies on. invariant is a short stable name (it is the rate-limit key);
// detail carries the measured figures. version is the backend's own version
// string ("" when not yet known). Returns true when this call produced a
// report, false when it was rate-limited.
//
// lg must not be a logger whose sink could contend on a lock the caller holds.
func (g *ExpectationGuard) Violated(lg ExpectationLogger, backend, version, invariant, detail string) bool {
	key := backend + "\x00" + invariant
	now := g.now()

	g.mu.Lock()
	if g.state == nil {
		g.state = make(map[string]*expectationState)
	}
	st := g.state[key]
	if st == nil {
		st = &expectationState{}
		g.state[key] = st
	}
	st.total++
	due := st.lastReport.IsZero() ||
		version != st.version ||
		now.Sub(st.lastReport) >= ExpectationRepeatInterval
	if !due {
		st.suppressed++
		g.mu.Unlock()
		return false
	}
	suppressed := st.suppressed
	st.suppressed = 0
	st.lastReport = now
	st.version = version
	g.mu.Unlock()

	if lg == nil {
		return true
	}
	v := version
	if v == "" {
		v = "version unknown"
	}
	repeat := ""
	if suppressed > 0 {
		repeat = fmt.Sprintf(" [%d more violation(s) since the last report of this]", suppressed)
	}
	lg.Errorf("BACKEND EXPECTATION VIOLATED: %s %s broke %q — %s. Data foci derives from this is likely wrong until fixed; "+
		"check whether a backend update changed the behaviour (#2013)%s",
		backend, v, invariant, detail, repeat)
	return true
}

// Count is how many times invariant has been violated on backend since this
// guard was created, reported or not.
func (g *ExpectationGuard) Count(backend, invariant string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.state[backend+"\x00"+invariant]; st != nil {
		return st.total
	}
	return 0
}

// NoteVersion records the version a backend process reported at startup, and
// logs when it differs from what this foci process has seen before — so a
// violation can be tied to the update that caused it. The first version seen is
// an INFO line. A version never seen before is a WARN, which reaches the
// operator through the same warn-hook path as other warnings: backends
// auto-update underneath foci, and this is often the only notice of it.
//
// A version already seen is silent. Old and new binaries can briefly coexist
// across a rolling restart, and alternating between two known versions is not
// news.
func (g *ExpectationGuard) NoteVersion(lg ExpectationLogger, backend, version string) {
	if version == "" {
		return
	}
	g.mu.Lock()
	if g.versions == nil {
		g.versions = make(map[string]map[string]bool)
		g.current = make(map[string]string)
	}
	seen := g.versions[backend]
	if seen == nil {
		seen = make(map[string]bool)
		g.versions[backend] = seen
	}
	prev := g.current[backend]
	known := seen[version]
	seen[version] = true
	g.current[backend] = version
	g.mu.Unlock()

	if lg == nil || known {
		return
	}
	if prev == "" {
		lg.Infof("backend version: %s %s (first seen by this foci process)", backend, version)
		return
	}
	lg.Warnf("backend version CHANGED: %s %s -> %s. If foci's accounting or routing now misbehaves, "+
		"suspect this update first (#2013)", backend, prev, version)
}
