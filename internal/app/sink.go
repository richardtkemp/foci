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
//   - the typing indicator, driven off the turn boundary as structured
//     fap.Typing frames (the renderer's SendTyping is a no-op for the app);
//   - the structured fap.Meta frame on turn completion (model/cost/tokens/mana —
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

	// statusFn supplies the meta-frame status chips (mana%, mana state, gap).
	// nil = those fields are omitted (e.g. a sink with no agent context).
	statusFn func() (manaPct *int, manaState, gap string)

	// thinking dedups the thinking indicator: the reasoning phase produces many
	// ThinkingDelta events but only a state change emits a frame. Safe without a
	// lock — the turn-event stream is single-producer and strictly ordered.
	thinking bool
	warming  bool
	tool     string
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
	// A turn abandoned without TurnComplete would strand the indicators; cleanup is
	// deferred by the agent on every turn, so clearing them here is the backstop.
	s.cleanup = func() {
		b.setInTurn(false)
		s.setThinking(false)
		s.setWarming(false)
		s.setTool("")
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
		s.b.setInTurn(true)
		s.setWarming(true)
		s.b.send(fap.Typing{ConversationID: s.b.convID, On: true})
		s.inner.Emit(ctx, ev)

	case turnevent.SubagentText:
		// No app surface for subagent progress yet; dropping it avoids OnReply
		// prematurely finalizing the in-flight reply stream. See type doc.

	case turnevent.ThinkingDelta, turnevent.ThinkingBlock:
		s.setWarming(false)
		s.setTool("")
		s.setThinking(true)
		s.inner.Emit(ctx, ev)

	case turnevent.ToolCall:
		s.setWarming(false)
		s.setThinking(false)
		s.setTool(e.Name)
		s.inner.Emit(ctx, ev)

	case turnevent.ToolResult:
		s.setTool("")
		s.inner.Emit(ctx, ev)

	case turnevent.TextDelta, turnevent.TextBlock:
		s.setWarming(false)
		s.setThinking(false)
		s.setTool("")
		s.inner.Emit(ctx, ev)

	case turnevent.TurnComplete:
		// Forward first so the final text is delivered (TextEnd / ServerMessage),
		// then bracket with thinking-off (safety), typing-off, and the meta frame.
		s.inner.Emit(ctx, ev)
		s.setThinking(false)
		s.setWarming(false)
		s.setTool("")
		s.b.setInTurn(false)
		s.b.send(fap.Typing{ConversationID: s.b.convID, On: false})
		s.emitMeta(e)

	default:
		s.inner.Emit(ctx, ev)
	}
}

// setThinking toggles the extended-thinking indicator, emitting the roster
// snapshot + live delta frame only on an actual state change.
func (s *appSink) setThinking(on bool) {
	if s.thinking == on {
		return
	}
	s.thinking = on
	s.b.setThinkingSnapshot(on)
	s.b.send(fap.Thinking{ConversationID: s.b.convID, On: on})
}

// setWarming toggles the "warming up" indicator, emitting the roster snapshot
// + live delta frame only on an actual state change.
func (s *appSink) setWarming(on bool) {
	if s.warming == on {
		return
	}
	s.warming = on
	s.b.setWarmingSnapshot(on)
	s.b.send(fap.Warming{ConversationID: s.b.convID, On: on})
}

// setTool toggles the running-tool indicator, emitting the roster snapshot +
// live frame only on an actual change. Empty name means no tool is running.
func (s *appSink) setTool(name string) {
	if s.tool == name {
		return
	}
	s.tool = name
	s.b.setToolSnapshot(name)
	s.b.send(fap.Tool{ConversationID: s.b.convID, On: name != "", Name: name})
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
		meta.ManaPct, meta.ManaState, meta.Gap = s.statusFn()
	}
	if meta.Model == "" && meta.PrevCostUsd == nil && meta.Tokens == nil &&
		meta.ManaPct == nil && meta.ManaState == "" && meta.Gap == "" {
		return
	}
	s.b.send(meta)
}
