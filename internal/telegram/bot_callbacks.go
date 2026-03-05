package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)
func (b *Bot) sendCommandKeyboard(chatID int64, cmdName string, opts []command.KeyboardOption) {
	// Group options by row
	rowMap := make(map[int][]gotgbot.InlineKeyboardButton)
	maxRow := 0
	for _, opt := range opts {
		data := fmt.Sprintf("cmd:/%s %s", cmdName, opt.Data)
		rowMap[opt.Row] = append(rowMap[opt.Row], gotgbot.InlineKeyboardButton{
			Text:         opt.Label,
			CallbackData: data,
		})
		if opt.Row > maxRow {
			maxRow = opt.Row
		}
	}

	rows := make([][]gotgbot.InlineKeyboardButton, 0, maxRow+1)
	for i := 0; i <= maxRow; i++ {
		if buttons, ok := rowMap[i]; ok {
			rows = append(rows, buttons)
		}
	}

	label := fmt.Sprintf("/%s:", cmdName)
	_, _ = b.client.SendMessage(chatID, label, &gotgbot.SendMessageOpts{
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
}

// handleCallbackQuery processes inline keyboard button presses for tool result
// and thinking block expansion, and command keyboard selections.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *gotgbot.CallbackQuery) {
	if cq.Data == "" || cq.Message.GetChat().Id == 0 {
		return
	}
	chatID := cq.Message.GetChat().Id

	// Always answer the callback query to dismiss the loading indicator.
	defer func() {
		_, _ = b.client.AnswerCallbackQuery(cq.Id, nil)
	}()

	// Command keyboard callbacks: "cmd:/name args"
	if strings.HasPrefix(cq.Data, "cmd:") {
		cmdText := cq.Data[4:] // strip "cmd:" prefix
		b.handleCommandCallback(ctx, chatID, cq.Message.GetMessageId(), cmdText)
		return
	}

	parts := strings.SplitN(cq.Data, ":", 3)
	if len(parts) != 3 {
		return
	}
	action := parts[1] // "show" or "hide"
	msgID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}

	switch parts[0] {
	case "tc":
		b.handleToolCallCallback(chatID, action, msgID)
	case "th":
		b.handleThinkingCallback(chatID, action, msgID)
	}
}

// handleCommandCallback executes a command from an inline keyboard press
// and edits the original message to show the result.
func (b *Bot) handleCommandCallback(ctx context.Context, chatID, msgID int64, cmdText string) {
	cmdCtx := context.WithValue(ctx, command.LastMessageUserKey{}, "")
	cmdCtx = context.WithValue(cmdCtx, command.ChatIDKey{}, chatID)
	cmdCtx = context.WithValue(cmdCtx, command.DisplayWidthKey{}, b.displayWidth)

	// Check if this bare subcommand needs a chained keyboard (e.g. /tmux kill → pick session)
	if parentName, opts, ok := b.commands.LookupChainKeyboard(cmdCtx, cmdText); ok {
		b.editMessageWithKeyboard(chatID, msgID, parentName, cmdText, opts)
		return
	}

	result, ok := b.commands.Dispatch(cmdCtx, cmdText)
	if !ok {
		result = "Unknown command: " + cmdText
	}

	// Strip multi-message separators for edit (edit replaces single message)
	result = strings.ReplaceAll(result, "\x00", "\n\n")

	display := ConvertToTelegramHTML(result, b.tableOpts())
	if len(display) > 4096 {
		display = display[:4090] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	_, _, err := b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: msgID,
		ParseMode: "HTML",
	})
	if err != nil {
		b.logger().Debugf("command callback HTML edit failed: %v, retrying as plain text", err)
		_, _, _ = b.client.EditMessageText(result, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
		})
	}
}

// editMessageWithKeyboard replaces the message with a chained inline keyboard.
func (b *Bot) editMessageWithKeyboard(chatID, msgID int64, parentName, cmdText string, opts []command.KeyboardOption) {
	// Group options by row
	rowMap := make(map[int][]command.KeyboardOption)
	for _, o := range opts {
		rowMap[o.Row] = append(rowMap[o.Row], o)
	}
	maxRow := 0
	for r := range rowMap {
		if r > maxRow {
			maxRow = r
		}
	}
	var rows [][]gotgbot.InlineKeyboardButton
	for r := 0; r <= maxRow; r++ {
		ropts := rowMap[r]
		if len(ropts) == 0 {
			continue
		}
		var buttons []gotgbot.InlineKeyboardButton
		for _, o := range ropts {
			buttons = append(buttons, gotgbot.InlineKeyboardButton{
				Text:         o.Label,
				CallbackData: fmt.Sprintf("cmd:/%s %s", parentName, o.Data),
			})
		}
		rows = append(rows, buttons)
	}

	display := "/" + parentName + " " + strings.TrimPrefix(cmdText, "/"+parentName+" ") + ":"
	_, _, _ = b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: msgID,
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
}

