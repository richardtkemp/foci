package tools

import "context"

// sessionKeyCtxKey is the context key for the originating session key.
type sessionKeyCtxKey struct{}

// WithSessionKey attaches the current session key to a context.
// Called by the agent loop so tools can route async results correctly.
func WithSessionKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, sessionKeyCtxKey{}, key)
}

// SessionKeyFromContext extracts the session key from context (empty if absent).
func SessionKeyFromContext(ctx context.Context) string {
	s, _ := ctx.Value(sessionKeyCtxKey{}).(string)
	return s
}

// OutputHints describe where a shell-function caller's output is going (#2048).
// The generated foci_* wrappers detect whether their stdout is piped and pass
// that (plus any explicit --format) through foci-call; the exec bridge attaches
// it to the tool's context. Each tool decides whether to act on it — most
// ignore it. The API tool path never sets it, so its output is unaffected.
type OutputHints struct {
	StdoutPiped bool   `json:"stdout_piped,omitempty"`
	Format      string `json:"format,omitempty"` // explicit override; "" = decide from StdoutPiped
}

type outputHintsKey struct{}

// WithOutputHints attaches exec-bridge output hints to a context.
func WithOutputHints(ctx context.Context, h OutputHints) context.Context {
	return context.WithValue(ctx, outputHintsKey{}, h)
}

// OutputHintsFromContext returns the output hints (zero value if absent).
func OutputHintsFromContext(ctx context.Context) OutputHints {
	h, _ := ctx.Value(outputHintsKey{}).(OutputHints)
	return h
}

// spawnInheritKey is the context key for marking a spawn-inherit session.
type spawnInheritKey struct{}

// WithSpawnInherit marks a context as running inside a spawn inherit session.
// The spawn tool checks this and rejects nested inherit calls.
func WithSpawnInherit(ctx context.Context) context.Context {
	return context.WithValue(ctx, spawnInheritKey{}, true)
}

// IsSpawnInherit returns true if the context is inside a spawn inherit session.
func IsSpawnInherit(ctx context.Context) bool {
	v, _ := ctx.Value(spawnInheritKey{}).(bool)
	return v
}
