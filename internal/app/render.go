package app

import (
	"encoding/json"
	"time"

	"foci/internal/app/fap"
	"foci/internal/log"
	"foci/internal/turn"
)

// appStreamInterval is the live-stream pump cadence for the native app. Unlike
// Telegram (which edits one message and is rate-limited to ~1 edit/sec), the FAP
// client appends deltas to a native bubble, so we can flush far more often. The
// pump still batches deltas between ticks — beneficial, it keeps a fast token
// stream from flooding the socket with one frame per token.
const appStreamInterval = 50 * time.Millisecond

// appBackend implements turn.Platform for the native app provider, so the app
// reuses the shared TurnRenderer + StreamingSink delivery coordination (the
// delivered-flag dance that dedups streamed deltas vs the intermediate TextBlock
// vs the final text, and resets the stream per reply segment) rather than
// re-implementing it. Only the OUTPUT shape is app-specific: where Telegram
// edits a message with a full snapshot, the app appends deltas (fap.TextDelta)
// and finalizes each segment with fap.TextEnd (or a fresh fap.ServerMessage when
// nothing streamed). One appBackend per turn, bound to the turn's conversation.
type appBackend struct {
	b      *convBinding
	logger *log.ComponentLogger
}

func newAppBackend(b *convBinding) *appBackend {
	return &appBackend{b: b, logger: log.NewComponentLogger("app:" + b.convID)}
}

// Compile-time check.
var (
	_ turn.Platform          = (*appBackend)(nil)
	_ turn.SubagentDeliverer = (*appBackend)(nil)
)

// OpenStream begins a live streaming surface for one reply segment. Each segment
// gets a fresh turnId, so a multi-reply turn (reply → tool → reply) renders as
// distinct bubbles instead of appending into one.
func (p *appBackend) OpenStream() turn.StreamSink {
	return &appStreamSink{b: p.b, turnID: fap.NewULID()}
}

// Deliver performs the terminal delivery of one segment. If the segment streamed
// (surfaced), it finalizes the existing turnId bubble with fap.TextEnd carrying
// the authoritative final text; otherwise it sends a fresh fap.ServerMessage.
func (p *appBackend) Deliver(pl turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	msgID := fap.NewULID()
	if ss, ok := stream.(*appStreamSink); ok && ss.surfaced() {
		final := pl.Text
		ss.b.send(fap.TextEnd{
			ConversationID: ss.b.convID,
			TurnID:         ss.turnID,
			MessageID:      msgID,
			FinalText:      &final,
		})
		return turn.DeliveryResult{MsgIDs: []string{msgID}}, nil
	}
	p.b.send(fap.ServerMessage{
		ConversationID: p.b.convID,
		MessageID:      msgID,
		Role:           "agent",
		Text:           pl.Text,
	})
	return turn.DeliveryResult{MsgIDs: []string{msgID}}, nil
}

// EditInPlace is unreachable for the app: the renderer only attempts a
// tool-preview edit when ShowToolCalls == "preview" AND the tracker has a live
// message ID, and the app uses neither (no-op tracker, ShowToolCalls off). The
// ErrTooLongForEdit return makes any unexpected call fall back to a fresh send.
func (p *appBackend) EditInPlace(string, turn.Payload) error { return turn.ErrTooLongForEdit }

// SendTyping is a no-op: the app's typing indicator is driven by the turn
// boundary (TurnStart/TurnComplete) in appSink, not refreshed mid-stream the way
// Telegram must to keep its indicator alive.
func (p *appBackend) SendTyping() {}

func (p *appBackend) Logger() *log.ComponentLogger { return p.logger }

// DeliverSubagentStart/Text/End implement turn.SubagentDeliverer so the app
// receives subagent (Task/Agent tool) progress as distinct, groupable frames —
// collapsed to one "Agent started/completed" entry client-side — instead of the
// blockquoted intermediate messages the OnReply fallback produces. SubagentTextRaw
// is true: the app renders traces in an expandable view and shows the agent name
// from the start frame, so it wants text raw (not the renderer's inline header).
func (p *appBackend) DeliverSubagentStart(groupKey, label string, runIndex int, prompt string) {
	p.b.send(fap.SubagentStart{ConversationID: p.b.convID, GroupKey: groupKey, Label: label, RunIndex: runIndex, Prompt: prompt})
}

func (p *appBackend) DeliverSubagentText(groupKey, text string) {
	p.b.send(fap.SubagentText{ConversationID: p.b.convID, GroupKey: groupKey, Text: text})
}

func (p *appBackend) DeliverSubagentEnd(groupKey string, runIndex int) {
	p.b.send(fap.SubagentEnd{ConversationID: p.b.convID, GroupKey: groupKey, RunIndex: runIndex})
}

func (p *appBackend) SubagentTextRaw() bool { return true }

// appStreamSink is the live streaming surface for one reply segment. It owns one
// turnId. Update receives the full accumulated snapshot from the turn-side pump;
// the sink emits only the new suffix as a fap.TextDelta (the client appends),
// lazily opening the turn with fap.TurnStart on the first non-empty delta.
type appStreamSink struct {
	b      *convBinding
	turnID string

	started bool // turn.start emitted (first delta sent)
	sent    int  // bytes already emitted as deltas
}

// Compile-time check.
var _ turn.StreamSink = (*appStreamSink)(nil)

// Update is called only by the turn-side pump goroutine (never concurrently with
// Close — happens-before via the pump's done channel), so it needs no lock.
func (s *appStreamSink) Update(fullText string) {
	if len(fullText) <= s.sent {
		return
	}
	if !s.started {
		s.started = true
		s.b.send(fap.TurnStart{ConversationID: s.b.convID, TurnID: s.turnID})
	}
	delta := fullText[s.sent:]
	s.sent = len(fullText)
	s.b.send(fap.TextDelta{ConversationID: s.b.convID, TurnID: s.turnID, Text: delta})
}

// Close reports whether any delta surfaced. A segment that opened (started) has
// always emitted at least one delta (TurnStart is coupled to the first delta).
func (s *appStreamSink) Close() bool { return s.started }

// surfaced reports whether this segment streamed any content. Read by Deliver
// after the pump has stopped (Finish), so no lock is needed.
func (s *appStreamSink) surfaced() bool { return s.started }

// MsgIDs returns the segment's turnId when it surfaced, else nil.
func (s *appStreamSink) MsgIDs() []string {
	if !s.started {
		return nil
	}
	return []string{s.turnID}
}

// noopTracker satisfies turn.SinkTracker (a superset of turn.ToolTracker) with
// no behaviour. The app surfaces neither tool-call previews nor retry notices in
// the stream, but the renderer requires a non-nil tracker (it calls LastMsgID /
// ResetMsgID / CleanupPreview). LastMsgID == "" makes the renderer's
// tool-preview edit path a no-op, so EditInPlace is never reached.
type noopTracker struct{}

// Compile-time check.
var _ turn.SinkTracker = noopTracker{}

func (noopTracker) LastMsgID() string                               { return "" }
func (noopTracker) ResetMsgID()                                     {}
func (noopTracker) CleanupPreview()                                 {}
func (noopTracker) ObserveToolCall(string, string, json.RawMessage) {}
func (noopTracker) ObserveToolResult(string, string, string, bool)  {}
func (noopTracker) NotifyRetry(string)                              {}
func (noopTracker) ClearRetryNotification()                         {}
