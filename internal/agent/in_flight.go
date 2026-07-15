package agent

import (
	"context"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/turn"
)

// IsTurnInFlight returns true if any turn is currently executing under
// OrchestrateFullTurn for the in-flight identity of the given session key —
// covers both API and delegated transports. Distinct from IsProcessing, which
// only reflects the API path's internal counter and is per-agent rather than
// per-session.
//
// Session keys are stable identities: a root session and its post-compaction
// content share one key, while a facet/branch (a 'b' child on its own backend)
// has its own key and tracks separately — coupling a facet to the parent would
// wrongly couple two independent conversations (TODO #719). Root-injected
// periodic turns (reflection/keepalive/memory) run under the parent key, so
// they register under the root identity as the #760/#767 gates expect.
//
// This is the runtime signal used by the activity gate to short-circuit
// keepalive sends while a turn is mid-flight (e.g. blocked waiting for a
// permission decision in the delegated path).
func (a *Agent) IsTurnInFlight(key string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.inFlight[key] > 0
}

// IsAnyTurnInFlight reports whether any session under this agent currently has
// a turn executing. This is the one place that genuinely needs an agent-wide
// aggregate rather than a per-session check: graceful shutdown drains all
// in-flight work before exit. Everything else should ask IsTurnInFlight(base).
func (a *Agent) IsAnyTurnInFlight() bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	for _, n := range a.inFlight {
		if n > 0 {
			return true
		}
	}
	return false
}

// SetTurnInFlightForTest marks a session's base as in-flight (inFlight=true) or
// clears it, so tests can exercise the in-flight guards without driving a real
// turn. Test-only.
func (a *Agent) SetTurnInFlightForTest(sessionKey string, inFlight bool) {
	base := sessionKey
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]int32)
	}
	if inFlight {
		a.inFlight[base] = 1
	} else {
		delete(a.inFlight, base)
	}
	a.notifyInFlightChangedLocked(base)
}

// IsInFlightDelivering returns true if at least one in-flight turn under base
// has a sink that reports DeliversToPlatform=true (i.e. routes output to a
// user-facing platform). Kept distinct from IsTurnInFlight so that combining
// logic — "in flight AND NOT delivering = block before dispatch" — is visible
// at the call site rather than hidden in a single overloaded predicate.
//
// Counts mirror inFlight: a turn is delivering iff its sink reports true at
// markInFlight time; the bookkeeping is exact across nested/concurrent turns
// on the same base.
func (a *Agent) IsInFlightDelivering(key string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.inFlightDelivering[key] > 0
}

// InFlightWaitCh returns a channel that closes the next time the in-flight
// state for base changes (any markInFlight increment or decrement closure
// for that base). Callers re-check state after the channel closes — the
// channel signals "something changed," not "your wait condition is now met."
//
// The channel is created lazily on first call per base and replaced with a
// fresh open channel after each close, so a single waiter can loop:
//
//	for a.IsTurnInFlight(base) && !a.IsInFlightDelivering(base) {
//	    wait := a.InFlightWaitCh(base)
//	    select {
//	    case <-ctx.Done(): return
//	    case <-wait:       // state changed, re-check
//	    }
//	}
//
// Safe under concurrent waiters and concurrent state changes: close-and-replace
// happens under inFlightMu, so every waiter that fetched the channel before
// the change observes the close.
func (a *Agent) InFlightWaitCh(key string) <-chan struct{} {
	base := key
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlightChanged == nil {
		a.inFlightChanged = make(map[string]chan struct{})
	}
	ch, ok := a.inFlightChanged[base]
	if !ok {
		ch = make(chan struct{})
		a.inFlightChanged[base] = ch
	}
	return ch
}

// notifyInFlightChangedLocked closes base's change-channel and installs a
// fresh open channel for future waiters. Must be called with inFlightMu held.
// Safe to call when no channel has been created yet (no-op).
func (a *Agent) notifyInFlightChangedLocked(base string) {
	if a.inFlightChanged == nil {
		return
	}
	ch, ok := a.inFlightChanged[base]
	if !ok {
		return
	}
	close(ch)
	a.inFlightChanged[base] = make(chan struct{})
}

