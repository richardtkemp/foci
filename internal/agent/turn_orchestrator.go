package agent

import (
	"context"
	"time"
)

// RunTurn executes a complete turn through the TurnContract pipeline.
// It calls all 20 concern methods in the canonical order, handling errors
// and cleanup at each step. The ctx is stored on ts.Ctx for use by
// contract methods that need it.
func (a *Agent) RunTurn(ctx context.Context, tc TurnContract, ts *TurnState) (string, error) {
	ts.Ctx = ctx

	// Derive context fields once — avoids repeated extraction in each method.
	ts.Meta = TurnMetadataFromContext(ctx)
	if ts.Meta == nil {
		ts.Meta = &TurnMetadata{}
	}
	ts.Trigger = TriggerFromContext(ctx)
	ts.StartedAt = time.Now()

	// Safety-net: if we exit without post-turn saving (error, panic),
	// flush any accumulated messages via the transport's SaveSession.
	// On success, post-turn's SaveSession nils NewMessages first.
	defer func() {
		if len(ts.NewMessages) > 0 {
			_ = tc.SaveSession(ts)
		}
	}()

	// Phase 1: Pre-lock
	if err := tc.RateLimitGate(ts); err != nil {
		return "", err
	}
	unlock := tc.AcquireTurnLock(ts)
	defer unlock()
	dec := tc.IncrementProcessing(ts)
	defer dec()
	unreg := tc.RegisterTurn(ts)
	defer unreg()
	if err := tc.CheckStaleContext(ts); err != nil {
		return "", err
	}

	// Phase 1b: Logging & tracking
	tc.RegisterSessionIndex(ts)
	tc.LogConversationRecv(ts)
	tc.TouchActivity(ts)

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
	if err := tc.ExecuteTurn(ts); err != nil {
		return "", err
	}

	// Phase 4: Post-turn
	a.runPostTurn(tc, ts)
	return ts.FinalText, nil
}

// runPostTurn handles the sync/async split for post-turn concerns.
//
// API path: CompletionChan is already closed when ExecuteTurn returns,
// so the select falls through to the inline post() call.
//
// Backend path: CompletionChan is still open (turn completes asynchronously).
// A goroutine waits for completion with a 10-minute safety timeout.
func (a *Agent) runPostTurn(tc TurnContract, ts *TurnState) {
	post := func() {
		if err := tc.SaveSession(ts); err != nil {
			a.logger().Errorf("session=%s post-turn save: %v", ts.SessionKey, err)
		}
		tc.UpdateSessionMeta(ts)
		tc.RunCompaction(ts)
		tc.LogConversationSent(ts)
		tc.TouchActivityPost(ts)
	}
	select {
	case <-ts.CompletionChan:
		post() // API: already done
	default:
		go func() { // Backend: wait for completion
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			select {
			case <-ts.CompletionChan:
				post()
			case <-ctx.Done():
				a.logger().Warnf("session=%s post-turn timeout", ts.SessionKey)
			}
		}()
	}
}
