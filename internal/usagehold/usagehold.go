// Package usagehold is the account-wide usage hold (#2491).
//
// A delegated backend reports the usage of its account's windows (Claude
// Code: five_hour and seven_day; codex: its primary and secondary windows).
// When a window reaches the hold threshold (default 98%, the global
// [usage_hold] threshold), every foci agent on that account HOLDS: only
// keepalives and foci_remind fires run, and everything else waits. When the
// backend reports the limit as reached (a rejected window), the account
// STOPS: keepalives and reminders wait too.
//
// An account is one backend family in the process (Dick 2026-11-02: "the
// same backend means the same account"): all ccstream agents are one
// account, all codex agents another.
//
// Each window clears on its own: at its reset time, or when a usage event for
// THAT window reports less than the threshold and not reached. The account
// lifts when no window holds or stops it. Clear lifts it at once (#3043: the
// /limit-reset command, or a /usage read that finds usage available). The state survives a restart
// through a Store.
//
// The package is a leaf: it knows nothing of agents. Agents subscribe to an
// account and react to its Changes (notices, interrupting turns, compaction,
// releasing held input).
package usagehold

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"
)

// Phase is an account's hold state.
type Phase int

const (
	// Open: nothing is held.
	Open Phase = iota
	// Hold: a window is at or above the threshold. Only keepalives and
	// foci_remind fires run.
	Hold
	// Stop: a window's limit is reached. Nothing runs.
	Stop
)

func (p Phase) String() string {
	switch p {
	case Hold:
		return "hold"
	case Stop:
		return "stop"
	default:
		return "open"
	}
}

// DefaultThresholdPercent is the hold threshold when none is configured.
const DefaultThresholdPercent = 98

// rejectedFallback is how long a reached window with no reset time stays
// closed, matching the shared usage fallback (internal/ratelimit).
const rejectedFallback = time.Hour

// Event is one usage report for one window of an account.
type Event struct {
	// Window names the window ("seven_day", "five_hour", "codex primary", …).
	Window string
	// Util is the used fraction (0..1). Unknown when HasUtil is false.
	Util    float64
	HasUtil bool
	// Reached is true when the backend reports the window's limit as
	// reached (CC status=rejected, codex reached signal).
	Reached bool
	// ResetAt is when the window resets (zero = unknown).
	ResetAt time.Time
}

// Window is one window's stored state while it holds or stops the account.
type Window struct {
	Name    string    `json:"name"`
	Util    float64   `json:"util,omitempty"`
	Reached bool      `json:"reached,omitempty"`
	ResetAt time.Time `json:"reset_at"`
}

// State is an account's state at one moment.
type State struct {
	Phase Phase
	// LiftAt is when the account lifts if nothing else is reported: the latest
	// reset of the windows that hold or stop it (zero when Open, or when a
	// holding window has no reset time).
	LiftAt time.Time
	// Windows are the windows that hold or stop it, sorted by name.
	Windows []Window
	// Epoch counts the account's changes from Open to Hold or Stop; it lets a
	// subscriber tell one hold from the next.
	Epoch uint64
}

// Primary is the window that names the state in a notice: a reached one
// first, else the one with the highest use.
func (s State) Primary() Window {
	var best Window
	for i, w := range s.Windows {
		if i == 0 || (w.Reached && !best.Reached) || (w.Reached == best.Reached && w.Util > best.Util) {
			best = w
		}
	}
	return best
}

// Change is one transition of an account's phase.
type Change struct {
	Account string
	From    Phase
	To      State
}

// Store persists each account's windows across a restart.
type Store interface {
	LoadUsageHold(account string) (string, bool)
	SaveUsageHold(account, value string)
}

type account struct {
	windows map[string]Window
	phase   Phase
	epoch   uint64
	loaded  bool
}

// Registry holds every account's state. One per process (Default); tests
// make their own.
type Registry struct {
	mu        sync.Mutex
	accounts  map[string]*account
	subs      map[string]map[int]func(Change)
	nextSub   int
	threshold float64 // fraction, e.g. 0.98
	store     Store
}

// New returns an empty registry with the default threshold.
func New() *Registry {
	return &Registry{
		accounts:  map[string]*account{},
		subs:      map[string]map[int]func(Change){},
		threshold: DefaultThresholdPercent / 100.0,
	}
}

// Default is the process's registry.
var Default = New()

// SetThresholdPercent sets the hold threshold (1..100). It applies to the next
// usage event; a window already holding stays held until its own clearing
// rule says otherwise. Out-of-range values are ignored (config validation
// rejects them first).
func (r *Registry) SetThresholdPercent(pct int) {
	if pct < 1 || pct > 100 {
		return
	}
	r.mu.Lock()
	r.threshold = float64(pct) / 100.0
	r.mu.Unlock()
}

// ThresholdPercent is the current hold threshold in percent.
func (r *Registry) ThresholdPercent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int(math.Round(r.threshold * 100))
}

// SetStore sets where account state persists. Call it once at startup,
// before any Observe.
func (r *Registry) SetStore(s Store) {
	r.mu.Lock()
	r.store = s
	r.mu.Unlock()
}

// Subscribe calls fn on every phase change of account, outside the
// registry's lock and in the goroutine that caused it. It returns the
// function that ends the subscription.
func (r *Registry) Subscribe(acct string, fn func(Change)) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subs[acct] == nil {
		r.subs[acct] = map[int]func(Change){}
	}
	id := r.nextSub
	r.nextSub++
	r.subs[acct][id] = fn
	return func() {
		r.mu.Lock()
		delete(r.subs[acct], id)
		r.mu.Unlock()
	}
}

