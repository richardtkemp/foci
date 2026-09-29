package accounting

import "context"

type turnKey struct{}

// WithTurn attaches the conversation turn whose work ctx carries, so a helper
// call that turn makes — a summary or spawn from one of its tools, the
// compaction run after it — is booked on that turn rather than a turn of its
// own (Dick, 2026-09-29: "subagent turns should be tied to their parent").
func WithTurn(ctx context.Context, t Turn) context.Context {
	return context.WithValue(ctx, turnKey{}, t)
}

// InvokingTurn is the turn attached to ctx, if any.
func InvokingTurn(ctx context.Context) (Turn, bool) {
	t, ok := ctx.Value(turnKey{}).(Turn)
	return t, ok && t.TurnID != ""
}

// TurnFor is the turn a helper call made under ctx is booked on: the invoking
// conversation turn when there is one, else own — a turn of the call's own for
// work no turn invoked (an operator's /compact).
func TurnFor(ctx context.Context, own Turn) Turn {
	if t, ok := InvokingTurn(ctx); ok {
		return t
	}
	return own
}
