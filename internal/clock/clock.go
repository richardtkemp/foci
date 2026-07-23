// Package clock is the single injectable time seam for foci production code
// that arms a timer and needs its tests to drive that timer deterministically
// instead of racing the wall clock.
//
// The anti-pattern this replaces: a test sleeps for what it hopes is "well
// under" some component's timeout, then asserts the timeout hasn't (or has)
// fired. Under `go test -p=$(nproc) -parallel=16` at `nice -n 19` (foci's
// `make test`), scheduling delays routinely blow through a 4x margin, and the
// test fails for a reason that has nothing to do with the code under test
// (#1503, #1513).
//
// The fix is to make the component's time source injectable — production
// code takes a Clock (defaulting to Real()), tests pass a *Fake — and to
// drive the fake's virtual time explicitly with Advance instead of sleeping.
// Advance runs any callback whose deadline it crosses synchronously, on the
// calling goroutine, before returning, so assertions immediately afterwards
// see the effect with zero wall-clock wait and zero flake margin.
//
// This is the ONE shared driver for that shape of problem — components
// should take a clock.Clock rather than growing their own nowFunc/timer
// mocking. (internal/warnings.Queue predates this package and only fakes
// Now(); it can be migrated to Clock when it's next touched, but is left
// alone for now since its tests don't arm real timers — see notes-1513.md.)
package clock

import "time"

// Clock abstracts the two time operations production code needs: reading the
// current time, and arming a one-shot callback after a delay (the
// time.AfterFunc shape used by watchdogs, debouncers, and rate-limit
// buckets throughout foci). Inject Real() in production, a *Fake in tests.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// AfterFunc schedules f to run in its own goroutine after d elapses and
	// returns a Timer to Stop or Reset it, mirroring time.AfterFunc.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of *time.Timer that AfterFunc callers need. *time.Timer
// satisfies this directly (Stop/Reset already have this exact signature), so
// Real's AfterFunc needs no wrapping.
type Timer interface {
	// Stop cancels the timer, returning true if it fired the cancellation
	// (i.e. the callback had not already run or been stopped).
	Stop() bool
	// Reset reschedules the timer to fire in d from now, returning true if
	// the timer was active before the call (matching time.Timer.Reset).
	Reset(d time.Duration) bool
}

// real is the production Clock: a thin pass-through to the time package.
type real struct{}

// Real returns the production Clock backed by the actual wall clock. Safe to
// share a single instance; it is stateless.
func Real() Clock { return real{} }

func (real) Now() time.Time { return time.Now() }

func (real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
