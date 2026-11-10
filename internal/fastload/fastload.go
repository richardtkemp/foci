// Package fastload measures the load the app's fast-summary tasks put on the
// model each one calls (#3012): the pin label, the Stop-view subagent
// summaries, the automatic chat name and the chat sections. A slow local
// model is the case it is for: Dick tunes how often those tasks run
// (auto_name_every, chat_sections_every, ...) from these figures.
//
// Dick's four figures (2026-11-10 15:17): "how often do we run, what % of the
// time during active periods is the endpoint in use, what % of requests
// queue, what is a typical queue length". Load reports them per target (one
// model on one endpoint), for the last hour and the last 24 hours:
//
//   - Runs: the calls started in the window, by task.
//   - In use: the time at least one call to the target was in flight, as a
//     share of the active time in the window. An active minute is a minute in
//     which an app chat message was stored (MarkActive) or a call to the
//     target was in flight.
//   - Queued: the share of calls that started while another call to the same
//     target was still in flight. Unless the endpoint sets max_in_flight
//     (#3019), foci does not limit how many calls run at once, so such a call
//     waits in the model server's queue whenever the server runs one request
//     at a time (a local Ollama by default); on a server that runs several at
//     once it is the overlap, not a wait.
//   - Queue length: for each queued call, how many calls to the target were
//     in flight when it started. Typical is the median of those; max is the
//     largest.
//   - Held (#3019): on an endpoint that sets max_in_flight (a local model
//     server), foci itself holds a call back (internal/epdispatch): a chat
//     name or sections call waits for the higher-priority calls, and every
//     call waits for a free slot. Held is how many calls waited there,
//     with the typical (median) and longest wait, and how many of them a
//     newer check of the same chat replaced before they were sent. A held
//     call's run starts when it is sent, so its wait is not in the other
//     figures.
//
// The record is in memory only, kept for 24 hours, and starts empty at each
// start of the process.
package fastload

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Retention is how long a call or an active minute is kept.
const Retention = 24 * time.Hour

// maxCalls bounds the call record: a runaway task cannot grow it without end.
// At 20,000 calls a day the oldest are dropped first.
const maxCalls = 20000

// hold is one call foci held back before sending it (#3019).
type hold struct {
	target   string
	task     string
	at       time.Time // when the wait ended
	wait     time.Duration
	replaced bool
}

type call struct {
	target  string
	task    string
	start   time.Time
	end     time.Time // zero while in flight
	ahead   int       // calls to target in flight when this one started
	failed  bool
	pending bool
}

// Recorder holds the call record. The zero value is not usable: use New.
type Recorder struct {
	mu       sync.Mutex
	calls    []*call
	holds    []hold
	inFlight map[string]int   // target → calls in flight
	active   map[int64]bool   // unix minute → an app chat message was stored
	started  time.Time        // when the record began (process start)
	now      func() time.Time // nil = time.Now; a test sets it
}

// New returns an empty Recorder.
func New() *Recorder {
	r := &Recorder{inFlight: map[string]int{}, active: map[int64]bool{}}
	r.started = r.clock()
	return r
}

// Default is the process-wide recorder the app writes and /status reads.
var Default = New()

func (r *Recorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// SetClock replaces the recorder's clock (tests only) and restarts its record
// at the new clock's time.
func (r *Recorder) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
	r.started = r.clock()
}

// Target names a model on an endpoint, the unit the figures are kept for.
func Target(model, endpoint string) string { return model + " @ " + endpoint }

// MarkActive records that the user's chats were active now (an app chat
// message was stored).
func (r *Recorder) MarkActive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active[r.clock().Unix()/60] = true
}

// Begin records the start of one call of task to target. ahead is the number
// of calls to target already in flight. Call end once when the call ends,
// with whether it failed; later calls of end are ignored.
func (r *Recorder) Begin(target, task string) (ahead int, end func(failed bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.prune(now)
	c := &call{target: target, task: task, start: now, ahead: r.inFlight[target], pending: true}
	r.inFlight[target]++
	r.calls = append(r.calls, c)
	var once sync.Once
	return c.ahead, func(failed bool) {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			c.end, c.failed, c.pending = r.clock(), failed, false
			if r.inFlight[target]--; r.inFlight[target] <= 0 {
				delete(r.inFlight, target)
			}
		})
	}
}