// Observe records one usage event for acct at now and returns the account's
// state after it.
func (r *Registry) Observe(acct string, ev Event, now time.Time) State {
	r.mu.Lock()
	a := r.accountLocked(acct)
	switch {
	case ev.Reached:
		reset := ev.ResetAt
		if !reset.After(now) {
			reset = now.Add(rejectedFallback)
		}
		a.windows[ev.Window] = Window{Name: ev.Window, Util: 1, Reached: true, ResetAt: reset}
	case ev.HasUtil && ev.Util >= r.threshold-1e-9:
		if ev.ResetAt.IsZero() || ev.ResetAt.After(now) {
			a.windows[ev.Window] = Window{Name: ev.Window, Util: ev.Util, ResetAt: ev.ResetAt}
		}
	case ev.HasUtil:
		// Below the threshold and not reached: this window clears. Another
		// window's event never clears it.
		delete(a.windows, ev.Window)
	}
	return r.settleLocked(acct, a, now, true)
}

// Clear lifts acct at once, whatever its windows say (#3043, /limit-reset
// and a /usage read that finds usage available): every window is dropped,
// the empty state is persisted, and subscribers see the change to Open as
// at a natural lift. It returns the phase before and the state after. A
// later usage event at or above the threshold holds the account again.
func (r *Registry) Clear(acct string, now time.Time) (Phase, State) {
	r.mu.Lock()
	a := r.accountLocked(acct)
	from := a.phase
	changed := len(a.windows) > 0
	a.windows = map[string]Window{}
	return from, r.settleLocked(acct, a, now, changed)
}

// Tick expires windows whose reset time has passed, on every account, and
// returns nothing; changes go to the subscribers. Agents call it on their
// periodic tick, so a hold lifts at its reset time even when no event comes.
func (r *Registry) Tick(now time.Time) {
	r.mu.Lock()
	names := make([]string, 0, len(r.accounts))
	for n := range r.accounts {
		names = append(names, n)
	}
	r.mu.Unlock()
	for _, n := range names {
		r.State(n, now)
	}
}

// State is acct's state at now, after expiring windows past their reset.
func (r *Registry) State(acct string, now time.Time) State {
	r.mu.Lock()
	a := r.accountLocked(acct)
	return r.settleLocked(acct, a, now, false)
}

// accountLocked returns acct's state, loading it from the store the first
// time. Caller holds r.mu.
func (r *Registry) accountLocked(acct string) *account {
	a, ok := r.accounts[acct]
	if !ok {
		a = &account{windows: map[string]Window{}}
		r.accounts[acct] = a
	}
	if !a.loaded && r.store != nil {
		a.loaded = true
		if raw, ok := r.store.LoadUsageHold(acct); ok && raw != "" {
			var ws []Window
			if json.Unmarshal([]byte(raw), &ws) == nil {
				for _, w := range ws {
					a.windows[w.Name] = w
				}
				a.phase = phaseOf(a.windows)
				if a.phase != Open {
					a.epoch++
				}
			}
		}
	}
	return a
}

// settleLocked expires windows, recomputes the phase, persists a change and
// notifies subscribers. It releases r.mu.
func (r *Registry) settleLocked(acct string, a *account, now time.Time, changed bool) State {
	for n, w := range a.windows {
		if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
			delete(a.windows, n)
			changed = true
		}
	}
	from := a.phase
	to := phaseOf(a.windows)
	if from == Open && to != Open {
		a.epoch++
	}
	a.phase = to
	st := stateOf(a)
	if changed && r.store != nil {
		r.store.SaveUsageHold(acct, encodeWindows(st.Windows))
	}
	var subs []func(Change)
	if from != to {
		for _, fn := range r.subs[acct] {
			subs = append(subs, fn)
		}
	}
	r.mu.Unlock()
	for _, fn := range subs {
		fn(Change{Account: acct, From: from, To: st})
	}
	return st
}

func phaseOf(ws map[string]Window) Phase {
	p := Open
	for _, w := range ws {
		if w.Reached {
			return Stop
		}
		p = Hold
	}
	return p
}

func stateOf(a *account) State {
	st := State{Phase: a.phase, Epoch: a.epoch}
	unknownReset := false
	for _, w := range a.windows {
		st.Windows = append(st.Windows, w)
		if w.ResetAt.IsZero() {
			unknownReset = true
		} else if w.ResetAt.After(st.LiftAt) {
			st.LiftAt = w.ResetAt
		}
	}
	if unknownReset {
		st.LiftAt = time.Time{}
	}
	sort.Slice(st.Windows, func(i, j int) bool { return st.Windows[i].Name < st.Windows[j].Name })
	return st
}

func encodeWindows(ws []Window) string {
	if len(ws) == 0 {
		return ""
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

type permitKey struct{}

// WithPermit marks ctx's work as started by the hold itself (the reflection
// and compaction at a hold start, #2491 requirement 8): it runs through a
// HOLD. A stop still holds it.
func WithPermit(ctx context.Context) context.Context {
	return context.WithValue(ctx, permitKey{}, true)
}

// Permitted reports whether ctx carries WithPermit.
func Permitted(ctx context.Context) bool {
	ok, _ := ctx.Value(permitKey{}).(bool)
	return ok
}
