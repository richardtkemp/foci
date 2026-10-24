package agent

import (
	"context"

	"foci/internal/delegator"
)

// SteerTurn folds text into the turn in flight on sk's delegated backend
// (ImmediateInject(SourceSteer), as a typed mid-turn message folds), and
// reports whether it did. It implements tools.SpawnSteerer for spawn_send
// (#2523): a delegated spawn runs on its own branch session, outside the
// inbox, so the inbox's steer path never sees it.
//
// False — and the caller queues the text for the spawn's next turn — when the
// agent is not delegated (an API spawn's turn drains queued text through its
// turnevent.Steerer at each tool boundary), when no turn is in flight on sk
// (its backend is still starting, or the turn just ended), or when the inject
// fails. Never starts a backend: the in-flight check is non-creating.
func (a *Agent) SteerTurn(ctx context.Context, sk, text string) bool {
	if a.DelegatedManager == nil || !a.DelegatedManager.BackendTurnInFlight(sk) {
		return false
	}
	be, err := a.resolveSessionBackend(ctx, sk)
	if err != nil || be == nil {
		return false
	}
	if err := be.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceSteer, Text: text}); err != nil {
		a.logger().Infof("spawn steer sk=%s not folded: %v (queued for its next turn instead)", sk, err)
		return false
	}
	return true
}
