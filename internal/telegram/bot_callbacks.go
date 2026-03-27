package telegram

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/command"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)
// buildCommandKeyboard groups KeyboardOptions by row and returns an
// InlineKeyboardMarkup with callback data prefixed by "cmd:/cmdName ".
// Rows with more than 3 buttons are auto-split into multiple rows.
func buildCommandKeyboard(cmdName string, opts []command.KeyboardOption) gotgbot.InlineKeyboardMarkup {
	rowMap := make(map[int][]gotgbot.InlineKeyboardButton)
	maxRow := 0
	for _, opt := range opts {
		rowMap[opt.Row] = append(rowMap[opt.Row], gotgbot.InlineKeyboardButton{
			Text:         opt.Label,
			CallbackData: fmt.Sprintf("cmd:/%s %s", cmdName, opt.Data),
		})
		if opt.Row > maxRow {
			maxRow = opt.Row
		}
	}
	var rows [][]gotgbot.InlineKeyboardButton
	for i := 0; i <= maxRow; i++ {
		if buttons, ok := rowMap[i]; ok {
			rows = append(rows, layoutButtons(buttons)...)
		}
	}
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// layoutButtons splits a slice of buttons into rows of reasonable width.
// It uses at most 3 buttons per row, dropping to 2 if any button label
// exceeds 14 characters (to keep text readable on narrow screens).
func layoutButtons(buttons []gotgbot.InlineKeyboardButton) [][]gotgbot.InlineKeyboardButton {
	n := len(buttons)
	perRow := 3
	for _, b := range buttons {
		if len([]rune(b.Text)) > 14 {
			perRow = 2
			break
		}
	}
	if n <= perRow {
		return [][]gotgbot.InlineKeyboardButton{buttons}
	}
	var rows [][]gotgbot.InlineKeyboardButton
	for i := 0; i < n; i += perRow {
		end := i + perRow
		if end > n {
			end = n
		}
		rows = append(rows, buttons[i:end])
	}
	return rows
}

// singleButtonKeyboard returns an InlineKeyboardMarkup with one button.
func singleButtonKeyboard(text, callbackData string) gotgbot.InlineKeyboardMarkup {
	return gotgbot.InlineKeyboardMarkup{
		InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
			{Text: text, CallbackData: callbackData},
		}},
	}
}

func (b *Bot) sendCommandKeyboard(chatID int64, cmdName string, header string, opts []command.KeyboardOption) {
	_, _ = b.client.SendMessage(chatID, header, &gotgbot.SendMessageOpts{
		ReplyMarkup: buildCommandKeyboard(cmdName, opts),
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

	// Permission prompt callbacks: "perm:<choice>:<reason>"
	// Send the choice as a raw keystroke to the backend's TUI.
	if strings.HasPrefix(cq.Data, "perm:") {
		payload := cq.Data[5:] // "1:go vet on backend" or just "1"
		choice := payload
		reason := ""
		if idx := strings.IndexByte(payload, ':'); idx >= 0 {
			choice = payload[:idx]
			reason = payload[idx+1:]
		}
		if pr, ok := b.handler.(platform.PermissionResponder); ok {
			sk := b.sessionKeyForMsg(chatID)
			_ = pr.SendPermissionResponse(ctx, sk, choice)
		}
		// Edit the message to show what was approved.
		editText := "✅ Approved"
		if reason != "" {
			editText = "✅ " + reason
		}
		_, _, _ = b.client.EditMessageText(
			ConvertToTelegramHTML(editText, b.tableOpts()),
			&gotgbot.EditMessageTextOpts{
				ChatId:    chatID,
				MessageId: cq.Message.GetMessageId(),
				ParseMode: "HTML",
			})
		return
	}

	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 {
		return
	}
	action := parts[1] // "show" or "hide"
	msgID := cq.Message.GetMessageId()

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
	if b.dispatcher == nil {
		return
	}

	// Check if this bare subcommand needs a chained keyboard (e.g. /tmux kill → pick session)
	if parentName, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, cmdText, chatID); ok {
		b.editMessageWithKeyboard(chatID, msgID, parentName, cmdText, opts)
		return
	}

	dr := b.dispatcher.DispatchCallback(ctx, chatID, cmdText)
	var result string
	if len(dr.Response.Parts) > 0 {
		result = strings.Join(dr.Response.Parts, "\n\n")
	} else {
		result = dr.Response.Text
	}
	if !dr.Handled {
		result = "Unknown command: " + cmdText
	}

	display := ConvertToTelegramHTML(result, b.tableOpts())
	if len(display) > 4096 {
		display = display[:4090] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	// If the response includes a keyboard, edit with both text and keyboard.
	if len(dr.Response.Keyboard) > 0 {
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(cmdText, "/"), " ")
		kb := buildCommandKeyboard(cmdName, dr.Response.Keyboard)
		_, _, err := b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
			ChatId:      chatID,
			MessageId:   msgID,
			ParseMode:   "HTML",
			ReplyMarkup: kb,
		})
		if err != nil {
			b.logger().Debugf("command callback HTML+keyboard edit failed: %v, retrying as plain text", err)
			_, _, _ = b.client.EditMessageText(result, &gotgbot.EditMessageTextOpts{
				ChatId:      chatID,
				MessageId:   msgID,
				ReplyMarkup: kb,
			})
		}
		return
	}

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
	display := "/" + parentName + " " + strings.TrimPrefix(cmdText, "/"+parentName+" ") + ":"
	kb := buildCommandKeyboard(parentName, opts)
	_, _, _ = b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: msgID,
		ReplyMarkup: kb,
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
		kb := singleButtonKeyboard("Hide", "tc:hide")
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgID, stored)
		kb := singleButtonKeyboard("Show full", "tc:show")
		_, _, _ = b.client.EditMessageText(stored.compactText, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
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
		expanded := formatThinkingExpanded(entry.thinkingText, entry.responseHTML, b.resolveDisplay(b.sessionKeyForMsg(chatID)).DisplayWidth)
		kb := singleButtonKeyboard("Hide thinking", "th:hide")
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
		})
	case "hide":
		kb := singleButtonKeyboard("Show thinking", "th:show")
		_, _, _ = b.client.EditMessageText(entry.responseHTML, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
		})
	}
}

// formatThinkingExpanded prepends thinking text above a separator, with the response below.
func formatThinkingExpanded(thinkingText, responseHTML string, displayWidth int) string {
	result := buildThinkingHTML(responseHTML, thinkingText, displayWidth)
	// Telegram messages are limited to 4096 characters; truncate thinking if needed.
	if len(result) > 4096 {
		divider := "\n" + strings.Repeat("—", displayWidth) + "\n\n"
		budget := 4096 - len(responseHTML) - len(divider) - len("<i>") - len("</i>") - len("\n... (truncated)")
		if budget < 100 {
			budget = 100
		}
		escaped := truncateHTMLSafe(htmlEscape(thinkingText), budget) + "\n... (truncated)"
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

// thinkingEntry stores thinking text and response HTML for compact mode toggle.
type thinkingEntry struct {
	responseHTML string // the original response HTML (collapsed state)
	thinkingText string // raw thinking text
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

