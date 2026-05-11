package agent

import (
	"context"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/delegator"
	"foci/internal/session"
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
	ts.StartedAt = time.Now()
	ts.ReceivedAt = ReceivedAtFromContext(ctx)
	if ts.ReceivedAt.IsZero() {
		ts.ReceivedAt = ts.StartedAt
	}

	// Phase 1: Pre-lock
	if err := tc.RateLimitGate(ts); err != nil {
		return "", err
	}
	unlock := tc.AcquireTurnLock(ts)
	defer unlock()
	dec := tc.IncrementProcessing(ts)
	defer dec()
	// markInFlight covers both API and delegated transports — sibling to
	// IncrementProcessing (which is API-only). Keyed by SessionKeyBase so the
	// gate can ask "is *this* session in-flight?" — branches and sub-agents
	// have distinct bases (sub-agents) or share the parent's base (branches),
	// matching the granularity the activity gate cares about. Released when
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
	sessionBase := session.SessionKeyBase(ts.SessionKey)
	delivering := turnevent.SinkFromContext(ctx).DeliversToPlatform()
	doneInFlight := a.markInFlight(sessionBase, delivering)
	defer doneInFlight()
	unreg := tc.RegisterTurn(ts)
	defer unreg()
	if err := tc.CheckStaleContext(ts); err != nil {
		return "", err
	}

	// Phase 1b: Logging & tracking
	tc.RegisterSessionIndex(ts)
	tc.LogConversationRecv(ts)
	tc.TouchActivity(ts)
	// touchLastActivity records that *this session* is doing something *now*,
	// regardless of trigger (user, cron, CLI, webhook, agent-to-agent,
	// system-injected). Single chokepoint covers every turn-init path. Keyed
	// by SessionKeyBase so the gate consults the specific session a CLI send
	// would target (the agent's main session) rather than being confused by
	// activity in unrelated branches or sub-agents. The per-receive-path
	// last_user_activity write (telegram/discord) is deliberately separate —
	// it tracks user attention, not agent activity, and lives in
	// agent_metadata rather than session_metadata.
	a.touchLastActivity(sessionBase)

	// Phase 2: Preparation
	tc.LoadSessionMeta(ts)
	if err := tc.LoadAndRepairSession(ts); err != nil {
		return "", err
	}
	tc.ResolveModelEffort(ts)
	if err := tc.ComposePrompt(ts); err != nil {
		return "", err
	}
	tc.BuildSystemAndTools(ts)
	tc.InjectNudges(ts)

	// Phase 3: Execution
	if err := tc.RunInference(ts); err != nil {
		// Flush accumulated messages — post-turn won't run.
		if len(ts.NewMessages) > 0 {
			_ = tc.SaveSession(ts)
		}
		return "", err
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
