package agent

import (
	"context"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/telemetry"
	"foci/internal/turnevent"
)

// OrchestrateFullTurn executes a complete turn through the TurnContract pipeline.
// It calls all 20 concern methods in the canonical order, handling errors
// and cleanup at each step. The ctx is stored on ts.Ctx for use by
// contract methods that need it. FinalText, Usage, Cost, and Model accumulate
// on ts and are surfaced to the caller's sink via the TurnComplete event
// emitted by HandleMessage.
func (a *Agent) OrchestrateFullTurn(ctx context.Context, tc TurnContract, ts *TurnState) (string, error) {
	ts.Ctx = ctx

	// Derive context fields once — avoids repeated extraction in each method.
	ts.Meta = TurnMetadataFromContext(ctx)
	if ts.Meta == nil {
		ts.Meta = &TurnMetadata{}
	}
	ts.Trigger = TriggerFromContext(ctx)
	ts.Purpose = batchPurposeFromContext(ctx)
	ts.StartedAt = time.Now()
	ts.ReceivedAt = ReceivedAtFromContext(ctx)
	if ts.ReceivedAt.IsZero() {
		ts.ReceivedAt = ts.StartedAt
	}
	// The trace's root span opens the instant the turn has an identity
	// (RowID needs StartedAt), before any gate can fail — so a rate-limited
	// or stale turn is still a (short, errored) trace.
	a.traceBegin(ctx, ts)

	// Phase 1: Pre-lock
	if err := tc.RateLimitGate(ts); err != nil {
		return "", err
	}
	unlock := tc.AcquireTurnLock(ts)
	defer unlock()
	// markInFlight is the sole in-flight tracker; it covers both API and
	// delegated transports. Session keys are stable identities: a facet/branch
	// (a 'b' child on its own backend, an independent conversation) has its own
	// key and tracks separately rather than wrongly coupling to the parent root
	// (TODO #719). Root-injected periodic turns (reflection/keepalive/memory)
	// run under the parent key, so they register under the root identity — the
	// granularity the activity gate cares about. Released when
	// OrchestrateFullTurn returns, which for delegated turns is after
	// runPostTurn unblocks on CompletionChan. A permission-blocked CC turn
	// keeps inFlight=1 until the user decides; that's exactly the gate
	// signal we want (TODO #753).
	//
	// The delivering bit is sourced from the sink on ctx — if the sink
	// routes to a user-facing platform (Telegram, Discord), the in-flight
	// turn is delivering and Telegram follow-ups can safely fold into it
	// via the existing inject path. If the sink is NopSink/BufferSink (e.g.
	// reflection's no-sink ctx, see TODO #767), the in-flight turn is
	// non-delivering and the inbox worker waits for it to clear before
	// dispatching a new envelope, avoiding the discarded-response bug.
	delivering := turnevent.SinkFromContext(ctx).DeliversToPlatform()
	doneInFlight := a.markInFlight(ts.SessionKey, delivering)
	defer doneInFlight()
	unreg := tc.RegisterTurn(ts)
	defer unreg()
	if err := tc.CheckStaleContext(ts); err != nil {
		return "", err
	}

	// Phase 1b: Logging & tracking
	tc.LogConversationRecv(ts)
	tc.TouchActivity(ts)
	// recordTurnActivity is the SINGLE per-turn timestamp write: one upsert that
	// captures the prior request time (for cache-bust) then sets last_cache_touch
	// (every turn — any trigger refreshes the cached prefix), last_activity_at
	// (skipped for memory-formation turns, so reflection isn't defeated), and
	// last_user_activity_at (only real-time interactive input — telegram/app/
	// discord/voice, NOT /send/cron/webhook/agent/memory). Replaces the former
	// separate RegisterSessionIndex + recordCacheTouch + touchUserActivity writes.
	a.recordTurnActivity(ts)

	// Phase 2: Preparation
	tc.LoadSessionMeta(ts)
	if err := tc.LoadAndRepairSession(ts); err != nil {
		return "", err
	}
	tc.ResolveModelEffort(ts)
	if err := tc.ComposePrompt(ts); err != nil {
		return "", err
	}
	// lastMessageTime feeds the [meta] gap= display only (cache-bust reads
	// prevRequestTime instead). Update it HERE — after ComposePrompt read the
	// previous value for this turn's gap, so the NEXT turn measures from this
	// message. Single central write covering both transports, replacing the
	// former per-transport sites (turn_delegated / turn_api).
	if ts.SessionMeta != nil {
		ts.SessionMeta.lastMessageTime = ts.UserMessageTime()
	}
	tc.BuildSystemAndTools(ts)
	tc.InjectNudges(ts)
	a.traceInput(ctx, ts)

	// Phase 3: Execution
	if err := tc.RunInference(ts); err != nil {
		// Flush accumulated messages — post-turn won't run.
		if len(ts.NewMessages) > 0 {
			_ = tc.SaveSession(ts)
		}
		return "", err
	}

	// Phase 3.5: register this turn's delivery sink on the session router, for
	// system turns only. A platform turn already registered its streaming sink in
	// RunTurn (its ctx sink IS the router); a system turn (reflection/keepalive/
	// memory) reaches here with a NopSink/BufferSink ctx sink and must register it
	// so its output is suppressed/captured rather than falling through to the
	// router's late-delivery fallback (which would leak reflection text to chat).
	// Registering HERE — after RunInference's exclusive begin was accepted, not
	// before dispatch — is what stops a mid-wait system turn from clobbering a
	// concurrent autonomous run's delivery (the #1068 poison). Cleared after
	// post-turn's completion wait so a later autonomous run falls through to
	// late-delivery.
	//
	// "Is the router" must see through decorators: HandleMessage wraps the ctx
	// sink for tracing, so on a platform turn the ctx sink is a turnSink whose
	// inner is the router. A plain identity compare registered that wrapper
	// INTO the router and the first event recursed to a stack overflow (#1944).
	if router := a.sessionRouter(ts.SessionKey); !router.routesTo(turnevent.SinkFromContext(ctx)) {
		router.Register(turnevent.SinkFromContext(ctx))
		defer router.Clear()
	}

	// Phase 4: Post-turn (sync for API, async for delegated)
	a.runPostTurn(tc, ts)
	return ts.FinalText, nil
}

