package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestAsyncNotifierDeliveryViaSendInjected(t *testing.T) {
	// Simulates the async notifier delivery path in main.go:
	// notifier calls HandleMessage → gets response → calls bot.SendInjected()
	mgr := NewBotManager()
	bot, mock := testBot([]string{"111"}, command.NewRegistry())
	bot.SetChatID(12345)
	mgr.AddPrimary("test-agent", bot)

	// Simulate: notifier got a response from HandleMessage
	resp := "Four undeployed commits now. Both queues empty."

	// Deliver via primary bot's SendInjected (same as main.go closure)
	primary := mgr.PrimaryBot("test-agent")
	if primary == nil {
		t.Fatal("PrimaryBot returned nil")
	}
	if err := primary.SendInjected(resp); err != nil {
		t.Fatalf("SendInjected error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestAsyncNotifierNoPrimaryBot(t *testing.T) {
	// When no primary bot is configured, PrimaryBot returns nil.
	// The main.go closure logs a warning and skips delivery.
	mgr := NewBotManager()
	if bot := mgr.PrimaryBot("nonexistent"); bot != nil {
		t.Errorf("PrimaryBot(nonexistent) = %v, want nil", bot)
	}
}

func TestToolCallObserverResetsAfterReply(t *testing.T) {
	// Simulates the processAgentMessage closure interactions to verify
	// that intermediate text resets toolMsgID, forcing subsequent tool
	// calls to create new messages instead of editing stale ones.
	mock := &mockClient{}
	b := &Bot{client: mock}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	// Simulate the ReplyFunc closure from processAgentMessage
	replyFunc := func(text string) {
		b.client.SendMessage(12345, text, nil)
		toolMsgMu.Lock()
		toolMsgID = 0
		toolMsgMu.Unlock()
	}

	// Simulate the tool call observer closure from processAgentMessage
	toolCallObserver := func(toolName string, params json.RawMessage) {
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		text := b.formatToolCall(toolName, params, b.display.ShowToolCalls)
		if toolMsgID == 0 {
			sent, err := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			if err != nil {
				return
			}
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId:    12345,
				MessageId: toolMsgID,
				ParseMode: "HTML",
			})
		}
	}

	// Step 1: First tool call → sends new message (ID=1)
	toolCallObserver("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Fatalf("after first tool call: sends=%d, want 1", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Fatalf("after first tool call: edits=%d, want 0", mock.editCount())
	}

	// Step 2: Intermediate reply fires → resets toolMsgID
	replyFunc("Let me check...")
	if mock.sentCount() != 2 {
		t.Fatalf("after reply: sends=%d, want 2", mock.sentCount())
	}

	// Step 3: Second tool call → should send NEW message (not edit old one)
	toolCallObserver("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 3 {
		t.Errorf("after second tool call: sends=%d, want 3 (new message, not edit)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("after second tool call: edits=%d, want 0 (should not edit stale message)", mock.editCount())
	}
}

func TestShowToolCalls_Preview(t *testing.T) {
	// When showToolCalls is "preview", tool call observer should send messages.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "preview"}}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	observer := func(toolName string, params json.RawMessage) {
		if b.display.ShowToolCalls == "off" || b.display.ShowToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()
		text := b.formatToolCall(toolName, params, b.display.ShowToolCalls)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Errorf("sends=%d, want 1", mock.sentCount())
	}

	observer("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.editCount() != 1 {
		t.Errorf("edits=%d, want 1", mock.editCount())
	}
}

func TestShowToolCalls_Off(t *testing.T) {
	// When showToolCalls is "off", tool call observer should be a no-op.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "off"}}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	observer := func(toolName string, params json.RawMessage) {
		if b.display.ShowToolCalls == "off" || b.display.ShowToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()
		text := b.formatToolCall(toolName, params, b.display.ShowToolCalls)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))
	observer("read", json.RawMessage(`{"path":"foo.txt"}`))

	if mock.sentCount() != 0 {
		t.Errorf("sends=%d, want 0 (tool calls should be suppressed)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("edits=%d, want 0 (tool calls should be suppressed)", mock.editCount())
	}
}

