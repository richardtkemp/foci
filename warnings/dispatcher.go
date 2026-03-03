package warnings

import (
	"sync"
	"time"

	"foci/log"
)

// DispatchFunc is called to dispatch a proactive warning turn.
// It receives the formatted warning text and should inject it as a user message
// into the agent's default session, triggering a full agent turn.
type DispatchFunc func(warningText string)

// DispatcherConfig holds all the dependencies for creating a Dispatcher.
type DispatcherConfig struct {
	Queue                 *Queue
	DispatchFn            DispatchFunc
	FormatFn              func(body string) string // wraps body with injection header; nil = use body as-is
	ActiveInterval        time.Duration
	InactiveInterval      time.Duration
	ActivityThreshold     time.Duration
	LastUserMessageTimeFn func() time.Time
}

// Dispatcher checks for pending warnings and dispatches them proactively
// as a user-role message. Rate limited by user activity: active interval if the user
// has been recently active, inactive interval otherwise.
type Dispatcher struct {
	log                   *log.ComponentLogger
	queue                 *Queue
	dispatchFn            DispatchFunc
	formatFn              func(body string) string
	activeInterval        time.Duration
	inactiveInterval      time.Duration
	activityThreshold     time.Duration
	lastUserMessageTimeFn func() time.Time

	mu            sync.Mutex
	lastDispatch  time.Time
	dispatching   bool
}

// NewDispatcher creates a Dispatcher from the given config.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	return &Dispatcher{
		log:                   log.NewComponentLogger("warnings"),
		queue:                 cfg.Queue,
		dispatchFn:            cfg.DispatchFn,
		formatFn:              cfg.FormatFn,
		activeInterval:        cfg.ActiveInterval,
		inactiveInterval:      cfg.InactiveInterval,
		activityThreshold:     cfg.ActivityThreshold,
		lastUserMessageTimeFn: cfg.LastUserMessageTimeFn,
	}
}

// MaybeFire checks for pending warnings and dispatches them if the rate limit allows.
func (d *Dispatcher) MaybeFire() {
	if d.queue == nil || d.dispatchFn == nil {
		return
	}

	if !d.queue.Pending() {
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

	// Drain and format
	warnings := d.queue.Drain()
	if len(warnings) == 0 {
		return
	}

	body := ""
	for i, w := range warnings {
		if i > 0 {
			body += "\n"
		}
		body += "- " + w
	}

	text := body
	if d.formatFn != nil {
		text = d.formatFn(body)
	}

	d.mu.Lock()
	d.dispatching = true
	d.lastDispatch = time.Now()
	d.mu.Unlock()

	d.log.Infof("dispatching %d proactive warnings", len(warnings))

	go func() {
		defer func() {
			d.mu.Lock()
			d.dispatching = false
			d.mu.Unlock()
		}()
		d.dispatchFn(text)
	}()
}
