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
// (typed via a messaging platform, spoken via voice, or sent via HTTP /send).
// Returns false for system-initiated triggers (keepalive, wake, cron, warnings, etc.).
func isUserTrigger(trigger string) bool {
	switch trigger {
	case "", "user", "telegram", "voice":
		return true
	default:
		return false
	}
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
	switch trigger {
	case "telegram":
		return "telegram"
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
	RetryNotifyFunc      func(endpoint string) // called on first API retry; endpoint is the base URL being retried
	RetrySuccessFunc     func() // called when a retry succeeds (to clear/overwrite retry message)
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

// notifyRetryCtx calls the retry notification callback via context.
func notifyRetryCtx(ctx context.Context, endpoint string) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.RetryNotifyFunc != nil {
		cb.RetryNotifyFunc(endpoint)
	}
}

// notifyRetrySuccessCtx calls the retry success callback via context.
func notifyRetrySuccessCtx(ctx context.Context) {
	if cb := TurnCallbacksFromContext(ctx); cb != nil && cb.RetrySuccessFunc != nil {
		cb.RetrySuccessFunc()
	}
}

