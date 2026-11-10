// Package epdispatch orders foci's model calls to a local model server by
// priority and caps how many run at once (#3019).
//
// A local model server (Ollama) runs a fixed number of requests at once and
// has no request priority: it serves its queue first in, first out. An
// [endpoints] entry opts in by setting max_in_flight > 0 (the work Mac's local
// Ollama sets 1); the default, 0, engages nothing: no tiers, no holds, calls
// go straight out. OpenRouter never engages. An engaged endpoint has one
// Dispatcher, and every foci call to it (any agent's or spawn's turn, the fast
// summaries) passes through it (Engage). A call has a tier:
//
//	High   user-facing agent turns (a user's message and the turns it drives),
//	       pin labels, Stop-view step summaries
//	Medium automatic chat names, chat sections
//	Low    background agent turns (keepalive, reflection, consolidation, ...)
//
// The tier rules (Dick, 2026-11-10 19:45):
//   - a High call is never held by the tiers;
//   - a Medium call is not sent while a High or another Medium call is in
//     flight: Medium calls go one at a time, when no High call runs;
//   - a Low call is not sent while a High or Medium call is in flight or a
//     Medium call waits; Low calls do not hold each other back.
//
// The cap: [endpoints.*] max_in_flight. At most that many calls of any tier
// are in flight, and the next goes when one returns (Dick 19:48: "local
// models can only execute one query at a time. We send a request, we wait.
// When it returns, we dispatch the next one."). The default is 0 (Dick 19:53:
// "The default should absolutely not be one! Should be zero, no limit"), and
// an endpoint at 0 is not dispatched at all (clutch's review, 2026-11-10: the
// rulings are about local servers; holding background calls behind a cloud
// API's traffic would be a regression). The server's parallel slots
// (OLLAMA_NUM_PARALLEL) are set by hand: Ollama does not report them over
// its API.
//
// Waiting calls go highest tier first, first come first served within a
// tier. A waiting call with a key (a Medium check: task and chat) is replaced
// when a newer call with the same key arrives, or when Replace is called for
// that key: the older one returns ErrReplaced and is not sent (Dick's ruling
// Q61: "Keep only the newest per chat ... no time limit"). A wait has no time
// limit; only the caller's context ends it.
//
// A streamed call is in flight until its response ends: the caller releases
// its slot when the call returns.
//
// foci sees only its own calls. Another client of the same server (a second
// foci, a desktop app) is invisible here and still queues inside the server.
// With max_in_flight set to the server's slots, foci never has more requests
// at the server than it runs at once, so a foci request queues there only
// behind another client's.
package epdispatch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tier is a call's priority on a dispatched endpoint.
type Tier int

// The tiers, highest first.
const (
	High Tier = iota
	Medium
	Low
	numTiers
)

func (t Tier) String() string {
	switch t {
	case High:
		return "high"
	case Medium:
		return "medium"
	case Low:
		return "low"
	}
	return fmt.Sprintf("tier(%d)", int(t))
}

// ErrReplaced is returned to a waiting call that a newer call with the same
// key replaced. The call was not sent.
var ErrReplaced = errors.New("replaced by a newer call of the same task and chat while waiting")

// DefaultMaxInFlight is an endpoint's max_in_flight when it sets none: 0,
// not dispatched.
const DefaultMaxInFlight = 0

// Applies reports whether calls to the [endpoints] entry name, with the given
// max_in_flight and base URLs (any format's; empty ones ignored), go through
// a Dispatcher: max_in_flight > 0 and the entry is not OpenRouter (by name or
// an openrouter.ai URL).
func Applies(name string, maxInFlight int, urls ...string) bool {
	if maxInFlight <= 0 || strings.EqualFold(name, "openrouter") {
		return false
	}
	for _, u := range urls {
		if isOpenRouter(u) {
			return false
		}
	}
	return true
}

// Engage returns the Dispatcher of the [endpoints] entry name with its cap
// set to maxInFlight, or nil when the entry is not dispatched (Applies). The
// callers read max_in_flight from their own config at each call, so an edit
// that turns it on or off applies to the next call.
func Engage(name string, maxInFlight int, urls ...string) *Dispatcher {
	if !Applies(name, maxInFlight, urls...) {
		return nil
	}
	d := For(name)
	d.mu.Lock()
	if d.limit != maxInFlight {
		d.limit = maxInFlight
		d.fillLocked()
	}
	d.mu.Unlock()
	return d
}