// streamIdleTimeout is how long runPostTurn tolerates silence on the CC
// stream before declaring the backend hung. This is a last-resort safety net
// for orphaned goroutines — not a liveness check. Normal backend death is
// detected by process exit / stream EOF, not by this timeout.
//
// Set high (24h) because legitimate silence happens during permission prompts
// (CC emits nothing while waiting for user approval in pipe mode — keep_alive
// frames are WebSocket-only). A short timeout here causes false warnings on
// every permission wait longer than the threshold.
const streamIdleTimeout = 24 * time.Hour

// fixedPostTurnTimeout is the hard safety ceiling for backends that don't
// implement ActivityChecker (e.g. cctmux, or API turns where CompletionChan
// is already closed).
const fixedPostTurnTimeout = 24 * time.Hour

// runPostTurn waits for the turn to complete, then runs post-turn concerns.
//
// API path: CompletionChan is already closed when RunInference returns —
// the select completes immediately.
//
// Delegated path: blocks until CC signals turn completion via OnTurnComplete
// (which closes CompletionChan). Uses activity-based timeout: if the backend
// implements ActivityChecker, the timeout resets on every stream event. A
// genuinely long turn (CC actively processing) keeps emitting events and
// is never killed. A hung backend (no events) times out after streamIdleTimeout.
//
// Steered follow-up (delegated): CompletionChan is already closed by
// RunInference (no callback registered). Falls through immediately and
// post-turn no-ops (FinalUsage is nil).
func (a *Agent) runPostTurn(tc TurnContract, ts *TurnState) {
	a.logger().Debugf("runPostTurn: enter sk=%s", ts.SessionKey)
	// Check if the backend supports activity tracking.
	var ac delegator.ActivityChecker
	if ts.Backend != nil {
		ac, _ = ts.Backend.(delegator.ActivityChecker)
	}

	if ac != nil {
		// Activity-based wait: poll LastActivity and only timeout when the
		// stream goes silent for streamIdleTimeout.
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ts.CompletionChan:
				a.logger().Debugf("runPostTurn: CompletionChan closed sk=%s", ts.SessionKey)
				goto done
			case <-ticker.C:
				last := ac.LastActivity()
				if !last.IsZero() && time.Since(last) > streamIdleTimeout {
					a.logger().Warnf("session=%s post-turn timeout: no stream activity for %s",
						ts.SessionKey, time.Since(last).Round(time.Second))
					return
				}
			}
		}
	} else {
		// Fixed timeout fallback for backends without activity tracking.
		ctx, cancel := context.WithTimeout(context.Background(), fixedPostTurnTimeout)
		defer cancel()
		select {
		case <-ts.CompletionChan:
			a.logger().Debugf("runPostTurn: CompletionChan closed (no-activity-checker) sk=%s", ts.SessionKey)
		case <-ctx.Done():
			a.logger().Warnf("session=%s post-turn timeout waiting for completion", ts.SessionKey)
			return
		}
	}

