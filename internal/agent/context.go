package agent

import (
	"context"
	"sync"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/provider"
)

var platformTriggers sync.Map // trigger string → true

// RegisterPlatformTrigger registers a trigger as a messaging platform trigger.
// Platform triggers are identity-mapped (trigger == platform name) and are
// always user-initiated. Called from platform package init() functions.
func RegisterPlatformTrigger(trigger string) {
	platformTriggers.Store(trigger, true)
}

// turnMetadataKey is the context key for TurnMetadata.
type turnMetadataKey struct{}

// TurnMetadata carries platform-specific identity information through the agent turn.
// Set by the platform layer so the agent can log conversation entries without
// coupling to platform-specific types.
type TurnMetadata struct {
	UserID   string // platform-specific user identifier (e.g. Telegram user ID)
	Username string // display name / username
	ChatID   int64  // from platform message or session key
}

// WithTurnMetadata attaches TurnMetadata to a context.
func WithTurnMetadata(ctx context.Context, meta *TurnMetadata) context.Context {
	return context.WithValue(ctx, turnMetadataKey{}, meta)
}

// TurnMetadataFromContext extracts TurnMetadata from context (nil if absent).
func TurnMetadataFromContext(ctx context.Context) *TurnMetadata {
	meta, _ := ctx.Value(turnMetadataKey{}).(*TurnMetadata)
	return meta
}

// triggerKey is the context key for the turn trigger type.
type triggerKey struct{}

// WithTrigger attaches a trigger label (e.g. "user", "keepalive") to a context.
func WithTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, triggerKey{}, trigger)
}

// TriggerFromContext extracts the trigger label from context (empty if absent).
func TriggerFromContext(ctx context.Context) string {
	s, _ := ctx.Value(triggerKey{}).(string)
	return s
}

// receivedAtKey is the context key for the user-message receipt time.
type receivedAtKey struct{}

// WithReceivedAt attaches the platform receipt time to a context. Platform
// workers set this from the first message of the batched turn so the meta
// header reflects when the user actually sent the message, not the time it
// was drained out of the queue.
func WithReceivedAt(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, receivedAtKey{}, t)
}

// ReceivedAtFromContext extracts the receipt time (zero Time if absent).
func ReceivedAtFromContext(ctx context.Context) time.Time {
	t, _ := ctx.Value(receivedAtKey{}).(time.Time)
	return t
}

// isUserTrigger returns true if the trigger represents a human-initiated message
// (typed via a messaging platform, spoken via voice, or sent via HTTP /send).
// Returns false for system-initiated triggers (keepalive, wake, cron, warnings, etc.).
func isUserTrigger(trigger string) bool {
	if _, ok := platformTriggers.Load(trigger); ok {
		return true
	}
	switch trigger {
	case "", "user", "voice":
		return true
	default:
		return false
	}
}

// isMemoryTrigger reports whether a turn is a memory-formation pass —
// reflection or end-of-session memory — rather than substantive activity.
//
// On delegated (CC) agents these passes inject into the *main* session, so a
// turn running them bumps that session's last_activity_at. Counting them as
// activity would defeat the reflection-skip guard
// (Agent.SessionIndex.ReflectionRedundant): a reflection would forever look
// like "new activity since the last reflection", so the next /reset would
// always fire a redundant reflection. These triggers are therefore excluded
// from last_activity_at bumps (RegisterSessionIndex / TouchActivity).
//
// Note: this is deliberately narrow. Keepalive, background work, wake, and
// cron turns DO count as activity — autonomous work is worth reflecting on.
// Only the reflection/memory passes themselves are excluded, because only they
// would self-trigger the guard.
func isMemoryTrigger(trigger string) bool {
	switch trigger {
	case "reflection", "session_end_memory":
		return true
	default:
		return false
	}
}

// nudgesAllowed reports whether automatic nudges should fire on this turn.
// Nudges (turn-interval, regex, after-tools, pre-answer) exist to shape the
// agent's user-facing reply. System-internal turns — reflection, keepalive,
// consolidation, session-end memory — produce no user-facing answer, so every
// nudge path is suppressed for them. Without this gate the pre-answer gate
// fires "verify before answering" on reflection turns that wrote memory files,
// and system turns inflate the every_n_turns lifetime counter. (#815)
func nudgesAllowed(ts *TurnState) bool {
	return isUserTrigger(ts.Trigger)
}

