package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/toolformat"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// newToolCallTracker creates a shared turn.ToolCallTracker backed by
// Discord-specific formatting and messaging.
func newToolCallTracker(bot *Bot, channelID string, d turn.TurnDisplay) *turn.ToolCallTracker {
	backend := &discordTrackerBackend{bot: bot, channelID: channelID}
	store := &discordTrackerStore{bot: bot, channelID: channelID}
	display := turn.TrackerDisplay{ShowToolCalls: d.ShowToolCalls}
	return turn.NewToolCallTracker(backend, store, display, compactResultHint)
}

// discordTrackerBackend implements turn.TrackerBackend for Discord.
type discordTrackerBackend struct {
	bot       *Bot
	channelID string
}

func (b *discordTrackerBackend) FormatCompact(toolName string, params json.RawMessage) string {
	return formatToolCallCompact(toolName, params)
}

func (b *discordTrackerBackend) FormatFull(toolName string, params json.RawMessage, showMode string) string {
	return formatToolCallFull(toolName, params, showMode, b.bot.display.ToolCallPreviewChars)
}

func (b *discordTrackerBackend) FormatWithResult(toolText, result string) string {
	return formatToolCallWithResult(toolText, result)
}

func (b *discordTrackerBackend) FormatHintSuffix(hint string) string {
	return " -> " + hint
}

func (b *discordTrackerBackend) FormatRetry(endpoint string) string {
	return fmt.Sprintf("*%s is busy right now, retrying...*", endpoint)
}

func (b *discordTrackerBackend) FormatRetryClear() string {
	return "*Request completed*"
}

func (b *discordTrackerBackend) Send(text string) (string, error) {
	sent, err := b.bot.api.ChannelMessageSend(b.channelID, text)
	if err != nil {
		return "", err
	}
	return sent.ID, nil
}

func (b *discordTrackerBackend) SendWithButton(text, btnLabel, btnData string) (string, error) {
	buttons := buildButtonComponents([]platform.ButtonChoice{{Label: btnLabel, Data: btnData}}, "")
	sent, err := b.bot.api.ChannelMessageSendComplex(b.channelID, &discordgo.MessageSend{
		Content:    text,
		Components: buttons,
	})
	if err != nil {
		return "", err
	}
	return sent.ID, nil
}

func (b *discordTrackerBackend) Edit(msgID, text string) error {
	_, err := b.bot.api.ChannelMessageEdit(b.channelID, msgID, text)
	return err
}

func (b *discordTrackerBackend) EditWithButton(msgID, text, btnLabel, btnData string) error {
	buttons := buildButtonComponents([]platform.ButtonChoice{{Label: btnLabel, Data: btnData}}, "")
	_, err := b.bot.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    b.channelID,
		ID:         msgID,
		Content:    &text,
		Components: &buttons,
	})
	return err
}

func (b *discordTrackerBackend) Delete(msgID string) error {
	return b.bot.api.ChannelMessageDelete(b.channelID, msgID)
}

func (b *discordTrackerBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// discordTrackerStore implements turn.TrackerStore backed by the Bot's
// sync.Map and optional ToolDetailStore.
type discordTrackerStore struct {
	bot       *Bot
	channelID string
}

func (s *discordTrackerStore) StoreEntry(msgID, compact, full, result string, expanded bool) {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	s.bot.toolResults.Store(id, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		result:      result,
		expanded:    expanded,
		channelID:   s.channelID,
	})
}

func (s *discordTrackerStore) IsExpanded(msgID string) bool {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	if prev, ok := s.bot.toolResults.Load(id); ok {
		return prev.(toolResultEntry).expanded
	}
	return false
}

func (s *discordTrackerStore) Persist(msgID, compact, full, result string) {
	if s.bot.toolDetailStore == nil {
		return
	}
	id, _ := strconv.ParseInt(msgID, 10, 64)
	s.bot.toolDetailStore.Store(id, compact, full, result)
}

// toolResultEntry stores the compact summary, full input text, and result
// for button expansion in "full" mode.
type toolResultEntry struct {
	compactText string // compact one-line summary (collapsed state)
	fullInput   string // full formatted tool call with JSON params
	result      string // the raw tool result text (empty while tool is running)
	expanded    bool   // true if user clicked "Show full" before result arrived
	channelID   string // channel where the message lives (for deferred edits)
}

// toolEmoji maps tool names to per-tool display prefixes.
var toolEmoji = map[string]string{
	"shell":           "> ",
	"web_fetch":       ">> ",
	"web_search":      "?? ",
	"http_request":    ">> ",
	"read":            "[] ",
	"write":           "<> ",
	"edit":            "/\\ ",
	"tmux":            ":: ",
	"todo":            "-- ",
	"send_to_chat":    ">> ",
	"memory_search":   "** ",
	"spawn":           "++ ",
	"scratchpad":      "// ",
	"send_to_session": ">> ",
	"remind":          ".. ",
}

// emojiForTool returns the per-tool prefix, falling back to a generic prefix.
func emojiForTool(name string) string {
	if e, ok := toolEmoji[name]; ok {
		return e
	}
	return "## "
}

// formatToolCallCompact returns a compact one-line summary.
func formatToolCallCompact(toolName string, params json.RawMessage) string {
	prefix := emojiForTool(toolName)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return fmt.Sprintf("%s**%s**", prefix, toolName)
	}

	summary := toolformat.CompactSummary(toolName, m)
	if summary == "" {
		return fmt.Sprintf("%s**%s**", prefix, toolName)
	}
	return fmt.Sprintf("%s**%s**: %s", prefix, toolName, summary)
}

// formatToolCallFull formats a tool call for display in Discord.
func formatToolCallFull(toolName string, params json.RawMessage, showMode string, maxChars int) string {
	if maxChars == 0 {
		maxChars = 450
	}
	paramStr := provider.UnescapeUnicodeJSON(string(params))
	var pretty bytes.Buffer
	if json.Indent(&pretty, json.RawMessage(paramStr), "", "  ") == nil {
		paramStr = pretty.String()
	}
	if showMode != "full" && len(paramStr) > maxChars {
		paramStr = paramStr[:maxChars] + "..."
	}
	prefix := emojiForTool(toolName)
	return fmt.Sprintf("%s**%s**\n```json\n%s\n```", prefix, toolName, paramStr)
}

// formatToolCallWithResult combines a tool call message with its result,
// truncating the result so the total message fits within Discord's 2000 char limit.
func formatToolCallWithResult(toolText, result string) string {
	const maxLen = discordMaxChars
	separator := "\n\n**Result:**\n```\n"
	suffix := "\n```"

	overhead := len(toolText) + len(separator) + len(suffix)
	if overhead >= maxLen {
		return toolText
	}

	available := maxLen - overhead
	if len(result) > available {
		result = result[:available-3] + "..."
	}
	return toolText + separator + result + suffix
}

// compactResultHint delegates to toolformat.CompactResultHint.
func compactResultHint(toolName string, params json.RawMessage, result string) string {
	return toolformat.CompactResultHint(toolName, params, result)
}
