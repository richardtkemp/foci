package agent

import (
	"context"

	"foci/internal/turnevent"
)

// A background spawn's branch has no chat of its own. Its spawner runs the
// spawn's turns with its own sink and reads their text for the [SPAWN
// RESULT]. Any other turn on the branch that would deliver to a platform (an
// injection: an async tool result, a scheduled wake, a send_to_session) went
// to the human's most recent conversation as an "unsolicited send to unbound
// session" (#3143 D2). Such a turn now reports to the spawn's spawner, as a
// run the spawn's backend starts by itself does (OpenAutonomousTurn, #2729):
// as one more [SPAWN RESULT] when the spawn has ended, or by taking the spawn
// over when the spawn's own run ends while it goes on.

// spawnBranchTurnCtx returns ctx with its sink replaced by the spawn's
// (SpawnBranchTurn), when sessionKey is a background spawn's branch and ctx's
// sink delivers to a platform. release ends what it set up; call it when the
// turn has ended. The spawn's stop ends the turn: its ctx is cancelled (an
// API turn) and its backend turn interrupted (a delegated one).
func (a *Agent) spawnBranchTurnCtx(ctx context.Context, sessionKey string) (_ context.Context, release func()) {
	if a.SpawnBranchTurn == nil || !turnevent.SinkFromContext(ctx).DeliversToPlatform() {
		return ctx, func() {}
	}
	turnCtx, cancel := context.WithCancel(ctx)
	sink, ok := a.SpawnBranchTurn(sessionKey, func() {
		cancel()
		// Not on the caller's goroutine: a stop runs under the spawn
		// tracker's lock, and the interrupt's events reach the spawn's sink.
		go func() {
			if err := a.InterruptTurn(sessionKey); err != nil {
				a.logger().Infof("turn sk=%s: interrupt for a spawn stop: %v", sessionKey, err)
			}
		}()
	})
	if !ok || sink == nil {
		cancel()
		return ctx, func() {}
	}
	a.logger().Infof("turn sk=%s (trigger=%s) on a background spawn's branch: reported to its spawner, not delivered", sessionKey, TriggerFromContext(ctx))
	return turnevent.WithSink(turnCtx, sink), cancel
}
