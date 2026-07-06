package app

import (
	"context"

	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/turn"
)

// appSink is the app provider's per-turn turnevent.Sink. It is a thin wrapper
// around the shared turn.StreamingSink (the same delivery coordination Telegram
// and Discord use): all text streaming, intermediate-vs-final dedup, and
// per-segment stream reset are handled by StreamingSink + TurnRenderer driving
// an appBackend (see render.go). The wrapper adds only what is genuinely
// app-specific and has no home in the platform renderer:
//
//   - the agent activity indicator, driven off the turn boundary as structured
//     fap.Activity frames (the renderer's SendTyping is a no-op for the app);
//   - the structured fap.Meta frame on turn completion (model/cost/tokens —
//     the typed replacement for the [meta] text blob the Telegram bridge injects);
//   - dropping SubagentText, which the app has no surface for yet. Forwarding it
//     would route through OnReply and prematurely finalize the in-flight reply
//     stream, fragmenting the main message.
//
// One appSink per turn on one conversation.
type appSink struct {
	b     *convBinding
	inner *turn.StreamingSink

	// cleanup finishes the renderer's stream buffer (stops the pump goroutine).
	// Returned from NewTurnSink for the agent to defer, so an abandoned turn
	// (no TurnComplete) doesn't leak the pump.
	cleanup func()

	// statusFn supplies the meta-frame gap chip.
	// nil = that field is omitted (e.g. a sink with no agent context).
	statusFn func() string
}

// newAppSink builds the per-turn app sink: an appBackend (turn.Platform) wrapped
// by a TurnRenderer and StreamingSink, all driving FAP frames on the binding.
// Typing is owned by appSink (conn passed as nil to StreamingSink), so the
// indicator tracks the turn boundary as structured frames.
func newAppSink(b *convBinding) *appSink {
	backend := newAppBackend(b)
	d := turn.TurnDisplay{StreamOutput: true, ShowThinking: "off", ShowToolCalls: "off"}
	tracker := noopTracker{}
	newSB := func() *turn.StreamBuffer {
		return turn.NewStreamBuffer(backend.OpenStream(), appStreamInterval, d.StreamOutput)
	}
	renderer := turn.NewTurnRenderer(backend, tracker, d, newSB)
	inner := turn.NewStreamingSink(renderer, tracker, nil)
	s := &appSink{b: b, inner: inner}
	// A turn abandoned without TurnComplete would strand the indicator; cleanup is
	// deferred by the agent on every turn, so clearing the turn-scoped activity
	// here is the backstop. Session-scoped states (subagents/waiting) are NOT
	// cleared here — they outlive the turn by design.
	s.cleanup = func() {
		b.setTurnActivity(fap.ActivityKindIdle, "")
		renderer.Cleanup()
	}
	return s
}

// DeliversToPlatform implements turnevent.Sink — output is always user-facing.
func (s *appSink) DeliversToPlatform() bool { return true }

// Emit implements turnevent.Sink. It forwards to the shared StreamingSink for all
// text coordination and layers on the app-specific typing + meta frames.
func (s *appSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TurnStart:
		// A fresh turn means this conversation's caller is active again — clear any
		// session-scoped "waiting on another agent" state before the turn opens.
		s.b.setWaitingDetail("")
		s.b.setTurnActivity(fap.ActivityKindWarming, "")
		s.inner.Emit(ctx, ev)

	case turnevent.SubagentText:
		// No app surface for subagent progress yet; dropping it avoids OnReply
		// prematurely finalizing the in-flight reply stream. See type doc.

	case turnevent.ThinkingDelta, turnevent.ThinkingBlock:
		s.b.setTurnActivity(fap.ActivityKindThinking, "")
		s.inner.Emit(ctx, ev)

	case turnevent.ToolCall:
		s.b.setTurnActivity(fap.ActivityKindTool, e.Name)
		s.inner.Emit(ctx, ev)

	case turnevent.ToolResult:
		// Tool finished; the model is processing its result with no output token
		// yet — back to the "warming" (working) state until the next event.
		s.b.setTurnActivity(fap.ActivityKindWarming, "")
		s.inner.Emit(ctx, ev)

	case turnevent.TextDelta, turnevent.TextBlock:
		s.b.setTurnActivity(fap.ActivityKindTyping, "")
		s.inner.Emit(ctx, ev)

	case turnevent.TurnComplete:
		// Forward first so the final text is delivered (TextEnd / ServerMessage),
		// then close out the turn-scoped activity (→ idle) and emit the meta frame.
		s.inner.Emit(ctx, ev)
		s.b.setTurnActivity(fap.ActivityKindIdle, "")
		s.emitMeta(e)

	default:
		s.inner.Emit(ctx, ev)
	}
}

// emitMeta sends the user-facing status chips (model, cost, tokens) the app
// renders in the conversation header — the structured replacement for the
// [meta] text blob the Telegram bridge injects.
func (s *appSink) emitMeta(e turnevent.TurnComplete) {
	meta := fap.Meta{ConversationID: s.b.convID, Model: e.Model}
	if e.Cost > 0 {
		cost := e.Cost
		meta.PrevCostUsd = &cost
	}
	if e.Usage != nil {
		meta.Tokens = &fap.Tokens{
			In:  int64(e.Usage.InputTokens),
			Out: int64(e.Usage.OutputTokens),
			CR:  int64(e.Usage.CacheReadInputTokens),
			CW:  int64(e.Usage.CacheCreationInputTokens),
		}
	}
	if s.statusFn != nil {
		meta.Gap = s.statusFn()
	}
	if meta.Model == "" && meta.PrevCostUsd == nil && meta.Tokens == nil && meta.Gap == "" {
		return
	}
	s.b.send(meta)
}