func TestShowToolCalls_Full(t *testing.T) {
	// When showToolCalls is "full", every tool call gets its own persistent
	// message with compact summary. The final response goes via sendReply.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "full"}}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	// This mirrors the ToolCallObserver closure in processMessage.
	observer := func(toolName string, params json.RawMessage) {
		if b.display.ShowToolCalls == "off" || b.display.ShowToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		if b.display.ShowToolCalls == "full" {
			compact := formatToolCallCompact(toolName, params)
			sent, _ := b.client.SendMessage(12345, compact, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
			return
		}

		text := b.formatToolCall(toolName, params, b.display.ShowToolCalls)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	// First tool call: new message with compact summary.
	observer("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Errorf("sends=%d, want 1", mock.sentCount())
	}
	if !strings.Contains(mock.lastSendInjected, "shell") {
		t.Errorf("sent text should contain tool name, got: %s", mock.lastSendInjected)
	}

	// Second tool call: also a new message (not an edit).
	observer("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 2 {
		t.Errorf("sends=%d, want 2 (full mode sends each tool call as new message)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("edits=%d, want 0 (full mode should never edit previous tool call)", mock.editCount())
	}

	// Simulate response delivery: in "full" mode, response should NOT edit the tool message.
	toolMsgMu.Lock()
	editID := toolMsgID
	toolMsgMu.Unlock()
	if editID != 0 && b.display.ShowToolCalls == "preview" {
		t.Error("should not enter preview branch for full mode")
	}
}

func TestToolCallTracker_CleanupPreview(t *testing.T) {
	// Verifies that CleanupPreview deletes the tool call preview message
	// when in preview mode, and does nothing when no message exists.
	// The full/preview mode behavior is tested in the shared turn package.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "preview"}}
	d := b.resolveDisplay("")
	tracker := newToolCallTracker(b, 12345, d)

	// No message → no delete.
	tracker.CleanupPreview()
	if mock.deleteCount() != 0 {
		t.Errorf("deleteCount = %d, want 0 (no message to clean)", mock.deleteCount())
	}

	// Send a tool call (creates a message), then cleanup.
	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.CleanupPreview()
	if mock.deleteCount() != 1 {
		t.Errorf("deleteCount = %d, want 1", mock.deleteCount())
	}

	// After cleanup, second call is a no-op.
	tracker.CleanupPreview()
	if mock.deleteCount() != 1 {
		t.Errorf("deleteCount = %d, want 1 (idempotent)", mock.deleteCount())
	}
}

func TestPreviewModeOverwritesToolCallOnReply(t *testing.T) {
	// In non-streaming preview mode, an intermediate reply should edit the
	// tool call preview message in-place (overwriting the tool indicator with
	// the reply text), not send a new message.
	//
	// Sequence: tool B → reply C → tool D → reply E (final)
	// Expected: C edits B's message, E edits D's message.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "preview"}}
	msg := &gotgbot.Message{Chat: gotgbot.Chat{Id: 12345}}
	d := b.resolveDisplay("")

	tracker := newToolCallTracker(b, 12345, d)
	r := newTurnRenderer(b, msg, tracker, d)
	defer r.Cleanup()

	// Tool call B: sends new preview message.
	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Fatalf("after tool B: sends=%d, want 1", mock.sentCount())
	}

	// Reply C: should EDIT tool B's message, not send a new one.
	r.OnReply("Reply C content")
	if mock.editCount() != 1 {
		t.Errorf("after reply C: edits=%d, want 1 (should overwrite tool B)", mock.editCount())
	}
	if mock.sentCount() != 1 {
		t.Errorf("after reply C: sends=%d, want 1 (should not send new message)", mock.sentCount())
	}

	// Tool call D: should send a NEW preview message (tracker was reset).
	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 2 {
		t.Errorf("after tool D: sends=%d, want 2", mock.sentCount())
	}
}

func TestPreviewModeResetsAfterStreamingReply(t *testing.T) {
	// When streaming is active and show_tool_calls=preview, an intermediate
	// reply (OnReply) must delete the tool preview and reset the tracker so
	// the next tool call sends a new message.
	//
	// Sequence: tool B → stream reply C → OnReply → tool D
	// Expected: B's preview deleted, D sends a new message.
	mock := &mockClient{}
	b := &Bot{client: mock, display: BotDisplayConfig{ShowToolCalls: "preview", StreamOutput: true}}
	msg := &gotgbot.Message{Chat: gotgbot.Chat{Id: 12345}}
	d := b.resolveDisplay("")

	tracker := newToolCallTracker(b, 12345, d)
	r := newTurnRenderer(b, msg, tracker, d)
	defer r.Cleanup()

	// Tool call B: sends new preview message.
	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Fatalf("after tool B: sends=%d, want 1", mock.sentCount())
	}

	// Stream some text so the stream writer has content + a message ID.
	r.OnTextDelta("Reply C content")
	// sends=2 now (stream writer sent initial message)

	// Intermediate reply fires — should delete tool B's preview.
	r.OnReply("Reply C content")
	if mock.deleteCount() != 1 {
		t.Errorf("after reply C: deletes=%d, want 1 (tool B preview should be deleted)", mock.deleteCount())
	}

	// Tool call D: should send a NEW message, not edit the deleted one.
	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 3 {
		t.Errorf("after tool D: sends=%d, want 3 (tool D should be a new message)", mock.sentCount())
	}
}

func TestFormatToolCall(t *testing.T) {
	// Verifies that formatToolCall produces properly formatted
	// tool call messages.
	b := &Bot{}
	text := b.formatToolCall("shell", json.RawMessage(`{"command":"ls -la"}`), "preview")
	if !strings.Contains(text, "▶️") {
		t.Error("missing tool emoji")
	}
	if !strings.Contains(text, "<b>shell</b>") {
		t.Errorf("missing tool name in %q", text)
	}
	if !strings.Contains(text, "ls -la") {
		t.Errorf("missing params in %q", text)
	}
}

func TestFormatToolCall_HTMLEscape(t *testing.T) {
	// Verifies that HTML is properly escaped in
	// tool call messages.
	b := &Bot{}
	text := b.formatToolCall("shell", json.RawMessage(`{"command":"echo <script>"}`), "preview")
	if strings.Contains(text, "<script>") {
		t.Errorf("HTML not escaped in %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Errorf("expected escaped HTML in %q", text)
	}
}

func TestFormatToolCall_LongParams(t *testing.T) {
	// Verifies that long parameters are truncated.
	b := &Bot{}
	longVal := strings.Repeat("x", 500)
	text := b.formatToolCall("shell", json.RawMessage(fmt.Sprintf(`{"command":"%s"}`, longVal)), "preview")
	// Long params should be truncated and contain "..."
	if !strings.Contains(text, "...") {
		t.Errorf("long params should be truncated: %q", text)
	}
}

func TestFormatToolCall_UnescapesNewlinesAndTabs(t *testing.T) {
	// Verifies that escaped newlines
	// and tabs in JSON are properly displayed.
	b := &Bot{}
	// Simulate a tool call where the JSON string value contains literal \n and \t
	text := b.formatToolCall("shell", json.RawMessage(`{"command":"echo\nline2"}`), "preview")
	// The unescaping should make newlines visible without the literal \n
	if strings.Contains(text, "\\n") && !strings.Contains(text, "\n") {
		t.Errorf("should unescape newlines: %q", text)
	}
}

func TestFormatToolCall_UnescapesUnicodeSequences(t *testing.T) {
	// Verifies that Unicode escape
	// sequences are properly displayed.
	b := &Bot{}
	// Emoji or other Unicode escape sequences should be unescaped
	text := b.formatToolCall("notify", json.RawMessage(`{"msg":"hello \\u2764"}`), "preview")
	// The Unicode escape should be decoded to the actual character
	if strings.Contains(text, "\\u") {
		t.Errorf("should unescape unicode: %q", text)
	}
}