// handleToolCallCallback handles tool call expand/collapse button presses.
func (b *Bot) handleToolCallCallback(chatID int64, action string, msgID int64) {
	toolTextVal, ok := b.toolResults.Load(msgID)
	if !ok {
		return
	}
	stored := toolTextVal.(toolResultEntry)

	switch action {
	case "show":
		var expanded string
		if stored.result == "" {
			// Tool still running — show params with placeholder.
			expanded = formatToolCallWithResult(stored.fullInput, "⏳ Running...")
		} else {
			expanded = formatToolCallWithResult(stored.fullInput, stored.result)
		}
		// Mark as expanded so ToolResultObserver can update when result arrives.
		stored.expanded = true
		stored.chatID = chatID
		b.toolResults.Store(msgID, stored)
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide", CallbackData: fmt.Sprintf("tc:hide:%d", msgID)},
				}},
			},
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgID, stored)
		_, _, _ = b.client.EditMessageText(stored.compactText, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", msgID)},
				}},
			},
		})
	}
}

// handleThinkingCallback handles thinking block expand/collapse button presses.
func (b *Bot) handleThinkingCallback(chatID int64, action string, msgID int64) {
	val, ok := b.thinkingStore.Load(msgID)
	if !ok {
		return
	}
	entry := val.(thinkingEntry)

	switch action {
	case "show":
		expanded := formatThinkingExpanded(entry.thinkingText, entry.responseHTML, b.displayWidth)
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide thinking", CallbackData: fmt.Sprintf("th:hide:%d", msgID)},
				}},
			},
		})
	case "hide":
		_, _, _ = b.client.EditMessageText(entry.responseHTML, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show thinking", CallbackData: fmt.Sprintf("th:show:%d", msgID)},
				}},
			},
		})
	}
}

// formatThinkingExpanded prepends thinking text above a separator, with the response below.
func formatThinkingExpanded(thinkingText, responseHTML string, displayWidth int) string {
	escaped := htmlEscapeBot(thinkingText)
	divider := "\n" + strings.Repeat("—", displayWidth) + "\n"
	result := "<i>" + escaped + "</i>" + divider + responseHTML
	// Telegram messages are limited to 4096 characters; truncate thinking if needed.
	if len(result) > 4096 {
		budget := 4096 - len(responseHTML) - len(divider) - len("<i>") - len("</i>") - len("\n... (truncated)")
		if budget < 100 {
			budget = 100
		}
		escaped = truncateHTMLSafe(escaped, budget) + "\n... (truncated)"
		result = "<i>" + escaped + "</i>" + divider + responseHTML
	}
	return result
}

// truncateHTMLSafe truncates HTML-escaped text to maxLen bytes without splitting
// HTML entities (e.g. &amp; &lt; &gt;). If the cut falls inside an entity, it
// backs up to before the '&'.
func truncateHTMLSafe(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	s = s[:maxLen]
	// If we cut inside an HTML entity, back up to before the '&'.
	if idx := strings.LastIndex(s, "&"); idx != -1 {
		if !strings.Contains(s[idx:], ";") {
			s = s[:idx]
		}
	}
	return s
}

// toolCallTracker manages tool call visibility state during an agent turn.
// It encapsulates the mutable state shared between ToolCallObserver and
// ToolResultObserver callbacks (message ID, text snapshots, mutex).
type toolCallTracker struct {
	bot    *Bot
	chatID int64

	mu       sync.Mutex
	msgID    int64  // Telegram message ID of the current tool-call message
	text     string // last compact summary HTML (full mode) or full HTML (preview mode)
	fullText string // last full formatted tool call HTML (full mode only)
}

// lastMsgID returns the current tool-call message ID (thread-safe).
func (t *toolCallTracker) lastMsgID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgID
}

// resetMsgID clears the tool-call message ID (e.g. after intermediate text).
func (t *toolCallTracker) resetMsgID() {
	t.mu.Lock()
	t.msgID = 0
	t.mu.Unlock()
}

