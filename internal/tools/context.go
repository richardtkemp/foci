package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

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

// OutputFormatJSON is the OutputHints.Format a shell call's --json sets (#1215).
const OutputFormatJSON = "json"

// WantsJSON reports whether the caller asked for --json output. Only exec-bridge
// calls can: the API tool path never sets hints.
func WantsJSON(ctx context.Context) bool {
	return OutputHintsFromContext(ctx).Format == OutputFormatJSON
}

// JSONResult renders v as a --json tool result. A tool that returns one
// documents its shape in Tool.JSONOutput.
func JSONResult(v any) (ToolResult, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // fetched pages and bodies are full of <, > and &
	if err := enc.Encode(v); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Text: b.String(), JSON: true}, nil
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

// spawnCharacterKey is the context key marking an API agent's character
// spawn's tool calls (#2643).
type spawnCharacterKey struct{}

// WithSpawnCharacter marks a context as an API character spawn's: nobody is
// there to answer it, so a tool that would need the user (ask, close_session)
// refuses. A delegated character spawn gets the same through its exec
// bridge's registry (RegistryForAccess, BatchAccessParent).
func WithSpawnCharacter(ctx context.Context) context.Context {
	return context.WithValue(ctx, spawnCharacterKey{}, true)
}

// IsSpawnCharacter reports whether ctx is an API character spawn's.
func IsSpawnCharacter(ctx context.Context) bool {
	v, _ := ctx.Value(spawnCharacterKey{}).(bool)
	return v
}

// errSpawnNeedsUser is what a tool that needs the user returns in a
// character spawn.
func errSpawnNeedsUser(tool string) error {
	return fmt.Errorf("%s: refused in a character spawn — nobody is there to answer; decide for yourself or put it in your result", tool)
}