// Held records that a call of task to target waited wait in foci before it
// was sent, or before a newer call replaced it (replaced).
func (r *Recorder) Held(target, task string, wait time.Duration, replaced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.prune(now)
	r.holds = append(r.holds, hold{target: target, task: task, at: now, wait: wait, replaced: replaced})
}

// InFlight is the number of calls to target in flight now.
func (r *Recorder) InFlight(target string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight[target]
}

// prune drops calls and active minutes older than Retention, and the oldest
// finished calls beyond maxCalls. Callers hold r.mu.
func (r *Recorder) prune(now time.Time) {
	cut := now.Add(-Retention)
	keep := r.calls[:0]
	for _, c := range r.calls {
		if c.pending || c.end.After(cut) {
			keep = append(keep, c)
		}
	}
	clear(r.calls[len(keep):])
	r.calls = keep
	if over := len(r.calls) - maxCalls; over > 0 {
		r.calls = slices.Delete(r.calls, 0, over)
	}
	keepH := r.holds[:0]
	for _, h := range r.holds {
		if h.at.After(cut) {
			keepH = append(keepH, h)
		}
	}
	r.holds = keepH
	if over := len(r.holds) - maxCalls; over > 0 {
		r.holds = slices.Delete(r.holds, 0, over)
	}
	for m := range r.active {
		if m < cut.Unix()/60 {
			delete(r.active, m)
		}
	}
}

// Load is one target's figures over one window.
type Load struct {
	Target   string
	Window   time.Duration // the window asked for
	Covered  time.Duration // the part of the window the record covers (less after a restart)
	Runs     int
	ByTask   map[string]int
	Failed   int
	InFlight int // now

	Busy       time.Duration // time with at least one call in flight
	ActiveTime time.Duration // active minutes × 1 minute
	Queued     int           // calls that started behind another call
	QueueP50   int           // median calls ahead, over the queued calls
	QueueMax   int
	LatencyP50 time.Duration // over the finished calls

	Held     int           // calls foci held back by priority or max_in_flight (#3019)
	Replaced int           // of those, replaced by a newer check before being sent
	HoldP50  time.Duration // median wait of the held calls
	HoldMax  time.Duration
}

// InUsePct is Busy as a percentage of ActiveTime (0 with no active time).
func (l Load) InUsePct() float64 {
	if l.ActiveTime <= 0 {
		return 0
	}
	return 100 * float64(l.Busy) / float64(l.ActiveTime)
}

// QueuedPct is the queued calls as a percentage of the runs.
func (l Load) QueuedPct() float64 {
	if l.Runs == 0 {
		return 0
	}
	return 100 * float64(l.Queued) / float64(l.Runs)
}

// PerHour is the runs per hour over the covered part of the window.
func (l Load) PerHour() float64 {
	if l.Covered <= 0 {
		return 0
	}
	return float64(l.Runs) / l.Covered.Hours()
}

