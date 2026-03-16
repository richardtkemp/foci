package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// toolCallTracker manages tool call visibility state during an agent turn.
type toolCallTracker struct {
	bot       *Bot
	channelID string
	display   turnDisplay

	mu         sync.Mutex
	msgID      string          // Discord message ID of the current tool-call message
	text       string          // last compact summary (full mode) or full text (preview mode)
	fullText   string          // last full formatted tool call text (full mode only)
	lastParams json.RawMessage // params of the last tool call (for result hints)
	retryMsgID string          // Discord message ID of the retry notification message
}

// lastMsgID returns the current tool-call message ID (thread-safe).
func (t *toolCallTracker) lastMsgID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgID
}

// resetMsgID clears the tool-call message ID (e.g. after intermediate text).
func (t *toolCallTracker) resetMsgID() {
	t.mu.Lock()
	t.msgID = ""
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
	full := formatToolCallFull(toolName, params, t.display.showToolCalls, t.bot.display.ToolCallPreviewChars)
	buttons := singleButton("Show full", "tc:show")
	sent, err := t.bot.session.ChannelMessageSendComplex(t.channelID, &discordgo.MessageSend{
		Content:    compact,
		Components: buttons,
	})
	if err != nil {
		t.bot.logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.msgID = sent.ID
	t.text = compact
	t.fullText = full
	t.lastParams = params
	msgIDInt, _ := strconv.ParseInt(sent.ID, 10, 64)
	t.bot.toolResults.Store(msgIDInt, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		channelID:   t.channelID,
	})
}

// sendPreviewModeToolCall sends or edits a tool call message (overwriting previous).
func (t *toolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := formatToolCallFull(toolName, params, t.display.showToolCalls, t.bot.display.ToolCallPreviewChars)
	if t.msgID == "" {
		sent, err := t.bot.session.ChannelMessageSend(t.channelID, text)
		if err != nil {
			t.bot.logger().Debugf("send tool call msg: %v", err)
			return
		}
		t.msgID = sent.ID
		t.text = text
	} else {
		_, err := t.bot.session.ChannelMessageEdit(t.channelID, t.msgID, text)
		if err != nil {
			t.bot.logger().Debugf("edit tool call msg: %v", err)
		}
		t.text = text
	}
}

// cleanupPreview deletes the tool call preview message if one exists.
func (t *toolCallTracker) cleanupPreview() {
	t.mu.Lock()
	id := t.msgID
	t.msgID = ""
	t.mu.Unlock()
	if id == "" || t.display.showToolCalls != "preview" {
		return
	}
	_ = t.bot.session.ChannelMessageDelete(t.channelID, id)
}

// observeToolResult stores tool results for button expansion (full mode only).
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
	if msgID == "" {
		return
	}

	// Generate a result hint to append to the compact notification.
	hint := compactResultHint(toolName, params, result)
	if hint != "" {
		compact = compact + " -> " + hint
	}

	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	var wasExpanded bool
	if prev, ok := t.bot.toolResults.Load(msgIDInt); ok {
		entry := prev.(toolResultEntry)
		wasExpanded = entry.expanded
	}

	t.bot.toolResults.Store(msgIDInt, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		result:      result,
		expanded:    wasExpanded,
		channelID:   t.channelID,
	})
	if t.bot.toolDetailStore != nil {
		t.bot.toolDetailStore.Store(msgIDInt, compact, full, result)
	}

	if wasExpanded {
		expanded := formatToolCallWithResult(full, result)
		buttons := singleButton("Hide", "tc:hide")
		if len(expanded) > discordMaxChars {
			expanded = expanded[:discordMaxChars-4] + "\n..."
		}
		_, _ = t.bot.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    t.channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	} else if hint != "" {
		buttons := singleButton("Show full", "tc:show")
		_, _ = t.bot.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    t.channelID,
			ID:         msgID,
			Content:    &compact,
			Components: &buttons,
		})
	}
}

// notifyRetry sends a retry notification message on first API retry.
func (t *toolCallTracker) notifyRetry(endpoint string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	endpointName := endpoint
	if strings.Contains(endpoint, "anthropic.com") {
		endpointName = "Anthropic API"
	} else if strings.Contains(endpoint, "openrouter") {
		endpointName = "OpenRouter"
	} else if strings.Contains(endpoint, "generativelanguage.googleapis.com") {
		endpointName = "Gemini API"
	}

	text := fmt.Sprintf("*%s is busy right now, retrying...*", endpointName)
	sent, err := t.bot.session.ChannelMessageSend(t.channelID, text)
	if err != nil {
		t.bot.logger().Debugf("send retry notification: %v", err)
		return
	}
	t.retryMsgID = sent.ID
}