// markInFlight increments the in-flight counter for the given session base
// and returns a one-shot decrement closure. The closure is safe to call
// exactly once; subsequent calls are no-ops via the local guard.
//
// delivering reports whether the turn's sink ultimately routes to a
// user-facing platform (see turnevent.Sink.DeliversToPlatform). A separate
// counter tracks delivering turns so callers can distinguish "any turn in
// flight" from "an in-flight turn whose output reaches the user."
//
// Usage at call sites:
//
//	delivering := turnevent.SinkFromContext(ctx).DeliversToPlatform()
//	done := a.markInFlight(ts.SessionKey, delivering)
//	defer done()
//
// The orchestrator pairs this with touchCacheFreshness at turn entry so that
// both signals — runtime "doing something now" and persistent "cache warmed
// at time T" — track every turn-init path through the single chokepoint.
//
// Both increment and decrement notify InFlightWaitCh listeners by
// close-and-replace, so the inbox's wait loop wakes on any state change and
// re-evaluates its predicate.
func (a *Agent) markInFlight(key string, delivering bool) func() {
	base := key
	a.inFlightMu.Lock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]int32)
	}
	if a.inFlightDelivering == nil {
		a.inFlightDelivering = make(map[string]int32)
	}
	a.inFlight[base]++
	if delivering {
		a.inFlightDelivering[base]++
	}
	a.notifyInFlightChangedLocked(base)
	a.inFlightMu.Unlock()

	var done bool
	return func() {
		if done {
			return
		}
		done = true
		a.inFlightMu.Lock()
		defer a.inFlightMu.Unlock()
		if a.inFlight == nil {
			return
		}
		a.inFlight[base]--
		if a.inFlight[base] <= 0 {
			delete(a.inFlight, base)
		}
		if delivering {
			a.inFlightDelivering[base]--
			if a.inFlightDelivering[base] <= 0 {
				delete(a.inFlightDelivering, base)
			}
		}
		a.notifyInFlightChangedLocked(base)
	}
}

// openAutonomousTurn adopts a CC-initiated run — one foci did not open with a
// send (a background-agent completion, task-notification, or back-to-back
// continuation) — as a FIRST-CLASS foci turn (#1261). Wired to the delegated
// backend's onAutonomousOpen and fired from the running-edge callback on the
// backend's reader goroutine, so the streaming sink is registered before the
// run's first delta is read (no lost events). It builds the platform per-turn
// sink, a minimal TurnState, and the turn's bookkeeping (buildTurnEvents),
// begins the turn via AdoptRunningTurn (no send), marks it in flight, and spawns
// the owning goroutine that awaits completion then runs the post-turn accounting
// and emits the meta frame — the same lifecycle a foci-initiated turn gets.
//
// Making the run turnActive is what gives first-classness for free: fold-in of a
// user message (IsTurnInFlight → true), the system-inject hold, the pre-answer
// nudge gate, api.db usage rows, and the meta frame all key on the normal turn
// path rather than the old whole-message late-delivery fallback.
func (a *Agent) OpenAutonomousTurn(sessionKey string, be delegator.Delegator) {
	adopter, ok := be.(interface {
		AdoptRunningTurn(*delegator.TurnEvents) bool
	})
	if !ok {
		return // backend cannot adopt a running turn (opencode, tests)
	}

	var conn platform.Connection
	if a.ResolveLateConn != nil {
		conn = a.ResolveLateConn(sessionKey)
	}
	sink, cleanup := a.autonomousTurnSink(conn, sessionKey)
	a.logger().Debugf("session=%s OpenAutonomousTurn: conn_nil=%v sink_type=%T (diagnostic instrumentation, #1274)", sessionKey, conn == nil, sink)

	// Wrap the sink so intermediate TextBlock events are logged to the
	// conversation DB (parity with foci-initiated turns, which wrap their
	// per-turn sink in newLoggingSink). Autonomous runs have no incoming
	// message, so the chat ID is resolved from the session key rather than
	// TurnMetadata. Without this the reply streams/delivers but is never
	// persisted to conversation.db (#1261 follow-up).
	autoMeta := &TurnMetadata{}
	chatID := session.ChatIDFromKey(sessionKey)
	sink = newLoggingSink(sink, a, chatID, autoMeta, sessionKey)

	router := a.sessionRouter(sessionKey)
	router.Register(sink) // synchronous — registered before the run's first delta
	a.logger().Debugf("session=%s OpenAutonomousTurn: router.Register (diagnostic instrumentation, #1274)", sessionKey)

	t := &DelegatedTransport{sharedTurnOps{agent: a}}
	ctx := turnevent.WithSink(WithTrigger(context.Background(), "autonomous"), sink)
	ts := NewTurnState(ctx, sessionKey, nil, nil)
	ts.StartedAt = time.Now()
	ts.Meta = autoMeta
	ts.ConvChatID = chatID // conv-DB logging (thinking + wrapped sink) needs it
	ts.Trigger = "autonomous"
	ts.Backend = be
	t.LoadSessionMeta(ts)
	ts.sessionFilePath = be.SessionFilePath()
	turnEvents := t.buildTurnEvents(ts, be)

	if !adopter.AdoptRunningTurn(turnEvents) {
		// A foci-initiated turn raced this open and now owns the run — unwind.
		a.logger().Debugf("session=%s OpenAutonomousTurn: AdoptRunningTurn declined (turnActive already true) — clearing router.Register from above, abandoning adoption (diagnostic instrumentation, #1274)", sessionKey)
		router.Clear()
		if cleanup != nil {
			cleanup()
		}
		return
	}
	a.logger().Debugf("session=%s OpenAutonomousTurn: AdoptRunningTurn accepted — sink stays registered until turn completion (diagnostic instrumentation, #1274)", sessionKey)

	sink.Emit(ctx, turnevent.TurnStart{})
	markDone := a.markInFlight(sessionKey, sink.DeliversToPlatform())

	// Owning goroutine: OnTurnComplete (built into turnEvents) closes
	// CompletionChan at idle / process exit. Post-turn accounting runs OFF the
	// reader goroutine; the TurnComplete event emits the meta frame.
	go func() {
		<-ts.CompletionChan
		t.UpdateSessionMeta(ts)
		t.RunCompaction(ts)
		sink.Emit(ctx, turnevent.TurnComplete{
			FinalText: ts.FinalText,
			Usage:     ts.DisplayUsage(),
			Cost:      ts.FinalCost,
			Model:     ts.FinalModel,
		})
		if cleanup != nil {
			cleanup()
		}
		a.logger().Debugf("session=%s OpenAutonomousTurn: completion — router.Clear (diagnostic instrumentation, #1274)", sessionKey)
		router.Clear()
		markDone()
	}()
}

