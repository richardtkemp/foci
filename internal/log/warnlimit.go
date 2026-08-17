package log

import (
	"sync"
	"time"

	"foci/internal/timeutil"
)

// WarnLimiter debounces a repeating warning WITHOUT touching the work that
// produces it. It answers one question — "should this occurrence be logged?" —
// and nothing else: callers keep detecting, retrying and recovering at full
// rate, and only the announcing is rationed.
//
// That separation is the whole point, and it is easy to lose. A condition that
// warns per-occurrence produces volume proportional to how hard the system is
// retrying, so the natural-looking fix is to retry less — which trades away
// responsiveness to buy quiet. Rationing the message instead costs nothing:
// the 47th "still broken" line carries no information the 1st did not.
//
// The interval ESCALATES (base, then doubling, capped at max) rather than
// staying fixed, because the value of a repeat decays with the age of the
// condition: minute two of an outage is news, minute ninety is not. A fixed
// interval is the degenerate case — pass base == max.
//
// Reset marks the condition cleared, so the NEXT occurrence warns immediately
// at full volume. This is what keeps an INTERMITTENT fault loud: each new
// episode starts a fresh escalation rather than inheriting the silence earned
// by the previous one. Without it, a flapping condition would be progressively
// muted precisely as it got worse.
//
// Keys let one limiter serve several independent conditions (per file, per
// device). State is retained per key, so keys must come from a bounded set —
// an unbounded key space (per request, per socket) would leak.
//
// All methods are safe for concurrent use; the nil receiver allows everything
// (a nil limiter means "unthrottled", never "silent").
type WarnLimiter struct {
	base, max time.Duration
	now       func() time.Time

	mu    sync.Mutex
	state map[string]*warnLimitState
}

type warnLimitState struct {
	lastWarn   time.Time     // when this key last emitted
	interval   time.Duration // how long the current suppression window is
	suppressed int           // occurrences dropped since the last emit
}

// NewWarnLimiter returns a limiter that emits immediately, then suppresses for
// base, doubling the window on each emit up to max. base == max gives a fixed
// cooldown. A non-positive base disables suppression entirely.
func NewWarnLimiter(base, max time.Duration) *WarnLimiter {
	return NewWarnLimiterWithClock(base, max, timeutil.Now)
}

// NewWarnLimiterWithClock is NewWarnLimiter with an injectable time source, so
// tests can assert the escalation schedule deterministically instead of
// sleeping through it. Production callers should use NewWarnLimiter.
func NewWarnLimiterWithClock(base, max time.Duration, now func() time.Time) *WarnLimiter {
	if max < base {
		max = base
	}
	if now == nil {
		now = timeutil.Now
	}
	return &WarnLimiter{base: base, max: max, now: now, state: map[string]*warnLimitState{}}
}

// Allow reports whether this occurrence of key should be logged, and how many
// occurrences were suppressed since the last one that was. Include suppressed
// in the message when it is non-zero: a reader who cannot tell that lines were
// dropped cannot tell quiet from broken, and that ambiguity is the one real
// cost of throttling anything.
func (l *WarnLimiter) Allow(key string) (emit bool, suppressed int) {
	if l == nil || l.base <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	st := l.state[key]
	if st == nil {
		st = &warnLimitState{}
		l.state[key] = st
	}
	now := l.now()
	// A zero lastWarn is the first occurrence for this key (or the first since
	// a Reset) and always emits — new conditions are never born throttled.
	if !st.lastWarn.IsZero() && now.Sub(st.lastWarn) < st.interval {
		st.suppressed++
		return false, 0
	}
	dropped := st.suppressed
	st.suppressed = 0
	st.lastWarn = now
	switch {
	case st.interval == 0:
		st.interval = l.base
	case st.interval < l.max:
		if st.interval *= 2; st.interval > l.max {
			st.interval = l.max
		}
	}
	return true, dropped
}

// Reset clears all state for key: the condition has ended, so the next
// occurrence is a NEW episode and warns immediately at the base interval.
// Calling it when nothing was suppressed is harmless.
func (l *WarnLimiter) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.state, key)
	l.mu.Unlock()
}
