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

// isUserTrigger returns true if the trigger represents a human-initiated message
// (typed in Telegram, spoken via voice, or sent via HTTP /send).
// Returns false for system-initiated triggers (keepalive, wake, cron, warnings, etc.).
func isUserTrigger(trigger string) bool {
	switch trigger {
	case "", "user", "telegram", "voice":
		return true
	default:
		return false
	}
}

// TurnCallbacks holds per-turn callbacks scoped to a context.
// Using context avoids cross-turn races from mutable Agent fields.
type TurnCallbacks struct {
	ReplyFunc            ReplyFunc
	ToolCallObserver     ToolCallObserver
	ToolResultObserver   ToolResultObserver
	ThinkingObserver     func(thinking string)
	ActivityFunc         func()
	TextDeltaObserver    func(delta string)
	ThinkingDeltaObserver func(delta string)
	SteerCheckFunc       func() string // non-blocking; returns "" if no pending steer
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

// notifyThinkingCtx calls the thinking observer via context.
func notifyThinkingCtx(ctx context.Context, thinking string) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ThinkingObserver != nil && thinking != "" {
		cb.ThinkingObserver(thinking)
	}
}

// notifyTextDeltaCtx calls the text delta observer via context.
func notifyTextDeltaCtx(ctx context.Context, delta string) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.TextDeltaObserver != nil && delta != "" {
		cb.TextDeltaObserver(delta)
	}
}

// notifyThinkingDeltaCtx calls the thinking delta observer via context.
func notifyThinkingDeltaCtx(ctx context.Context, delta string) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.ThinkingDeltaObserver != nil && delta != "" {
		cb.ThinkingDeltaObserver(delta)
	}
}

// steerCheckFromCtx calls the steer check function via context.
// Returns "" if no steer callback is set or no steer text is pending.
func steerCheckFromCtx(ctx context.Context) string {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.SteerCheckFunc != nil {
		return cb.SteerCheckFunc()
	}
	return ""
}

