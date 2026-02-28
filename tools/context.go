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