// clearRetryNotification deletes or overwrites the retry notification on success.
func (t *toolCallTracker) clearRetryNotification() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.retryMsgID == "" {
		return
	}

	text := "*Request completed*"
	_, err := t.bot.session.ChannelMessageEdit(t.channelID, t.retryMsgID, text)
	if err != nil {
		t.bot.logger().Debugf("clear retry notification: %v", err)
	}

	t.retryMsgID = ""
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

// toolEmoji maps tool names to per-tool display emoji.
var toolEmoji = map[string]string{
	"shell":                "> ",
	"web_fetch":            ">> ",
	"web_search":           "?? ",
	"http_request":         ">> ",
	"read":                 "[] ",
	"write":                "<> ",
	"edit":                 "/\\ ",
	"tmux":                 ":: ",
	"todo":                 "-- ",
	"send_message_to_user": ">> ",
	"memory_search":        "** ",
	"spawn":                "++ ",
	"scratchpad":           "// ",
	"send_to_session":      ">> ",
	"remind":               ".. ",
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
	summary := compactSummary(toolName, params)
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
	paramStr := string(params)
	var pretty bytes.Buffer
	if json.Indent(&pretty, params, "", "  ") == nil {
		paramStr = pretty.String()
	}
	if showMode != "full" && len(paramStr) > maxChars {
		paramStr = paramStr[:maxChars] + "..."
	}
	prefix := emojiForTool(toolName)
	return fmt.Sprintf("%s**%s**\n```json\n%s\n```", prefix, toolName, paramStr)
}

// compactSummary extracts the most meaningful param values for a compact display.
func compactSummary(toolName string, params json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return ""
	}

	str := func(key string) string {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return strings.TrimSpace(string(raw))
	}

	switch toolName {
	case "shell":
		return truncate(str("command"), 60)
	case "web_fetch":
		return truncate(str("url"), 80)
	case "web_search", "memory_search":
		return truncate(str("query"), 60)
	case "http_request":
		method := str("method")
		if method == "" {
			method = "GET"
		}
		return truncate(method+" "+str("url"), 80)
	case "read", "write", "edit":
		return truncate(str("path"), 80)
	case "tmux":
		op := str("operation")
		name := str("name")
		if op != "" && name != "" {
			return op + " " + name
		}
		if op != "" {
			return op
		}
		return truncate(str("name"), 60)
	case "todo", "scratchpad":
		return str("action")
	case "remind":
		return truncate(str("text"), 40)
	case "send_message_to_user":
		return truncate(str("text"), 40)
	case "spawn":
		return truncate(str("prompt"), 40)
	}

	// Fallback: use the first string-valued param
	for _, raw := range m {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return truncate(s, 60)
		}
	}
	return ""
}

// compactResultHint extracts a short hint from a tool result.
func compactResultHint(toolName string, params json.RawMessage, result string) string {
	switch toolName {
	case "todo":
		return todoResultHint(params, result)
	case "shell":
		if result == "" {
			return "(empty)"
		}
		lines := strings.Count(result, "\n") + 1
		if lines <= 3 {
			return ""
		}
		return fmt.Sprintf("%d lines", lines)
	case "write":
		if strings.HasPrefix(result, "Wrote ") {
			parts := strings.SplitN(result, " ", 4)
			if len(parts) >= 3 {
				return parts[1] + " " + parts[2]
			}
		}
	case "edit":
		if strings.HasPrefix(result, "Applied ") || strings.HasPrefix(result, "Edited ") {
			firstLine, _, _ := strings.Cut(result, "\n")
			return truncate(firstLine, 30)
		}
	case "spawn":
		if strings.HasPrefix(result, "Spawned ") {
			firstLine, _, _ := strings.Cut(result, "\n")
			return truncate(firstLine, 30)
		}
	}
	return ""
}

// todoResultHint extracts a compact hint from a todo tool result.
func todoResultHint(params json.RawMessage, result string) string {
	var p struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	firstLine, _, _ := strings.Cut(result, "\n")
	switch p.Action {
	case "add":
		// "Added #542 (medium)" -> "#542"
		if i := strings.Index(firstLine, "#"); i >= 0 {
			end := strings.IndexByte(firstLine[i:], ' ')
			if end > 0 {
				return firstLine[i : i+end]
			}
			return firstLine[i:]
		}
	case "list", "search":
		if strings.HasPrefix(firstLine, "No ") {
			return "0 items"
		}
		n := strings.Count(result, "\n") + 1
		if n == 1 {
			return "1 item"
		}
		return fmt.Sprintf("%d items", n)
	case "done", "delete", "update":
		return truncate(firstLine, 30)
	}
	return ""
}
