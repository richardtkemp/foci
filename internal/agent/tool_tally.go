package agent

import (
	"sync"

	"foci/internal/delegator"
)

// toolTally counts one session's tool calls for the current delegated turn,
// from the per-call SessionEvents every backend already emits (OnToolStart /
// OnToolEnd). It is the single tool counter for delegated turns: backends keep
// none of their own (#2193). Fed by the SessionEvents AttachDelivery builds,
// reset by buildTurnEvents at the start of each turn, read by the nudge hooks.
//
// Calls are keyed by tool id, so a backend that re-emits a start or end for
// the same call (opencode re-sends running parts; ccstream can end a denied
// call from two paths) still counts it once. A call seen only as a start, or
// only as an end, still counts: no backend has to deliver both.
//
// Safe for concurrent use: the backend's stream reader feeds it while its hook
// listener (ccstream's PostToolNudgeFunc) reads it.
type toolTally struct {
	mu      sync.Mutex
	started map[string]struct{}
	ended   map[string]struct{}
	// Calls with no id can't be deduplicated; count them by event.
	anonStarted, anonEnded int
}

func newToolTally() *toolTally {
	return &toolTally{started: map[string]struct{}{}, ended: map[string]struct{}{}}
}

func (t *toolTally) start(id, name string) {
	if name == delegator.CompactionToolName {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		t.anonStarted++
		return
	}
	t.started[id] = struct{}{}
}

func (t *toolTally) end(id, name string) {
	if name == delegator.CompactionToolName {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		t.anonEnded++
		return
	}
	t.ended[id] = struct{}{}
}

// reset empties the tally for a new turn.
func (t *toolTally) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.started)
	clear(t.ended)
	t.anonStarted, t.anonEnded = 0, 0
}

// calls is the number of distinct tool calls this turn, started or finished.
// The pre-answer gate's min_tools threshold reads it.
func (t *toolTally) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.started)
	for id := range t.ended {
		if _, ok := t.started[id]; !ok {
			n++
		}
	}
	return n + max(t.anonStarted, t.anonEnded)
}

// completed is the number of distinct tool calls that have finished this turn.
// Post-tool nudges read it: they fire per completion, so counting starts would
// run ahead of them under parallel tool calls and skip every_n_tools multiples.
func (t *toolTally) completed() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ended) + t.anonEnded
}

// toolTallyLocked returns sk's tally, creating it. Caller holds routersMu.
func (a *Agent) toolTallyLocked(sk string) *toolTally {
	if t := a.toolTallies[sk]; t != nil {
		return t
	}
	if a.toolTallies == nil {
		a.toolTallies = make(map[string]*toolTally)
	}
	t := newToolTally()
	a.toolTallies[sk] = t
	return t
}

// resetToolTally empties sk's tally for a new turn and returns it.
func (a *Agent) resetToolTally(sk string) *toolTally {
	a.routersMu.Lock()
	t := a.toolTallyLocked(sk)
	a.routersMu.Unlock()
	t.reset()
	return t
}