// triggerToPlatform maps a trigger label to a platform name for the [meta] header.
// Platform tells the agent which transport delivered the message:
//   - telegram: message arrived via Telegram text
//   - voice: message arrived via voice (speech-to-text)
//   - android: message arrived via Android app
//   - api: message arrived via HTTP /send endpoint
//   - tmux: message from tmux watch inactivity detection
//   - async: message from async tool result (shell, http_request, etc.)
//   - cron: message is system-initiated (keepalive, wake, scheduled, etc.)
func triggerToPlatform(trigger string) string {
	if _, ok := platformTriggers.Load(trigger); ok {
		return trigger
	}
	switch trigger {
	case "voice":
		return "voice"
	case "android":
		return "android"
	case "", "user":
		return "api"
	case "tmux_watch":
		return "tmux"
	case "async_notify":
		return "async"
	default:
		return "cron"
	}
}

// --- sink-based event emission ---
//
// The per-turn event stream replaces the old TurnCallbacks struct. Producers
// emit events via the sink attached to ctx (turnevent.SinkFromContext). The
// helpers below are thin wrappers that match the pre-refactor call shapes so
// existing producer code does not need to reach into turnevent on every line.

// emitIntermediateText sends an intermediate assistant text block through the
// sink attached to ctx. No-op for empty text — matches the old
// sendIntermediateCtx shape.
func emitIntermediateText(ctx context.Context, text string) {
	if text == "" {
		return
	}
	turnevent.Emit(ctx, turnevent.TextBlock{Text: text, Phase: turnevent.PhaseIntermediate})
}

// emitActivity fires a heartbeat event — sinks use this to refresh
// typing-indicator state between content events.
func emitActivity(ctx context.Context) {
	turnevent.Emit(ctx, turnevent.Activity{})
}

// emitToolCall announces a tool invocation on the event stream.
func emitToolCall(ctx context.Context, name, id string, params []byte) {
	turnevent.Emit(ctx, turnevent.ToolCall{Name: name, ID: id, Args: params})
}

// emitToolResult announces a tool execution outcome.
func emitToolResult(ctx context.Context, name, id, result string, isError bool) {
	turnevent.Emit(ctx, turnevent.ToolResult{Name: name, ID: id, Output: result, IsError: isError})
}

// emitThinkingBlock delivers a full extended-thinking block.
func emitThinkingBlock(ctx context.Context, text string) {
	if text == "" {
		return
	}
	turnevent.Emit(ctx, turnevent.ThinkingBlock{Text: text})
}

// emitTextDelta delivers a streaming text fragment.
func emitTextDelta(ctx context.Context, delta string) {
	if delta == "" {
		return
	}
	turnevent.Emit(ctx, turnevent.TextDelta{Delta: delta})
}

// emitThinkingDelta delivers a streaming thinking fragment.
func emitThinkingDelta(ctx context.Context, delta string) {
	if delta == "" {
		return
	}
	turnevent.Emit(ctx, turnevent.ThinkingDelta{Delta: delta})
}

// emitRetryNotice announces the first retry of an upstream API call.
func emitRetryNotice(ctx context.Context, endpoint string) {
	turnevent.Emit(ctx, turnevent.RetryNotice{Endpoint: endpoint})
}

// emitRetrySuccess announces that a retry succeeded (clears retry notice UI).
func emitRetrySuccess(ctx context.Context) {
	turnevent.Emit(ctx, turnevent.RetrySuccess{})
}

// steerBlocks drains pending steer messages via the context-attached Steerer
// and returns them as [user] content blocks for injection into the next prompt.
// Returns nil when no steerer is set or no text is pending.
func steerBlocks(ctx context.Context) []provider.ContentBlock {
	st := turnevent.SteererFromContext(ctx)
	if st == nil {
		return nil
	}
	steers := st.PendingSteers()
	if len(steers) == 0 {
		return nil
	}
	blocks := make([]provider.ContentBlock, len(steers))
	for i, s := range steers {
		blocks[i] = provider.ContentBlock{Type: "text", Text: "[user] " + s}
	}
	return blocks
}
