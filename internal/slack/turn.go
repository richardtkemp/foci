package slack

import (
	"context"
	"encoding/json"
	"sync"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/turn"
	"foci/internal/turnevent"
)

// slackBackend implements turn.Platform and turn.ChunkWriter for one turn of
// one session: every primitive addresses that session's own Target. Long-reply
// splitting, Markdown rendering and tool-call/thinking display are #2459 A2
// (A1 sends the reply as one plain-text message); the shared
// turn.DeliverChunks loop still drives delivery so A2 only swaps the
// primitives.
type slackBackend struct {
	core       *agentCore
	target     Target
	sessionKey string // names the session in refusal logs
}

var (
	_ turn.Platform    = (*slackBackend)(nil)
	_ turn.ChunkWriter = (*slackBackend)(nil)
)

// noToolTracker is the inert turn.SinkTracker: tool calls and retries are
// observed but displayed nowhere in A1 (show_tool_calls is forced off).
type noToolTracker struct{}

func (noToolTracker) LastMsgID() string                               { return "" }
func (noToolTracker) ResetMsgID()                                     {}
func (noToolTracker) CleanupPreview()                                 {}
func (noToolTracker) ObserveToolCall(string, string, json.RawMessage) {}
func (noToolTracker) ObserveToolResult(string, string, string, bool)  {}
func (noToolTracker) NotifyRetry(string)                              {}
func (noToolTracker) ClearRetryNotification()                         {}

// OpenStream begins the live streaming surface: one message that later
// updates edit in place.
func (b *slackBackend) OpenStream() turn.StreamSink {
	return &slackStreamSink{core: b.core, target: b.target, sessionKey: b.sessionKey}
}

// Deliver performs the terminal delivery via the shared chunk loop.
func (b *slackBackend) Deliver(p turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	return turn.DeliverChunks(b, p, stream)
}

// EditInPlace replaces one existing message (a tool-call preview — unused
// while tool display is off, kept for A2) via the shared single-message edit.
func (b *slackBackend) EditInPlace(msgID string, p turn.Payload) error {
	return turn.EditChunksInPlace(b, msgID, p)
}

// ComposeBody: thinking is never requested on Slack in A1, so the body is
// the payload text with no button.
func (b *slackBackend) ComposeBody(p turn.Payload) (string, bool, string) {
	return p.Text, false, ""
}

// Split returns the body as one chunk (A2 splits long replies).
func (b *slackBackend) Split(body string) []string {
	if body == "" {
		return []string{""}
	}
	return []string{body}
}

func (b *slackBackend) SendChunk(text string) (string, bool) {
	ref, err := b.core.post(context.Background(), b.target, text)
	if err != nil {
		b.core.logger().Errorf("send error (channel=%s session=%s): %v", b.target.Channel, b.sessionKey, err)
		return "", false
	}
	return ref.String(), true
}

// SendChunkWithButton never fires (ComposeBody reports no button); it sends
// plainly so the shared loop's contract holds.
func (b *slackBackend) SendChunkWithButton(text, _ string) (string, error) {
	ref, err := b.core.post(context.Background(), b.target, text)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

func (b *slackBackend) EditChunk(msgID, text string) error {
	ref, err := parseEditRef(msgID)
	if err != nil {
		return err
	}
	return b.core.editText(context.Background(), ref, text)
}

// EditChunkWithButton edits with no button (never requested in A1).
func (b *slackBackend) EditChunkWithButton(msgID, text, _ string) error {
	return b.EditChunk(msgID, text)
}

func (b *slackBackend) DeleteMsg(msgID string) {
	ref, ok := parseMsgRef(msgID)
	if !ok {
		return
	}
	if err := b.core.api.deleteMessage(context.Background(), ref.Channel, ref.TS); err != nil {
		b.core.logger().Debugf("deliver delete orphan %s: %v", msgID, err)
	}
}

// SendTyping is a no-op (no typing indicator on Socket Mode bots).
func (b *slackBackend) SendTyping() {}

func (b *slackBackend) Logger() *log.ComponentLogger { return b.core.logger() }

// slackStreamSink is the live streaming surface: the first Update posts one
// message; later Updates chat.update the same message.
type slackStreamSink struct {
	core       *agentCore
	target     Target
	sessionKey string // names the session in refusal logs

	mu     sync.Mutex
	closed bool
	ref    msgRef
	sent   bool
}

func (s *slackStreamSink) Update(fullText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.sent {
		ref, err := s.core.post(context.Background(), s.target, fullText)
		if err != nil {
			// Slack refused or the call failed: the partial is lost (the
			// terminal delivery usually repeats it). Visible at the default
			// level, like SendChunk's ERROR — "logged and lost" (review F4).
			s.core.logger().Warnf("stream post refused (channel=%s session=%s): %v", s.target.Channel, s.sessionKey, err)
			return
		}
		s.ref, s.sent = ref, true
		return
	}
	if err := s.core.editText(context.Background(), s.ref, fullText); err != nil {
		s.core.logger().Warnf("stream edit refused (channel=%s session=%s): %v", s.target.Channel, s.sessionKey, err)
	}
}

func (s *slackStreamSink) Close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.sent
}

func (s *slackStreamSink) MsgIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent {
		return nil
	}
	return []string{s.ref.String()}
}

// --- agent.Driver ---

// NewTurnSink implements agent.Driver: builds the per-turn renderer bound to
// the envelope's own conversation. Returns (nil, nil) when the session has
// no registered Slack conversation — the agent then skips the whole turn
// (RunTurn returns before running the model), so the drop is logged here
// rather than staying silent: refusing to guess a conversation must be
// visible (#2459 req 5, 6). Inbound messages register their target before
// enqueueing, so today this is the defensive arm, not a reachable path.
func (c *Conn) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	target, ok := c.core.targetFor(env.SessionKey)
	if !ok {
		c.core.logger().Warnf("dropped turn for session %s (no Slack conversation registered for it; the turn does not run)", env.SessionKey)
		return nil, nil
	}
	d := c.core.turnDisplay()
	backend := &slackBackend{core: c.core, target: target, sessionKey: env.SessionKey}
	newSB := func() *turn.StreamBuffer {
		return turn.NewStreamBuffer(backend.OpenStream(), c.core.streamInterval, d.StreamOutput)
	}
	renderer := turn.NewTurnRenderer(backend, noToolTracker{}, d, newSB)
	sink := turn.NewStreamingSink(renderer, noToolTracker{}, c)
	return sink, renderer.Cleanup
}

// WrapTurn implements agent.Driver: runs the turn, logging a failed one.
func (c *Conn) WrapTurn(_ context.Context, fn func() error) error {
	err := fn()
	if err != nil {
		c.core.logger().Errorf("agent error: %v", err)
	}
	return err
}

// turnDisplay resolves the turn's display settings: tool calls and thinking
// are off in A1 (documented as ignored), streaming follows the resolved
// platform display config.
func (c *agentCore) turnDisplay() turn.TurnDisplay {
	return turn.TurnDisplay{
		ShowToolCalls: "off",
		ShowThinking:  "off",
		StreamOutput:  c.streamOutput,
	}
}
