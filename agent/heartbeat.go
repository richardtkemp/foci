package agent

import (
	"context"
	"sync"
	"time"

	"clod/log"
)

// Heartbeat fires when a session has been idle for a configurable duration.
type Heartbeat struct {
	agent        *Agent
	sessionKey   string        // static session key (legacy)
	SessionKeyFn func() string // dynamic session key resolver (overrides sessionKey)
	interval     time.Duration
	timer        *time.Timer
	mu           sync.Mutex
	cancel       context.CancelFunc
}

// NewHeartbeat creates a new heartbeat for the given session.
func NewHeartbeat(ag *Agent, sessionKey string, interval time.Duration) *Heartbeat {
	return &Heartbeat{
		agent:      ag,
		sessionKey: sessionKey,
		interval:   interval,
	}
}

// Start begins the heartbeat timer.
func (h *Heartbeat) Start(ctx context.Context) {
	ctx, h.cancel = context.WithCancel(ctx)

	h.mu.Lock()
	h.timer = time.NewTimer(h.interval)
	h.mu.Unlock()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.timer.C:
				h.fire(ctx)
				h.mu.Lock()
				h.timer.Reset(h.interval)
				h.mu.Unlock()
			}
		}
	}()
}

// Reset resets the heartbeat timer. Call this on any activity.
func (h *Heartbeat) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.timer != nil {
		h.timer.Reset(h.interval)
	}
}

// Stop stops the heartbeat.
func (h *Heartbeat) Stop() {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
}

func (h *Heartbeat) fire(ctx context.Context) {
	sk := h.sessionKey
	if h.SessionKeyFn != nil {
		sk = h.SessionKeyFn()
	}
	if sk == "" {
		log.Debugf("heartbeat", "no session key, skipping")
		return
	}
	log.Infof("heartbeat", "firing for session %s", sk)

	resp, err := h.agent.HandleMessage(WithTrigger(ctx, "heartbeat"), sk, "[HEARTBEAT] The idle timer has fired. Check HEARTBEAT.md for instructions on what to do during idle time.")
	if err != nil {
		log.Errorf("heartbeat", "error: %v", err)
		return
	}

	log.Debugf("heartbeat", "response: %s", truncateStr(resp, 200))
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