done:

	// OnTurnDone is subsumed by the sink's TurnComplete handler (which is
	// emitted from HandleMessage's defer). No additional platform notification
	// is needed here.
	if err := tc.SaveSession(ts); err != nil {
		a.logger().Errorf("session=%s post-turn save: %v", ts.SessionKey, err)
	}
	tc.UpdateSessionMeta(ts)
	a.logger().Debugf("runPostTurn: entering RunCompaction sk=%s", ts.SessionKey)
	tc.RunCompaction(ts)
	a.logger().Debugf("runPostTurn: RunCompaction returned sk=%s", ts.SessionKey)
	tc.LogConversationSent(ts)
	tc.TouchActivityPost(ts)
	a.logger().Debugf("runPostTurn: exit sk=%s", ts.SessionKey)
}

// traceBegin opens the turn's root span with everything known at this point.
// Nil-safe: with tracing off, TurnFromContext is nil and every Turn method
// is a no-op.
func (a *Agent) traceBegin(ctx context.Context, ts *TurnState) {
	t := telemetry.TurnFromContext(ctx)
	if t == nil {
		return
	}
	backend := "api"
	if a.DelegatedManager != nil {
		backend = "delegated"
		if a.DelegatedManager.BackendType != "" {
			backend = a.DelegatedManager.BackendType
		}
	}
	info := telemetry.TurnInfo{
		TurnID:     ts.RowID(),
		SessionKey: ts.SessionKey,
		AgentID:    a.AgentID,
		Trigger:    ts.Trigger,
		Via:        triggerToPlatform(ts.Trigger),
		Backend:    backend,
		StartedAt:  ts.StartedAt,
		Purpose:    ts.Purpose,
	}
	if ts.ReceivedAt != ts.StartedAt {
		info.ReceivedAt = ts.ReceivedAt
	}
	if ts.Meta != nil {
		info.ChatID, info.UserID, info.Username = ts.Meta.ChatID, ts.Meta.UserID, ts.Meta.Username
	}
	t.Begin(info)
}

// traceInput attaches the prompt as sent (delegated: the flat composed
// prompt; API: the user message text) and the resolved model, and registers
// the API path's system blocks — the delegated path registers its prompt at
// backend launch (DelegatedManager.Get), where it is actually sent.
func (a *Agent) traceInput(ctx context.Context, ts *TurnState) {
	t := telemetry.TurnFromContext(ctx)
	if t == nil {
		return
	}
	prompt := ts.Prompt
	if prompt == "" {
		prompt = provider.TextOf(ts.UserMsg.Content)
	}
	t.SetInput(prompt, ts.TurnModel)
	if len(ts.System) > 0 {
		texts := make([]string, 0, len(ts.System))
		for _, b := range ts.System {
			texts = append(texts, b.Text)
		}
		telemetry.RegisterSystemPrompt(ts.SessionKey, strings.Join(texts, "\n\n"), "api")
	}
}
