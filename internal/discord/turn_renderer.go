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

// discordBackend implements turn.Platform for Discord. It owns all layout:
// markdown chopping at Discord's 2000-char limit, message identity, streaming
// rollover, and thinking-button placement.
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

// Deliver performs the terminal delivery. It composes the message body per
// thinking mode, chops it into discordMaxChars chunks, and lays those chunks
// over the stream's existing message sequence (editing/appending/deleting as
// needed) or sends fresh messages when nothing surfaced.
func (b *discordBackend) Deliver(p turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	body, hasButton, thinkingText := b.composeBody(p)
	chunks := splitMessage(body, discordMaxChars)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	var ids []string
	if stream != nil {
		ids = stream.MsgIDs()
	}

	// Fresh send: nothing surfaced.
	if len(ids) == 0 {
		var used []string
		for i, chunk := range chunks {
			last := i == len(chunks)-1
			if last && hasButton {
				id, err := b.sendChunkWithButton(chunk, thinkingText)
				if err != nil {
					return turn.DeliveryResult{MsgIDs: used}, err
				}
				used = append(used, id)
				continue
			}
			if id, ok := b.sendMarkdown(chunk); ok {
				used = append(used, id)
			}
		}
		return turn.DeliveryResult{MsgIDs: used}, nil
	}

	// Finalize-in-place: lay the chunks over the existing live sequence.
	var used []string
	for i, chunk := range chunks {
		last := i == len(chunks)-1
		if i < len(ids) {
			if last && hasButton {
				if err := b.editChunkWithButton(ids[i], chunk, thinkingText); err != nil {
					return turn.DeliveryResult{MsgIDs: used}, err
				}
			} else {
				if err := b.editMarkdown(ids[i], chunk); err != nil {
					b.bot.logger().Debugf("deliver edit: %v", err)
				}
			}
			used = append(used, ids[i])
			continue
		}
		if last && hasButton {
			id, err := b.sendChunkWithButton(chunk, thinkingText)
			if err != nil {
				return turn.DeliveryResult{MsgIDs: used}, err
			}
			used = append(used, id)
		} else if id, ok := b.sendMarkdown(chunk); ok {
			used = append(used, id)
		}
	}

	// Delete leftover messages from the live sequence (final shorter than live).
	if len(ids) > len(chunks) {
		for _, orphan := range ids[len(chunks):] {
			if err := b.bot.api.ChannelMessageDelete(b.channelID, orphan); err != nil {
				b.bot.logger().Debugf("deliver delete orphan %s: %v", orphan, err)
			}
		}
	}

	return turn.DeliveryResult{MsgIDs: used}, nil
}

// EditInPlace replaces a single existing message (a tool-call preview) in
// place. Returns turn.ErrTooLongForEdit if the composed body would need to
// split across more than one message.
func (b *discordBackend) EditInPlace(msgID string, p turn.Payload) error {
	body, hasButton, thinkingText := b.composeBody(p)
	chunks := splitMessage(body, discordMaxChars)
	if len(chunks) > 1 {
		return turn.ErrTooLongForEdit
	}
	chunk := body
	if len(chunks) == 1 {
		chunk = chunks[0]
	}
	if hasButton {
		return b.editChunkWithButton(msgID, chunk, thinkingText)
	}
	return b.editMarkdown(msgID, chunk)
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
func (b *discordBackend) composeBody(p turn.Payload) (body string, hasButton bool, thinkingText string) {
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

// sendMarkdown sends one (already-chunked) markdown body as a single message and
// returns its ID.
func (b *discordBackend) sendMarkdown(text string) (string, bool) {
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

// sendChunkWithButton sends a chunk with a "Show thinking" button and stores the
// thinking entry keyed on the sent message ID.
func (b *discordBackend) sendChunkWithButton(text, thinkingText string) (string, error) {
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

// editMarkdown edits an existing message with the given markdown body.
func (b *discordBackend) editMarkdown(msgID, text string) error {
	_, err := b.bot.api.ChannelMessageEdit(b.channelID, msgID, text)
	return err
}

// editChunkWithButton edits an existing message with a "Show thinking" button
// and stores the thinking entry keyed on that message ID.
func (b *discordBackend) editChunkWithButton(msgID, text, thinkingText string) error {
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
