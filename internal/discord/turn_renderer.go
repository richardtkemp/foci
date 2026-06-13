package discord

import (
	"strconv"
	"strings"
	"sync"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// newTurnRenderer creates a turn.TurnRenderer backed by Discord APIs.
// The tracker is created externally and passed in so the worker can wire
// its observeToolCall/observeToolResult directly to agent callbacks.
func newTurnRenderer(bot *Bot, msg *discordgo.Message, tracker *turn.ToolCallTracker, d turn.TurnDisplay) *turn.TurnRenderer {
	backend := &discordBackend{
		bot:       bot,
		msg:       msg,
		channelID: msg.ChannelID,
		width:     d.DisplayWidth,
	}
	interval := bot.streamInterval()
	newSB := func() *turn.StreamBuffer {
		return turn.NewStreamBuffer(backend.OpenStream(), interval, d.StreamOutput)
	}
	return turn.NewTurnRenderer(backend, tracker, d, newSB)
}

// Compile-time checks.
var (
	_ turn.Platform    = (*discordBackend)(nil)
	_ turn.ChunkWriter = (*discordBackend)(nil)
)

// discordBackend implements turn.Platform and turn.ChunkWriter for Discord. It
// owns all layout: markdown chopping at Discord's 2000-char limit, message
// identity, streaming rollover, and thinking-button placement. The shared
// delivery loop (turn.DeliverChunks) drives the ChunkWriter primitives.
type discordBackend struct {
	bot       *Bot
	msg       *discordgo.Message
	channelID string
	width     int
}

// OpenStream begins a live streaming surface.
func (b *discordBackend) OpenStream() turn.StreamSink {
	return &discordStreamSink{
		bot:       b.bot,
		channelID: b.channelID,
	}
}

// Deliver performs the terminal delivery via the shared chunk-delivery loop.
func (b *discordBackend) Deliver(p turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	return turn.DeliverChunks(b, p, stream)
}

// EditInPlace replaces a single existing message (a tool-call preview) in place
// via the shared single-message edit. Returns turn.ErrTooLongForEdit if the
// composed body would need to split across more than one message.
func (b *discordBackend) EditInPlace(msgID string, p turn.Payload) error {
	return turn.EditChunksInPlace(b, msgID, p)
}

// Split chops a markdown body at Discord's char limit, preserving code fences.
func (b *discordBackend) Split(body string) []string {
	return splitMessage(body, discordMaxChars)
}

// DeleteMsg deletes a leftover live-stream message, best-effort.
func (b *discordBackend) DeleteMsg(msgID string) {
	if err := b.bot.api.ChannelMessageDelete(b.channelID, msgID); err != nil {
		b.bot.logger().Debugf("deliver delete orphan %s: %v", msgID, err)
	}
}

func (b *discordBackend) SendTyping() {
	b.bot.SetTyping(true)
}

func (b *discordBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// composeBody builds the message body for the payload per thinking mode and
// reports whether the last chunk should carry a "Show thinking" button (compact
// mode) and the raw thinking text to store with it.
func (b *discordBackend) ComposeBody(p turn.Payload) (body string, hasButton bool, thinkingText string) {
	switch p.ThinkingMode {
	case "full":
		divider := "\n" + strings.Repeat("-", b.width) + "\n\n"
		return "*" + p.ThinkingText + "*" + divider + p.Text, false, ""
	case "compact":
		return p.Text, true, p.ThinkingText
	default:
		return p.Text, false, ""
	}
}

// SendChunk sends one (already-chunked) markdown body as a single message and
// returns its ID; ok=false on a (logged) send failure.
func (b *discordBackend) SendChunk(text string) (string, bool) {
	msg, err := b.bot.api.ChannelMessageSend(b.channelID, text)
	if err != nil {
		b.bot.logger().Errorf("send error (channel=%s): %s", b.channelID, b.bot.sanitizeError(err))
		if isUnknownChannel(err) {
			b.bot.clearStaleChannel(b.channelID)
		}
		return "", false
	}
	return msg.ID, true
}

// SendChunkWithButton sends a chunk with a "Show thinking" button and stores the
// thinking entry keyed on the sent message ID.
func (b *discordBackend) SendChunkWithButton(text, thinkingText string) (string, error) {
	buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
	sent, err := b.bot.api.ChannelMessageSendComplex(b.channelID, &discordgo.MessageSend{
		Content:    text,
		Components: buttons,
	})
	if err != nil {
		return "", err
	}
	msgIDInt, _ := strconv.ParseInt(sent.ID, 10, 64)
	b.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
		responseText: text,
		thinkingText: thinkingText,
	})
	return sent.ID, nil
}

// EditChunk edits an existing message with the given markdown body.
func (b *discordBackend) EditChunk(msgID, text string) error {
	_, err := b.bot.api.ChannelMessageEdit(b.channelID, msgID, text)
	return err
}

// EditChunkWithButton edits an existing message with a "Show thinking" button
// and stores the thinking entry keyed on that message ID.
func (b *discordBackend) EditChunkWithButton(msgID, text, thinkingText string) error {
	buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
	_, err := b.bot.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    b.channelID,
		ID:         msgID,
		Content:    &text,
		Components: &buttons,
	})
	if err != nil {
		return err
	}
	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	b.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
		responseText: text,
		thinkingText: thinkingText,
	})
	return nil
}

// discordStreamSink is the live streaming surface returned by OpenStream. It
// owns the live message sequence and rolls over to new messages when the text
// exceeds a single discordMaxChars message.
type discordStreamSink struct {
	bot       *Bot
	channelID string

	mu       sync.Mutex
	closed   bool
	msgIDs   []string
	lastSent []string // last content sent per chunk index
}

// Update chops the full accumulated markdown into discordMaxChars chunks and
// edits existing messages (skipping unchanged chunks) or sends new messages
// (rollover) for chunks beyond the live sequence.
func (s *discordStreamSink) Update(fullText string) {
	chunks := splitMessage(fullText, discordMaxChars)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	for i, chunk := range chunks {
		if i < len(s.msgIDs) {
			// Skip unchanged chunks to avoid needless edits.
			if i < len(s.lastSent) && s.lastSent[i] == chunk {
				continue
			}
			if _, err := s.bot.api.ChannelMessageEdit(s.channelID, s.msgIDs[i], chunk); err != nil {
				s.bot.logger().Debugf("stream edit: %v", err)
			}
			s.setLastSent(i, chunk)
			continue
		}
		// Rollover: send a new message for this chunk.
		msg, err := s.bot.api.ChannelMessageSend(s.channelID, chunk)
		if err != nil {
			break
		}
		s.msgIDs = append(s.msgIDs, msg.ID)
		s.setLastSent(i, chunk)
	}
}

// setLastSent records the last content sent at chunk index i. Caller holds the lock.
func (s *discordStreamSink) setLastSent(i int, content string) {
	for len(s.lastSent) <= i {
		s.lastSent = append(s.lastSent, "")
	}
	s.lastSent[i] = content
}

// Close stops accepting updates and reports whether any message surfaced.
func (s *discordStreamSink) Close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return len(s.msgIDs) > 0
}

// MsgIDs returns the live sequence IDs, in order.
func (s *discordStreamSink) MsgIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.msgIDs))
	copy(out, s.msgIDs)
	return out
}
