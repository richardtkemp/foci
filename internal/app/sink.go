package app

import (
	"context"
	"fmt"
	"sync"

	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/platform"
)

// appSink translates a turnevent.Event stream into FAP frames for one turn on
// one conversation. It is the app's native-streaming sink: text.delta per
// chunk (no message-edit rate limits), bracketed by turn.start / text.end.
//
// One appSink per turn. Concurrency: Emit is called from the turn goroutine;
// the started/delivered flags are guarded by mu, and the binding's send is
// itself goroutine-safe (atomic seq + buffered channel).
type appSink struct {
	b      *convBinding
	turnID string

	mu        sync.Mutex
	started   bool // turn.start emitted (lazy — only once real text streams)
	delivered bool // any content shown this turn
}

func newAppSink(b *convBinding) *appSink {
	return &appSink{b: b, turnID: fap.NewULID()}
}

// DeliversToPlatform implements turnevent.Sink — output is always user-facing.
func (s *appSink) DeliversToPlatform() bool { return true }

func (s *appSink) ensureStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.b.send(fap.TurnStart{ConversationID: s.b.convID, TurnID: s.turnID})
}

func (s *appSink) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *appSink) markDelivered() {
	s.mu.Lock()
	s.delivered = true
	s.mu.Unlock()
}

// Emit implements turnevent.Sink.
func (s *appSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TurnStart:
		s.b.send(fap.Typing{ConversationID: s.b.convID, On: true})

	case turnevent.TextDelta:
		if e.Delta == "" {
			return
		}
		s.ensureStarted()
		s.b.send(fap.TextDelta{ConversationID: s.b.convID, TurnID: s.turnID, Text: e.Delta})
		s.markDelivered()

	case turnevent.TextBlock:
		// Intermediate blocks (tool-loop replies) are complete mid-turn
		// messages — deliver each as its own message row. Final-phase text is
		// carried by TurnComplete, handled below.
		if e.Phase != turnevent.PhaseIntermediate {
			return
		}
		clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.Text))
		if clean == "" {
			return
		}
		s.b.send(fap.ServerMessage{
			ConversationID: s.b.convID,
			MessageID:      fap.NewULID(),
			Role:           "agent",
			Text:           clean,
		})
		s.markDelivered()

	case turnevent.TurnComplete:
		s.finish(ctx, e)
	}
}

func (s *appSink) finish(ctx context.Context, e turnevent.TurnComplete) {
	defer s.b.send(fap.Typing{ConversationID: s.b.convID, On: false})

	text := e.FinalText
	if e.Err != nil && ctx.Err() == nil {
		text = fmt.Sprintf("Error: %s", e.Err.Error())
	}
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))

	if s.isStarted() {
		// Streaming deltas already shown — finalize the streamed message.
		var final *string
		if clean != "" {
			final = &clean
		}
		s.b.send(fap.TextEnd{
			ConversationID: s.b.convID,
			TurnID:         s.turnID,
			MessageID:      fap.NewULID(),
			FinalText:      final,
		})
	} else if clean != "" {
		// No streaming happened (non-streamed turn). Deliver the final text as
		// a single message.
		s.b.send(fap.ServerMessage{
			ConversationID: s.b.convID,
			MessageID:      fap.NewULID(),
			Role:           "agent",
			Text:           clean,
		})
	}

	s.emitMeta(e)
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
	if meta.Model == "" && meta.PrevCostUsd == nil && meta.Tokens == nil {
		return
	}
	s.b.send(meta)
}
