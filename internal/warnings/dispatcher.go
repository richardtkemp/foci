package warnings

import (
	"sync"
	"time"

	"foci/internal/log"
)

// DispatchFunc is called to dispatch a proactive warning turn.
// It receives the formatted warning text and should inject it as a user message
// into the agent's default session, triggering a full agent turn.
type DispatchFunc func(warningText string)

// DispatcherConfig holds all the dependencies for creating a Dispatcher.
type DispatcherConfig struct {
	Queue                 *Queue
	PeerQueues            []*Queue // additional queues to suppress during dispatch (prevents cross-queue feedback)
	DispatchFn            DispatchFunc
	FormatFn              func(body string) string // wraps body with injection header; nil = use body as-is
	ActiveInterval        time.Duration
	InactiveInterval      time.Duration
	ActivityThreshold     time.Duration
	LastUserMessageTimeFn func() time.Time
	IsProcessingFn        func() bool // if non-nil and returns true, defer dispatch until turn ends
}

// Dispatcher checks for pending warnings and dispatches them proactively
// as a user-role message. Rate limited by user activity: active interval if the user
// has been recently active, inactive interval otherwise.
type Dispatcher struct {
	log                   *log.ComponentLogger
	queue                 *Queue
	peerQueues            []*Queue
	dispatchFn            DispatchFunc
	formatFn              func(body string) string
	activeInterval        time.Duration
	inactiveInterval      time.Duration
	activityThreshold     time.Duration
	lastUserMessageTimeFn func() time.Time
	isProcessingFn        func() bool

	mu           sync.Mutex
	lastDispatch time.Time
	dispatching  bool
}

// NewDispatcher creates a Dispatcher from the given config.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	return &Dispatcher{
		log:                   log.NewComponentLogger("warnings"),
		queue:                 cfg.Queue,
		peerQueues:            cfg.PeerQueues,
		dispatchFn:            cfg.DispatchFn,
		formatFn:              cfg.FormatFn,
		activeInterval:        cfg.ActiveInterval,
		inactiveInterval:      cfg.InactiveInterval,
		activityThreshold:     cfg.ActivityThreshold,
		lastUserMessageTimeFn: cfg.LastUserMessageTimeFn,
		isProcessingFn:        cfg.IsProcessingFn,
	}
}

// MaybeFire checks for pending warnings and dispatches them if the rate limit allows.
// If IsProcessingFn is set and returns true, dispatch is deferred — warnings stay
// in the queue and will be flushed via FlushPending after the turn ends.
func (d *Dispatcher) MaybeFire() {
	if d.queue == nil || d.dispatchFn == nil {
		return
	}

	if !d.queue.Pending() {
		return
	}

	// Defer dispatch while an agent turn is active.
	if d.isProcessingFn != nil && d.isProcessingFn() {
		return
	}

	d.mu.Lock()
	dispatching := d.dispatching
	sinceLastDispatch := time.Since(d.lastDispatch)
	d.mu.Unlock()

	if dispatching {
		return
	}

	// Determine rate limit interval based on user activity
	interval := d.inactiveInterval
	if d.lastUserMessageTimeFn != nil {
		lastMsg := d.lastUserMessageTimeFn()
		if !lastMsg.IsZero() && time.Since(lastMsg) < d.activityThreshold {
			interval = d.activeInterval
		}
	}

	if sinceLastDispatch < interval {
		return
	}

	d.dispatchDrained()
}

// FlushPending dispatches any pending warnings without rate-limit checks.
// Called after an agent turn ends to flush warnings that were deferred.
func (d *Dispatcher) FlushPending() {
	if d.queue == nil || d.dispatchFn == nil {
		return
	}
	if !d.queue.Pending() {
		return
	}

	d.mu.Lock()
	dispatching := d.dispatching
	d.mu.Unlock()
	if dispatching {
		return
	}

	d.dispatchDrained()
}

// dispatchDrained drains the warning queue, formats the result, and launches
// a goroutine to dispatch the formatted text.
func (d *Dispatcher) dispatchDrained() {
	warnings := d.queue.Drain()
	if len(warnings) == 0 {
		return
	}

	body := FormatList(warnings)

	text := body
	if d.formatFn != nil {
		text = d.formatFn(body)
	}

	d.mu.Lock()
	d.dispatching = true
	d.lastDispatch = time.Now()
	d.mu.Unlock()

	d.log.Infof("dispatching %d proactive warnings", len(warnings))

	d.queue.Suppress()
	for _, pq := range d.peerQueues {
		if pq != nil {
			pq.Suppress()
		}
	}
	go func() {
		defer func() {
			d.queue.Unsuppress()
			for _, pq := range d.peerQueues {
				if pq != nil {
					pq.Unsuppress()
				}
			}
			d.mu.Lock()
			d.dispatching = false
			d.mu.Unlock()
		}()
		d.dispatchFn(text)
	}()
}
