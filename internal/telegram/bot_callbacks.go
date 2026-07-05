package telegram

import (
	"context"
	"strconv"
	"strings"

	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

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

func (b *Bot) sendCommandKeyboard(cmdName string, header string, opts []command.KeyboardOption) {
	_, _ = b.SendTextWithButtons(header, dispatch.CmdButtons(cmdName, opts), "cmd:")
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

	msgID := cq.Message.GetMessageId()
	action, data := dispatch.ParseCallback(cq.Data)
	b.logger().Debugf("callback_query: action=%d raw=%q chatID=%d msgID=%d", action, cq.Data, chatID, msgID)
	switch action {
	case dispatch.CallbackCommand:
		b.handleCommandCallback(ctx, chatID, msgID, data)
	case dispatch.CallbackInteractive:
		editText, _, ok := platform.HandleInteractiveCallback(data)
		b.logger().Debugf("callback_query interactive: data=%q found=%v editText=%q", data, ok, editText)
		if ok && editText != "" {
			_, _, _ = b.client.EditMessageText(
				ConvertToTelegramHTML(editText, b.tableOpts()),
				&gotgbot.EditMessageTextOpts{
					ChatId:    chatID,
					MessageId: msgID,
					ParseMode: "HTML",
				})
		}
	case dispatch.CallbackToolCall:
		b.handleToolCallCallback(chatID, data, msgID)
	case dispatch.CallbackThinking:
		b.handleThinkingCallback(chatID, data, msgID)
	case dispatch.CallbackSubagentHide:
		b.handleSubagentHideCallback(chatID, data)
	}
}

// handleCommandCallback executes a command from an inline keyboard press
// and edits the original message to show the result.
func (b *Bot) handleCommandCallback(ctx context.Context, chatID, msgID int64, cmdText string) {
	if b.dispatcher == nil {
		return
	}

	outcome := b.dispatcher.DispatchCommandCallback(ctx, chatID, cmdText)

	if outcome.Chain != nil {
		b.editMessageWithKeyboard(chatID, msgID, outcome.Chain.CommandName, cmdText, outcome.Chain.Options)
		return
	}

	var result string
	var resp command.Response
	if outcome.Response != nil {
		resp = outcome.Response.Result.Response
		if len(resp.Parts) > 0 {
			result = strings.Join(resp.Parts, "\n\n")
		} else {
			result = resp.Text
		}
		if !outcome.Response.Result.Handled {
			result = "Unknown command: " + cmdText
		}
	} else {
		result = "Unknown command: " + cmdText
	}

	display := ConvertToTelegramHTML(result, b.tableOpts())
	if len(display) > 4096 {
		display = display[:4090] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	// If the response includes a keyboard, edit with both text and keyboard.
	if len(resp.Keyboard) > 0 {
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(cmdText, "/"), " ")
		rows := buildButtonRows(dispatch.CmdButtons(cmdName, resp.Keyboard), "cmd:")
		_, _, err := b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
			ChatId:      chatID,
			MessageId:   msgID,
			ParseMode:   "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
		})
		if err != nil {
			b.logger().Debugf("command callback HTML+keyboard edit failed: %v, retrying as plain text", err)
			_, _, _ = b.client.EditMessageText(result, &gotgbot.EditMessageTextOpts{
				ChatId:      chatID,
				MessageId:   msgID,
				ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
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
	rows := buildButtonRows(dispatch.CmdButtons(parentName, opts), "cmd:")
	_, _, _ = b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:      chatID,
		MessageId:   msgID,
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
}

// handleToolCallCallback handles tool call expand/collapse button presses.
func (b *Bot) handleToolCallCallback(chatID int64, action string, msgID int64) {
	key := strconv.FormatInt(msgID, 10)
	stored, ok := b.toolStore.Load(key)
	if !ok {
		return
	}

	switch action {
	case "show":
		var expanded string
		if stored.Result == "" {
			// Tool still running — show params with placeholder.
			expanded = formatToolCallWithResult(stored.FullInput, "⏳ Running...")
		} else {
			expanded = formatToolCallWithResult(stored.FullInput, stored.Result)
		}
		// Mark as expanded so ToolResultObserver can update when result arrives.
		stored.Expanded = true
		b.toolStore.Update(key, stored)
		rows := buildButtonRows([]platform.ButtonChoice{{Label: "Hide", Data: "hide"}}, "tc:")
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
		})
	case "hide":
		stored.Expanded = false
		b.toolStore.Update(key, stored)
		rows := buildButtonRows([]platform.ButtonChoice{{Label: "Show full", Data: "show"}}, "tc:")
		_, _, _ = b.client.EditMessageText(stored.CompactText, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: rows,
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
		expanded := formatThinkingExpanded(entry.thinkingText, entry.responseHTML, b.resolveDisplay(b.sessionKeyForMsg(chatID)).DisplayWidth)
		rows := buildButtonRows([]platform.ButtonChoice{{Label: "Hide thinking", Data: "hide"}}, "th:")
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
		})
	case "hide":
		rows := buildButtonRows([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
		_, _, _ = b.client.EditMessageText(entry.responseHTML, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
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
		// Preserve the END of the thinking, not the start (#720): the tail (the
		// conclusion/answer) is what's worth reading; drop the earlier reasoning.
		escaped := "... (truncated)\n" + truncateHTMLSafeTail(htmlEscape(thinkingText), budget)
		result = "<i>" + escaped + "</i>" + divider + responseHTML
	}
	return result
}

// truncateHTMLSafeTail keeps the LAST maxLen bytes of HTML-escaped text without
// leaving a dangling entity fragment at the cut. If the kept prefix begins inside
// an entity (a ';' appears before any '&'), it drops up to and including that ';'.
func truncateHTMLSafeTail(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	s = s[len(s)-maxLen:]
	amp := strings.IndexByte(s, '&')
	semi := strings.IndexByte(s, ';')
	if semi != -1 && (amp == -1 || semi < amp) {
		s = s[semi+1:]
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
	return redactToken(err.Error(), b.botToken)
}
