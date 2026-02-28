package agent

import (
	"context"
	"encoding/json"
)

// turnCallbacksKey is the context key for TurnCallbacks.
type turnCallbacksKey struct{}

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

// TurnCallbacks holds per-turn callbacks scoped to a context.
// Using context avoids cross-turn races from mutable Agent fields.
type TurnCallbacks struct {
	ReplyFunc          ReplyFunc
	VoiceReplyFunc     VoiceReplyFunc
	ToolCallObserver   ToolCallObserver
	ToolResultObserver ToolResultObserver
	ActivityFunc       func()
}

// WithTurnCallbacks attaches TurnCallbacks to a context.
func WithTurnCallbacks(ctx context.Context, cb *TurnCallbacks) context.Context {
	return context.WithValue(ctx, turnCallbacksKey{}, cb)
}

// TurnCallbacksFromContext extracts TurnCallbacks from context (nil if absent).
func TurnCallbacksFromContext(ctx context.Context) *TurnCallbacks {
	cb, _ := ctx.Value(turnCallbacksKey{}).(*TurnCallbacks)
	return cb
}

// sendIntermediateCtx sends an intermediate reply via context callbacks.
func sendIntermediateCtx(ctx context.Context, text string) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ReplyFunc != nil && text != "" {
		cb.ReplyFunc(text)
	}
}

// signalActivityCtx calls the activity callback via context.
func signalActivityCtx(ctx context.Context) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ActivityFunc != nil {
		cb.ActivityFunc()
	}
}

// notifyToolCallCtx calls the tool call observer via context.
func notifyToolCallCtx(ctx context.Context, name string, params json.RawMessage) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ToolCallObserver != nil {
		cb.ToolCallObserver(name, params)
	}
}

// notifyToolResultCtx calls the tool result observer via context.
func notifyToolResultCtx(ctx context.Context, name string, result string, isError bool) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ToolResultObserver != nil {
		cb.ToolResultObserver(name, result, isError)
	}
}

// sendVoiceCtx sends a voice note via context callbacks.
func sendVoiceCtx(ctx context.Context, data []byte) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.VoiceReplyFunc != nil && len(data) > 0 {
		cb.VoiceReplyFunc(data)
	}
}

// noCompactKey is the context key for disabling compaction.
type noCompactKey struct{}

// WithNoCompact attaches a no-compact flag to a context.
// When set, the agent loop returns instead of triggering compaction.
func WithNoCompact(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCompactKey{}, true)
}

// NoCompactFromContext returns true if compaction should be skipped.
func NoCompactFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noCompactKey{}).(bool)
	return v
}