// observeToolCall handles tool call visibility via send+edit pattern.
func (t *toolCallTracker) observeToolCall(toolName string, params json.RawMessage) {
	if t.bot.showToolCalls == "off" || t.bot.showToolCalls == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.bot.showToolCalls == "full" {
		t.sendFullModeToolCall(toolName, params)
		return
	}
	t.sendPreviewModeToolCall(toolName, params)
}

// sendFullModeToolCall sends a compact summary with a "Show full" button.
func (t *toolCallTracker) sendFullModeToolCall(toolName string, params json.RawMessage) {
	compact := formatToolCallCompact(toolName, params)
	full := t.bot.formatToolCall(toolName, params)
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
				{Text: "Show full", CallbackData: "tc:show:0"},
			}},
		},
	}
	sent, err := t.bot.client.SendMessage(t.chatID, compact, sendOpts)
	if err != nil {
		t.bot.logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.msgID = sent.MessageId
	t.text = compact
	t.fullText = full
	t.bot.toolResults.Store(t.msgID, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		chatID:      t.chatID,
	})
	_, _, _ = t.bot.client.EditMessageText(compact, &gotgbot.EditMessageTextOpts{
		ChatId:    t.chatID,
		MessageId: t.msgID,
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
				{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", t.msgID)},
			}},
		},
	})
}

// sendPreviewModeToolCall sends or edits a tool call message (overwriting previous).
func (t *toolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := t.bot.formatToolCall(toolName, params)
	sendOpts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
	if t.msgID == 0 {
		sent, err := t.bot.client.SendMessage(t.chatID, text, sendOpts)
		if err != nil {
			t.bot.logger().Debugf("send tool call msg: %v", err)
			return
		}
		t.msgID = sent.MessageId
		t.text = text
	} else {
		_, _, err := t.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: t.msgID,
			ParseMode: "HTML",
		})
		if err != nil {
			t.bot.logger().Debugf("edit tool call msg: %v", err)
		}
		t.text = text
	}
}

// observeToolResult stores tool results for inline keyboard expansion (full mode only).
func (t *toolCallTracker) observeToolResult(toolName string, result string, isError bool) {
	if t.bot.showToolCalls != "full" {
		return
	}
	t.mu.Lock()
	msgID := t.msgID
	compact := t.text
	full := t.fullText
	t.mu.Unlock()
	if msgID == 0 {
		return
	}

	var wasExpanded bool
	if prev, ok := t.bot.toolResults.Load(msgID); ok {
		entry := prev.(toolResultEntry)
		wasExpanded = entry.expanded
	}

	t.bot.toolResults.Store(msgID, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		result:      result,
		expanded:    wasExpanded,
		chatID:      t.chatID,
	})
	if t.bot.toolDetailStore != nil {
		t.bot.toolDetailStore.Store(msgID, compact, full, result)
	}

	if wasExpanded {
		expanded := formatToolCallWithResult(full, result)
		_, _, _ = t.bot.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide", CallbackData: fmt.Sprintf("tc:hide:%d", msgID)},
				}},
			},
		})
	}
}

// toolResultEntry stores the compact summary, full input text, and result
// for inline keyboard expansion in "full" mode.
type toolResultEntry struct {
	compactText string // compact one-line summary (collapsed state)
	fullInput   string // full formatted tool call HTML with JSON params
	result      string // the raw tool result text (empty while tool is running)
	expanded    bool   // true if user clicked "Show full" before result arrived
	chatID      int64  // chat where the message lives (for deferred edits)
}

// thinkingEntry stores thinking text and response HTML for compact mode toggle.
type thinkingEntry struct {
	responseHTML string // the original response HTML (collapsed state)
	thinkingText string // raw thinking text
}

// formatToolCallWithResult combines a tool call message with its result,
// truncating the result so the total message fits within Telegram's 4096 char limit.
func formatToolCallWithResult(toolText, result string) string {
	const maxLen = 4096
	separator := "\n\n📋 <b>Result:</b>\n<pre>"
	suffix := "</pre>"

	overhead := len(toolText) + len(separator) + len(suffix)
	if overhead >= maxLen {
		// Tool text alone is too long; just return it as-is.
		return toolText
	}

	escapedResult := htmlEscapeBot(result)
	available := maxLen - overhead
	if len(escapedResult) > available {
		escapedResult = escapedResult[:available-3] + "..."
	}
	return toolText + separator + escapedResult + suffix
}

// sanitizeError replaces the bot token in an error string to prevent it
// from leaking into log files.
func (b *Bot) sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	if b.botToken == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), b.botToken, "[REDACTED]")
}

