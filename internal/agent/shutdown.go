package agent

import (
	"errors"
	"sync"
	"time"
)

// ErrShuttingDown is returned when work that would begin something new — a
// system turn, a backend spawn — is refused because the process is draining
// for shutdown (#2059). It is not a failure: the work was never started, so a
// caller holding a durable record of it (a scheduled wake) leaves that record
// pending for the next process to pick up.
var ErrShuttingDown = errors.New("agent: shutting down")

// shutdownGate is the agent's drain latch. Once begun it never reopens: the
// process is exiting. Lazily initialised so a zero Agent (tests) is valid.
type shutdownGate struct {
	mu    sync.Mutex
	begun bool
	ch    chan struct{}
}

func (g *shutdownGate) done() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ch == nil {
		g.ch = make(chan struct{})
	}
	return g.ch
}

func (g *shutdownGate) begin() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.begun {
		return
	}
	g.begun = true
	if g.ch == nil {
		g.ch = make(chan struct{})
	}
	close(g.ch)
}

func (g *shutdownGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.begun
}

// BeginShutdown puts the agent into drain mode (#2059). Turns already running
// are left to finish — graceful shutdown waits for them — but nothing NEW
// begins:
//
//   - the session inbox runs no further injections (they stay unrun, so their
//     producers' durable records survive the restart);
//   - a system (non-interactive) turn is refused at dispatch, including one
//     already waiting for an in-flight turn to clear;
//   - post-turn compaction is skipped;
//   - no delegated backend is created or respawned.
//
// The gate sits at the backend-agnostic layer, so it applies equally to every
// transport (API) and every delegated backend (claude-code, opencode, codex).
// Idempotent.
func (a *Agent) BeginShutdown() {
	a.shutdown.begin()
	if a.DelegatedManager != nil {
		a.DelegatedManager.refuseNewBackends()
	}
	a.logger().Infof("shutdown: draining — no new system turns, compactions or backends")
}

// ShuttingDown reports whether BeginShutdown has been called.
func (a *Agent) ShuttingDown() bool { return a.shutdown.active() }

// refuseSystemTurn returns ErrShuttingDown when a turn with this trigger must
// not begin because the agent is draining. Interactive (real-time user) input
// is not refused here: a message the platform has already accepted has no
// durable record to fall back on, so dropping it would lose it outright.
func (a *Agent) refuseSystemTurn(trigger string) error {
	if a.ShuttingDown() && !isInteractiveTrigger(trigger) {
		return ErrShuttingDown
	}
	return nil
}

// markTurnDispatched stamps the moment a registered turn actually began on
// its backend, as distinct from when it was registered (it may have waited
// behind an in-flight turn first). Shutdown diagnostics report elapsed time
// from here, so a turn that queued for minutes and ran for seconds reads as
// what it is. Nil-safe for turns that were never registered.
func (a *Agent) markTurnDispatched(td *TurnDetail) {
	if td == nil {
		return
	}
	a.turnDetailsMu.Lock()
	if td.DispatchedAt.IsZero() {
		td.DispatchedAt = time.Now()
	}
	a.turnDetailsMu.Unlock()
}

// cancelOnClose calls cancel if ch closes before the returned stop is called,
// so a bounded wait also ends the moment shutdown begins.
func cancelOnClose(ch <-chan struct{}, cancel func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			cancel()
		case <-done:
		}
	}()
	return func() { close(done) }
}
