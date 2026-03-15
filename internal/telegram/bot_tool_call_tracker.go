package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// toolCallTracker manages tool call visibility state during an agent turn.
// It encapsulates the mutable state shared between ToolCallObserver and
// ToolResultObserver callbacks (message ID, text snapshots, mutex).
type toolCallTracker struct {
	bot     *Bot
	chatID  int64
	display turnDisplay

	mu         sync.Mutex
	msgID      int64            // Telegram message ID of the current tool-call message
	text       string           // last compact summary HTML (full mode) or full HTML (preview mode)
	fullText   string           // last full formatted tool call HTML (full mode only)
	lastParams json.RawMessage  // params of the last tool call (for result hints)
	retryMsgID int64            // Telegram message ID of the retry notification message
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
	mode := t.display.showToolCalls
	if mode == "off" || mode == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if mode == "full" {
		t.sendFullModeToolCall(toolName, params)
		return
	}
	t.sendPreviewModeToolCall(toolName, params)
}

// sendFullModeToolCall sends a compact summary with a "Show full" button.
func (t *toolCallTracker) sendFullModeToolCall(toolName string, params json.RawMessage) {
	compact := formatToolCallCompact(toolName, params)
	full := t.bot.formatToolCall(toolName, params, t.display.showToolCalls)
	kb := singleButtonKeyboard("Show full", "tc:show")
	sent, err := t.bot.client.SendMessage(t.chatID, compact, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
	if err != nil {
		t.bot.logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.msgID = sent.MessageId
	t.text = compact
	t.fullText = full
	t.lastParams = params
	t.bot.toolResults.Store(t.msgID, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		chatID:      t.chatID,
	})
}

// sendPreviewModeToolCall sends or edits a tool call message (overwriting previous).
func (t *toolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := t.bot.formatToolCall(toolName, params, t.display.showToolCalls)
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

// cleanupPreview deletes the tool call preview message if one exists.
// Called when the final response is delivered via a separate message (streaming,
// thinking, or long response) so the transient tool call doesn't linger in chat.
func (t *toolCallTracker) cleanupPreview() {
	t.mu.Lock()
	id := t.msgID
	t.msgID = 0
	t.mu.Unlock()
	if id == 0 || t.display.showToolCalls != "preview" {
		return
	}
	_, _ = t.bot.client.DeleteMessage(t.chatID, id, nil)
}

// observeToolResult stores tool results for inline keyboard expansion (full mode only).
// When a result hint is available, the compact notification is updated inline
// (e.g. "☑️ todo: add" becomes "☑️ todo: add → #542").
func (t *toolCallTracker) observeToolResult(toolName string, result string, isError bool) {
	if t.display.showToolCalls != "full" {
		return
	}
	t.mu.Lock()
	msgID := t.msgID
	compact := t.text
	full := t.fullText
	params := t.lastParams
	t.mu.Unlock()
	if msgID == 0 {
		return
	}

	// Generate a result hint to append to the compact notification.
	hint := compactResultHint(toolName, params, result)
	if hint != "" {
		compact = compact + " → " + htmlEscape(hint)
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
		kb := singleButtonKeyboard("Hide", "tc:hide")
		_, _, _ = t.bot.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
		})
	} else if hint != "" {
		// Update the compact notification with the result hint.
		kb := singleButtonKeyboard("Show full", "tc:show")
		_, _, _ = t.bot.client.EditMessageText(compact, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: kb,
		})
	}
}

// notifyRetry sends a retry notification message on first API retry.
func (t *toolCallTracker) notifyRetry(endpoint string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Parse endpoint to extract a readable name
	endpointName := endpoint
	if strings.Contains(endpoint, "anthropic.com") {
		endpointName = "Anthropic API"
	} else if strings.Contains(endpoint, "openrouter") {
		endpointName = "OpenRouter"
	} else if strings.Contains(endpoint, "generativelanguage.googleapis.com") {
		endpointName = "Gemini API"
	}

	text := fmt.Sprintf("⏳ <i>%s is busy right now, retrying...</i>", endpointName)
	sent, err := t.bot.client.SendMessage(t.chatID, text, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
	})
	if err != nil {
		t.bot.logger().Debugf("send retry notification: %v", err)
		return
	}
	t.retryMsgID = sent.MessageId
}

// clearRetryNotification deletes or overwrites the retry notification on success.
func (t *toolCallTracker) clearRetryNotification() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.retryMsgID == 0 {
		return
	}

	// Overwrite with success message
	_, _, err := t.bot.client.EditMessageText("✓ <i>Request completed</i>", &gotgbot.EditMessageTextOpts{
		ChatId:    t.chatID,
		MessageId: t.retryMsgID,
		ParseMode: "HTML",
	})
	if err != nil {
		t.bot.logger().Debugf("clear retry notification: %v", err)
	}

	t.retryMsgID = 0
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

	escapedResult := htmlEscape(result)
	available := maxLen - overhead
	if len(escapedResult) > available {
		escapedResult = escapedResult[:available-3] + "..."
	}
	return toolText + separator + escapedResult + suffix
}
