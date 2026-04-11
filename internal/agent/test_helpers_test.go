package agent

import (
	"context"
	"encoding/json"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

// TurnCallbacks is a test-only compatibility shim that replays the pre-refactor
// callback shape onto the new turnevent.Sink event stream. Tests construct a
// TurnCallbacks with the fields they care about and attach it via
// WithTurnCallbacks — the adapter Emits events back as the matching callback
// calls, preserving existing assertions on received-call counts/order.
//
// Production code uses turnevent.Sink directly; this shim exists solely so
// the large surface of pre-refactor tests can keep compiling while their
// assertions are migrated.
type TurnCallbacks struct {
	ReplyFunc             func(text string)
	ToolCallObserver      func(toolName string, params json.RawMessage)
	ToolResultObserver    func(toolName string, result string, isError bool)
	ThinkingObserver      func(thinking string)
	ActivityFunc          func()
	TextDeltaObserver     func(delta string)
	ThinkingDeltaObserver func(delta string)
	SteerCheckFunc        func() []string
	RetryNotifyFunc       func(endpoint string)
	RetrySuccessFunc      func()
	OnTurnDone            func()
}

// asSink wraps this TurnCallbacks into a turnevent.Sink that replays events
// as the matching callback calls.
func (cb *TurnCallbacks) asSink() turnevent.Sink {
	return turnevent.SinkFunc(func(_ context.Context, ev turnevent.Event) {
		switch e := ev.(type) {
		case turnevent.TextBlock:
			if cb.ReplyFunc != nil && e.Phase == turnevent.PhaseIntermediate {
				cb.ReplyFunc(e.Text)
			}
		case turnevent.TextDelta:
			if cb.TextDeltaObserver != nil {
				cb.TextDeltaObserver(e.Delta)
			}
		case turnevent.ThinkingBlock:
			if cb.ThinkingObserver != nil {
				cb.ThinkingObserver(e.Text)
			}
		case turnevent.ThinkingDelta:
			if cb.ThinkingDeltaObserver != nil {
				cb.ThinkingDeltaObserver(e.Delta)
			}
		case turnevent.ToolCall:
			if cb.ToolCallObserver != nil {
				cb.ToolCallObserver(e.Name, e.Args)
			}
		case turnevent.ToolResult:
			if cb.ToolResultObserver != nil {
				cb.ToolResultObserver(e.Name, e.Output, e.IsError)
			}
		case turnevent.Activity:
			if cb.ActivityFunc != nil {
				cb.ActivityFunc()
			}
		case turnevent.RetryNotice:
			if cb.RetryNotifyFunc != nil {
				cb.RetryNotifyFunc(e.Endpoint)
			}
		case turnevent.RetrySuccess:
			if cb.RetrySuccessFunc != nil {
				cb.RetrySuccessFunc()
			}
		case turnevent.TurnComplete:
			if cb.OnTurnDone != nil {
				cb.OnTurnDone()
			}
		}
	})
}

// WithTurnCallbacks is the test-only compat attachment helper. It wires the
// TurnCallbacks into ctx as a turnevent.Sink and, if SteerCheckFunc is set,
// also installs a matching Steerer.
func WithTurnCallbacks(ctx context.Context, cb *TurnCallbacks) context.Context {
	if cb == nil {
		return ctx
	}
	ctx = turnevent.WithSink(ctx, cb.asSink())
	if cb.SteerCheckFunc != nil {
		ctx = turnevent.WithSteerer(ctx, turnevent.SteererFunc(cb.SteerCheckFunc))
	}
	return ctx
}

// hmTest wraps HandleMessage with a BufferSink so tests that want the old
// (string, error) return shape can keep their existing assertions.
// Defined as a method so `ag.HandleMessage(ctx, sk, msg)` call sites can be
// mechanically rewritten to `ag.hmTest(ctx, sk, msg)`.
//
// If ctx already carries a sink (e.g. from a test-local TurnCallbacks compat
// shim), both receive every event via a TeeSink — that way pre-existing
// callback-based assertions keep firing alongside the BufferSink capture.
func (a *Agent) hmTest(ctx context.Context, sessionKey, message string) (string, error) {
	buf := turnevent.NewBufferSink()
	existing := turnevent.SinkFromContext(ctx)
	ctx = turnevent.WithSink(ctx, turnevent.NewTeeSink(existing, buf))
	err := a.HandleMessage(ctx, sessionKey, []string{message}, nil)
	return buf.FinalText(), err
}

// hmTestAttachments is the attachment-aware counterpart, replacing
// HandleMessageWithAttachments at call sites.
func (a *Agent) hmTestAttachments(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (string, error) {
	buf := turnevent.NewBufferSink()
	existing := turnevent.SinkFromContext(ctx)
	ctx = turnevent.WithSink(ctx, turnevent.NewTeeSink(existing, buf))
	err := a.HandleMessage(ctx, sessionKey, texts, attachments)
	return buf.FinalText(), err
}
