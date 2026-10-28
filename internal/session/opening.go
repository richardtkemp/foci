package session

import (
	"context"

	"foci/internal/provider"
)

// BranchOpening is what a forked branch ends on before its first turn (#2617):
// a depth leaf's fork keeps the parent's in-flight fan-out call, answers it
// with ToolResult, and has AgentText as the agent's own next message, so the
// leaf's first turn is the user's reply to it. Where the fork cannot be given
// that end, Fallback is added to the branch orientation instead, which rides on
// the first user message.
type BranchOpening struct {
	// CallMarker picks the parent's in-flight tool call to answer (a call whose
	// tool name or input contains it).
	CallMarker string
	ToolResult string
	AgentText  string
	// Fallback is appended to the orientation when the opening could not be
	// written into the fork.
	Fallback string
}

type unsavedTurnKey struct{}

// WithUnsavedTurn records on ctx the messages of the running API turn that are
// not on disk yet (the API transport saves a turn when it ends), so a fork
// taken from inside one of its tool calls can carry them (#2617). The slice is
// copied.
func WithUnsavedTurn(ctx context.Context, msgs []provider.Message) context.Context {
	return context.WithValue(ctx, unsavedTurnKey{}, append([]provider.Message(nil), msgs...))
}

// UnsavedTurn returns the messages WithUnsavedTurn recorded, or nil.
func UnsavedTurn(ctx context.Context) []provider.Message {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(unsavedTurnKey{}).([]provider.Message)
	return m
}
