package discord

import (
	"strconv"
	"strings"

	"foci/internal/log"
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
	transport := &discordStreamTransport{
		bot:       bot,
		channelID: msg.ChannelID,
	}
	interval := bot.streamInterval()
	newSW := func() *turn.StreamWriter {
		return turn.NewStreamWriter(transport, interval, d.MaxChars-100, d.StreamOutput)
	}
	return turn.NewTurnRenderer(backend, tracker, d, newSW)
}

// discordBackend implements turn.TurnBackend for Discord.
type discordBackend struct {
	bot       *Bot
	msg       *discordgo.Message
	channelID string
	width     int
}

func (b *discordBackend) FormatResponse(text string) string {
	return text // Discord uses plain markdown, no conversion needed.
}

func (b *discordBackend) SendReply(text string) {
	b.bot.sendReply(b.msg, text)
}

func (b *discordBackend) SendChunked(formatted string) {
	b.bot.sendMarkdownChunks(b.channelID, formatted)
}

func (b *discordBackend) EditMessage(msgID, formatted string) error {
	_, err := b.bot.session.ChannelMessageEdit(b.channelID, msgID, formatted)
	return err
}

func (b *discordBackend) SendWithThinkingButton(formatted, thinkingText string) error {
	chunks := splitMessage(formatted, discordMaxChars)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			b.bot.sendMarkdownChunks(b.channelID, chunk)
			continue
		}
		buttons := singleButton("Show thinking", "th:show")
		sent, err := b.bot.session.ChannelMessageSendComplex(b.channelID, &discordgo.MessageSend{
			Content:    chunk,
			Components: buttons,
		})
		if err != nil {
			return err
		}
		msgIDInt, _ := strconv.ParseInt(sent.ID, 10, 64)
		b.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
			responseText: chunk,
			thinkingText: thinkingText,
		})
	}
	return nil
}

func (b *discordBackend) EditWithThinkingButton(msgID, formatted, thinkingText string) error {
	buttons := singleButton("Show thinking", "th:show")
	_, err := b.bot.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    b.channelID,
		ID:         msgID,
		Content:    &formatted,
		Components: &buttons,
	})
	if err != nil {
		return err
	}
	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	b.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
		responseText: formatted,
		thinkingText: thinkingText,
	})
	return nil
}

func (b *discordBackend) BuildThinkingCombined(responseFormatted, thinkingText string) string {
	divider := "\n" + strings.Repeat("-", b.width) + "\n\n"
	return "*" + thinkingText + "*" + divider + responseFormatted
}

func (b *discordBackend) FormatStreamPreview(preview string) string {
	return preview + "\n\n*(full response below)*"
}

func (b *discordBackend) SendTyping() {
	_ = b.bot.session.ChannelTyping(b.channelID)
}

func (b *discordBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// discordStreamTransport implements turn.StreamTransport for Discord.
type discordStreamTransport struct {
	bot       *Bot
	channelID string
}

func (t *discordStreamTransport) SendInitial(text string) (string, error) {
	msg, err := t.bot.session.ChannelMessageSend(t.channelID, text)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (t *discordStreamTransport) EditStream(msgID, text string) error {
	_, err := t.bot.session.ChannelMessageEdit(t.channelID, msgID, text)
	return err
}