// Loads returns each target's figures over the last window, targets in name
// order. A target with no call in the window is left out.
func (r *Recorder) Loads(window time.Duration) []Load {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.prune(now)
	from := now.Add(-window)
	covered := window
	if from.Before(r.started) {
		from = r.started
		covered = now.Sub(r.started)
	}

	holdsBy := map[string][]hold{}
	for _, h := range r.holds {
		if !h.at.Before(from) {
			holdsBy[h.target] = append(holdsBy[h.target], h)
		}
	}
	byTarget := map[string][]*call{}
	for target := range holdsBy {
		byTarget[target] = nil // a target whose calls were all replaced still shows
	}
	for _, c := range r.calls {
		end := c.end
		if c.pending {
			end = now
		}
		if end.Before(from) && !c.pending {
			continue
		}
		byTarget[c.target] = append(byTarget[c.target], c)
	}

	var out []Load
	for target, calls := range byTarget {
		l := Load{Target: target, Window: window, Covered: covered, ByTask: map[string]int{}, InFlight: r.inFlight[target]}
		var spans [][2]time.Time
		var aheads []int
		var lat []time.Duration
		minutes := map[int64]bool{}
		for m := range r.active {
			if m >= from.Unix()/60 {
				minutes[m] = true
			}
		}
		for _, c := range calls {
			end := c.end
			if c.pending {
				end = now
			}
			s := c.start
			if s.Before(from) {
				s = from
			}
			if end.After(s) {
				spans = append(spans, [2]time.Time{s, end})
			}
			for m := s.Unix() / 60; m <= end.Unix()/60; m++ {
				minutes[m] = true
			}
			if c.start.Before(from) {
				continue // in flight into the window: counts as busy time, not as a run
			}
			l.Runs++
			l.ByTask[c.task]++
			if c.failed {
				l.Failed++
			}
			if c.ahead > 0 {
				l.Queued++
				aheads = append(aheads, c.ahead)
			}
			if !c.pending {
				lat = append(lat, c.end.Sub(c.start))
			}
		}
		var waits []time.Duration
		for _, h := range holdsBy[target] {
			l.Held++
			if h.replaced {
				l.Replaced++
			}
			waits = append(waits, h.wait)
		}
		if len(waits) > 0 {
			slices.Sort(waits)
			l.HoldP50 = waits[(len(waits)-1)/2]
			l.HoldMax = waits[len(waits)-1]
		}
		if l.Runs == 0 && l.InFlight == 0 && l.Held == 0 {
			continue
		}
		l.Busy = unionLength(spans)
		l.ActiveTime = time.Duration(len(minutes)) * time.Minute
		if len(aheads) > 0 {
			sort.Ints(aheads)
			l.QueueP50 = aheads[(len(aheads)-1)/2]
			l.QueueMax = aheads[len(aheads)-1]
		}
		if len(lat) > 0 {
			slices.Sort(lat)
			l.LatencyP50 = lat[(len(lat)-1)/2]
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// unionLength is the total length covered by spans, overlaps counted once.
func unionLength(spans [][2]time.Time) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0].Before(spans[j][0]) })
	var total time.Duration
	cur := spans[0]
	for _, s := range spans[1:] {
		if s[0].After(cur[1]) {
			total += cur[1].Sub(cur[0])
			cur = s
			continue
		}
		if s[1].After(cur[1]) {
			cur[1] = s[1]
		}
	}
	return total + cur[1].Sub(cur[0])
}

// Report is the /status section: each target's figures for the last hour,
// then the last 24 hours. "" when no fast-summary call ran in 24 hours.
func (r *Recorder) Report() string {
	day := r.Loads(Retention)
	if len(day) == 0 {
		return ""
	}
	hour := map[string]Load{}
	for _, l := range r.Loads(time.Hour) {
		hour[l.Target] = l
	}
	var sb strings.Builder
	sb.WriteString("⚡ Fast-summary load (last hour | last 24h)\n")
	for _, d := range day {
		h := hour[d.Target]
		h.Target = d.Target
		fmt.Fprintf(&sb, "   %s\n", d.Target)
		fmt.Fprintf(&sb, "     Runs: %d | %d (%.1f/h)%s\n", h.Runs, d.Runs, d.PerHour(), taskList(d.ByTask))
		fmt.Fprintf(&sb, "     In use: %.0f%% | %.0f%% of active time (%s | %s active)\n",
			h.InUsePct(), d.InUsePct(), roundDur(h.ActiveTime), roundDur(d.ActiveTime))
		fmt.Fprintf(&sb, "     Queued: %.0f%% | %.0f%% of runs; queue length typical %s, max %s\n",
			h.QueuedPct(), d.QueuedPct(), queueLen(h, d, false), queueLen(h, d, true))
		fmt.Fprintf(&sb, "     Latency p50: %s | %s; failed %d | %d; in flight now %d\n",
			roundDur(h.LatencyP50), roundDur(d.LatencyP50), h.Failed, d.Failed, d.InFlight)
		if d.Held > 0 {
			fmt.Fprintf(&sb, "     Held by foci: %d | %d; wait typical %s | %s, max %s | %s; replaced %d | %d\n",
				h.Held, d.Held, roundDur(h.HoldP50), roundDur(d.HoldP50), roundDur(h.HoldMax), roundDur(d.HoldMax), h.Replaced, d.Replaced)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func queueLen(h, d Load, max bool) string {
	f := func(l Load) string {
		if l.Queued == 0 {
			return "-"
		}
		if max {
			return fmt.Sprint(l.QueueMax)
		}
		return fmt.Sprint(l.QueueP50)
	}
	return f(h) + " | " + f(d)
}

func taskList(by map[string]int) string {
	if len(by) == 0 {
		return ""
	}
	tasks := make([]string, 0, len(by))
	for t := range by {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)
	parts := make([]string, len(tasks))
	for i, t := range tasks {
		parts[i] = fmt.Sprintf("%s %d", t, by[t])
	}
	return " — 24h: " + strings.Join(parts, ", ")
}

func roundDur(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}