func isOpenRouter(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	host := raw
	if p, err := url.Parse(raw); err == nil && p.Host != "" {
		host = p.Hostname()
	}
	host = strings.ToLower(host)
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// waiter is one call waiting for a slot.
type waiter struct {
	tier  Tier
	key   string
	since time.Time
	ready chan struct{} // closed when admitted or replaced
	err   error         // ErrReplaced when replaced; set before ready closes
	// admitted: a slot was handed to this call. parked: the call waited
	// (it was not admitted as it arrived). Guarded by Dispatcher.mu.
	admitted, parked bool
}

// tierStats counts one tier's calls since the process started.
type tierStats struct {
	calls    int           // admitted (sent)
	held     int           // admitted or replaced after a wait
	replaced int           // replaced while waiting
	maxWait  time.Duration // longest wait
}

// Dispatcher orders the calls to one endpoint. Use For.
type Dispatcher struct {
	name string

	mu       sync.Mutex
	limit    int // calls that may be in flight at once; 0 = no limit
	inFlight [numTiers]int
	queues   [numTiers][]*waiter
	stats    [numTiers]tierStats
	now      func() time.Time // nil = time.Now; a test sets it
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Dispatcher{}
)

// For returns the process-wide Dispatcher of the [endpoints] entry name.
func For(name string) *Dispatcher {
	registryMu.Lock()
	defer registryMu.Unlock()
	d, ok := registry[name]
	if !ok {
		d = &Dispatcher{name: name}
		registry[name] = d
	}
	return d
}

// SetLimits sets each named endpoint's max_in_flight. The client registry
// calls it at startup and on an [endpoints] edit, with every entry of the
// config, so a raised cap sends waiting calls at once. 0 lifts the cap: the
// endpoint is no longer dispatched (callers stop calling Engage's
// dispatcher), and its waiting calls go. An endpoint absent from limits keeps
// its limit.
func SetLimits(limits map[string]int) {
	for n, l := range limits {
		For(n).SetLimit(l)
	}
}

// SetLimit sets how many calls may be in flight at once (n < 1 = no limit).
// A raised limit sends waiting calls at once; a lowered one lets the calls in
// flight finish and sends no more until fewer than n run.
func (d *Dispatcher) SetLimit(n int) {
	if n < 0 {
		n = 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limit = n
	d.fillLocked()
}

func (d *Dispatcher) runningLocked() int {
	n := 0
	for _, c := range d.inFlight {
		n += c
	}
	return n
}

// admissibleLocked reports whether a call of tier t may be sent now, by the
// cap and the tier rules (package comment). Callers hold d.mu.
func (d *Dispatcher) admissibleLocked(t Tier) bool {
	if d.limit > 0 && d.runningLocked() >= d.limit {
		return false
	}
	switch t {
	case High:
		return true
	case Medium:
		return d.inFlight[High] == 0 && d.inFlight[Medium] == 0
	default:
		// The waiting-queue terms restate the rule; fillLocked's tier order
		// already sends a waiting High or Medium call first.
		return d.inFlight[High] == 0 && d.inFlight[Medium] == 0 &&
			len(d.queues[High]) == 0 && len(d.queues[Medium]) == 0
	}
}

// Acquire waits until this call may be sent to the endpoint and returns the
// function that frees its slot; call it once when the call has returned (a
// streamed call: when its response has ended). wait is how long the call
// waited (0 = sent at once).
//
// key, when not empty, names the call's task and chat: a waiting call with
// the same tier and key is replaced by this one (it gets ErrReplaced). The
// wait ends early only when ctx ends (ctx.Err()) or the call is replaced
// (ErrReplaced, with the time it waited); release is nil then.
func (d *Dispatcher) Acquire(ctx context.Context, tier Tier, key string) (release func(), wait time.Duration, err error) {
	if tier < High || tier >= numTiers {
		tier = High
	}
	d.mu.Lock()
	if key != "" {
		d.replaceLocked(tier, key)
	}
	w := &waiter{tier: tier, key: key, since: d.clock(), ready: make(chan struct{})}
	d.queues[tier] = append(d.queues[tier], w)
	d.fillLocked()
	if w.admitted {
		d.mu.Unlock()
		return d.releaseFunc(tier), 0, nil
	}
	w.parked = true
	d.mu.Unlock()

	select {
	case <-w.ready:
	case <-ctx.Done():
		d.mu.Lock()
		switch {
		case w.admitted: // handed a slot as ctx ended: pass it on
			d.mu.Unlock()
			d.releaseFunc(tier)()
			return nil, 0, ctx.Err()
		case w.err != nil: // replaced as ctx ended
			wait = d.clock().Sub(w.since)
			d.mu.Unlock()
			return nil, wait, w.err
		}
		d.removeLocked(w)
		d.fillLocked() // a Low call may have waited only on this one
		d.mu.Unlock()
		return nil, 0, ctx.Err()
	}
	d.mu.Lock()
	wait = d.clock().Sub(w.since)
	d.mu.Unlock()
	if w.err != nil {
		return nil, wait, w.err
	}
	return d.releaseFunc(tier), wait, nil
}

// Replace replaces the waiting call of tier Medium with key, if one waits: it
// returns ErrReplaced and is not sent. It reports whether one was replaced.
// For a newer check of the same task and chat that cannot reach Acquire while
// the older one waits (the older one holds the chat's claim).
func (d *Dispatcher) Replace(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	replaced := d.replaceLocked(Medium, key)
	if replaced {
		d.fillLocked() // a Low call may have waited only on the replaced one
	}
	return replaced
}

// replaceLocked replaces the waiting calls of tier with key. Callers hold d.mu.
func (d *Dispatcher) replaceLocked(tier Tier, key string) bool {
	q := d.queues[tier]
	keep := q[:0]
	replaced := false
	now := d.clock()
	for _, w := range q {
		if w.key != key {
			keep = append(keep, w)
			continue
		}
		replaced = true
		w.err = ErrReplaced
		st := &d.stats[tier]
		st.held++
		st.replaced++
		if wt := now.Sub(w.since); wt > st.maxWait {
			st.maxWait = wt
		}
		close(w.ready)
	}
	clear(q[len(keep):])
	d.queues[tier] = keep
	return replaced
}

// removeLocked takes w out of its queue. Callers hold d.mu.
func (d *Dispatcher) removeLocked(w *waiter) {
	q := d.queues[w.tier]
	for i, x := range q {
		if x == w {
			d.queues[w.tier] = append(q[:i], q[i+1:]...)
			return
		}
	}
}

// releaseFunc returns the once-only release of a slot a call of tier holds.
func (d *Dispatcher) releaseFunc(tier Tier) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.inFlight[tier]--
			d.fillLocked()
		})
	}
}