// autonomousTurnSink builds the per-turn delivery sink for an adopted autonomous
// turn: the app's streaming appSink (text.delta + activity) when the connection
// is a streaming Driver, a whole-message SessionSink for Telegram/Discord, or a
// NopSink when no connection is bound (the turn still runs and accounts).
func (a *Agent) autonomousTurnSink(conn platform.Connection, sessionKey string) (turnevent.Sink, func()) {
	if driver, ok := conn.(Driver); ok {
		if sink, cleanup := driver.NewTurnSink(Envelope{SessionKey: sessionKey}); sink != nil {
			return sink, cleanup
		}
	}
	if conn != nil {
		return turn.NewSessionSink(conn, sessionKey, "autonomous"), nil
	}
	return turnevent.NopSink{}, nil
}

// recordTurnActivity performs a turn's ENTIRE per-turn timestamp write. It first
// captures the PREVIOUS request time (last_cache_touch as of turn entry) onto
// the session meta for cache-bust idle detection — that capture MUST read the
// old value before the write overwrites it — then issues the single
// RecordTurnActivity upsert that sets, atomically: last_cache_touch (always),
// last_activity_at (unless a memory-formation turn), and last_user_activity_at
// (only interactive human turns). Replaces the former separate RegisterSessionIndex
// + touchCacheFreshness + touchUserActivity writes. Must run at turn entry,
// before inference. Own key only — root warmth is handled at branch creation by
// TouchRootCacheForBranch.
func (a *Agent) recordTurnActivity(ts *TurnState) {
	if a.SessionIndex == nil || ts.SessionKey == "" {
		return
	}
	// Capture prior request time before the bump (cache-bust reads it mid-inference).
	prev, ok := a.SessionIndex.LastCacheTouch(ts.SessionKey)
	sm := a.getSessionMeta(ts.SessionKey)
	a.metaMu.Lock()
	if ok {
		sm.prevRequestTime = prev
	} else {
		sm.prevRequestTime = time.Time{}
	}
	a.metaMu.Unlock()

	filePath := ""
	if a.DelegatedManager != nil {
		filePath = a.DelegatedManager.SessionFilePath(ts.SessionKey)
	}
	a.SessionIndex.RecordTurnActivity(session.SessionIndexEntry{
		SessionKey:  ts.SessionKey,
		FilePath:    filePath,
		CreatedAt:   time.Now(),
		SessionType: session.ClassifySessionKey(ts.SessionKey),
		Status:      session.SessionStatusActive,
	}, !isMemoryTrigger(ts.Trigger), isInteractiveTrigger(ts.Trigger))
	a.emitCacheExpiry(ts.SessionKey) // last_cache_touch just advanced → refresh client
}

// TouchRootCacheForBranch records that creating a branch warmed its root's
// shared cached prefix — a ONE-TIME touch at the moment of branching, called
// from the branch-creation sites (createMemoryBranch, buildBranchFunc, the facet
// command). Branch turns themselves no longer bump the root (recordTurnActivity
// is own-key only). No-op for a root key, a nil index, or an empty key.
func (a *Agent) TouchRootCacheForBranch(branchKey string) {
	if a.SessionIndex == nil || branchKey == "" {
		return
	}
	if sk, err := session.ParseSessionKey(branchKey); err == nil {
		if root := sk.Root().String(); root != branchKey {
			a.SessionIndex.TouchCacheTouch(root, time.Now())
			a.emitCacheExpiry(root) // branch warmed the root's shared prefix → refresh client
		}
	}
}