// fillLocked sends every waiting call the cap and the tier rules allow,
// highest tier first and in arrival order within a tier. Callers hold d.mu.
func (d *Dispatcher) fillLocked() {
	now := d.clock()
	for {
		var w *waiter
		for t := High; t < numTiers; t++ {
			if len(d.queues[t]) > 0 && d.admissibleLocked(t) {
				q := d.queues[t]
				w = q[0]
				q[0] = nil
				d.queues[t] = q[1:]
				break
			}
		}
		if w == nil {
			return
		}
		w.admitted = true
		d.inFlight[w.tier]++
		st := &d.stats[w.tier]
		st.calls++
		if w.parked {
			st.held++
			if wt := now.Sub(w.since); wt > st.maxWait {
				st.maxWait = wt
			}
			close(w.ready)
		}
	}
}

// Status is a Dispatcher's state now and its counts since the process
// started, by tier.
type Status struct {
	Endpoint string
	Limit    int // calls that may be in flight at once; 0 = no limit
	InFlight [numTiers]int
	Waiting  [numTiers]int
	Calls    [numTiers]int           // sent
	Held     [numTiers]int           // waited before being sent or replaced
	Replaced [numTiers]int           // replaced while waiting
	MaxWait  [numTiers]time.Duration // longest wait
}

// Status returns d's state.
func (d *Dispatcher) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Status{Endpoint: d.name, Limit: d.limit}
	for t := High; t < numTiers; t++ {
		s.InFlight[t] = d.inFlight[t]
		s.Waiting[t] = len(d.queues[t])
		s.Calls[t] = d.stats[t].calls
		s.Held[t] = d.stats[t].held
		s.Replaced[t] = d.stats[t].replaced
		s.MaxWait[t] = d.stats[t].maxWait
	}
	return s
}

// Report is the /status section: for each dispatched endpoint (max_in_flight
// > 0) that has had a call, the calls in flight and waiting now and the
// counts since start, by tier. "" when there is none.
func Report() string {
	registryMu.Lock()
	ds := make([]*Dispatcher, 0, len(registry))
	for _, d := range registry {
		ds = append(ds, d)
	}
	registryMu.Unlock()
	sort.Slice(ds, func(i, j int) bool { return ds[i].name < ds[j].name })

	var sb strings.Builder
	for _, d := range ds {
		s := d.Status()
		total := 0
		for t := High; t < numTiers; t++ {
			total += s.Calls[t] + s.Replaced[t] + s.Waiting[t]
		}
		if s.Limit <= 0 || total == 0 {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("🚦 Endpoint dispatch (tier order; counts since start)\n")
		}
		fmt.Fprintf(&sb, "   %s (max_in_flight %d): in flight high %d, medium %d, low %d; waiting high %d, medium %d, low %d\n",
			s.Endpoint, s.Limit, s.InFlight[High], s.InFlight[Medium], s.InFlight[Low],
			s.Waiting[High], s.Waiting[Medium], s.Waiting[Low])
		for t := High; t < numTiers; t++ {
			if s.Calls[t]+s.Replaced[t] == 0 {
				continue
			}
			fmt.Fprintf(&sb, "     %s: sent %d, held %d (max wait %s), replaced %d\n",
				t, s.Calls[t], s.Held[t], s.MaxWait[t].Round(time.Second), s.Replaced[t])
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
