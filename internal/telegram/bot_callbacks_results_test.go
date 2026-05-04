package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestToolCallFull_InlineKeyboard(t *testing.T) {
	// Verifies that inline keyboards are properly
	// generated for tool calls in full mode.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.display.ShowToolCalls = "full"

	// Simulate the ToolCallObserver closure from processAgentMessage.
	var toolMsgMu sync.Mutex
	chatID := int64(12345)

	observer := func(toolName string, params json.RawMessage) {
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		compact := formatToolCallCompact(toolName, params)
		sendOpts := &gotgbot.SendMessageOpts{
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: "tc:show"},
				}},
			},
		}
		_, err := b.client.SendMessage(chatID, compact, sendOpts)
		if err != nil {
			return
		}
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.sends != 1 {
		t.Fatalf("expected 1 send, got %d", mock.sends)
	}
	if mock.lastSendOpts == nil {
		t.Fatal("expected SendMessageOpts to be set")
	}
	// No edit needed — callback data no longer contains message ID
	if mock.edits != 0 {
		t.Fatalf("expected 0 edits, got %d", mock.edits)
	}
	kb := mock.lastSendOpts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatal("expected 1x1 inline keyboard")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show full")
	}
	if btn.CallbackData != "tc:show" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "tc:show")
	}
	// Sent text should be compact (no <pre> block)
	if strings.Contains(mock.lastSendInjected, "<pre>") {
		t.Errorf("compact summary should not contain <pre> block, got: %s", mock.lastSendInjected)
	}
}

func TestHandleCallbackQuery_Show(t *testing.T) {
	// Verifies that callback queries expand compact
	// tool call summaries to show full details.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Pre-store a tool result with compact text, full input, and result.
	var msgID int64 = 42
	compactText := `▶️ <b>shell</b>: ls`
	fullInput := "▶️ <b>shell</b>\n<pre>ls</pre>"
	b.toolResults.Store(msgID, toolResultEntry{
		compactText: compactText,
		fullInput:   fullInput,
		result:      "file1.txt\nfile2.txt",
	})

	cq := &gotgbot.CallbackQuery{
		Id:   "cq1",
		From: gotgbot.User{Id: 111},
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
		Data: "tc:show",
	}
	b.handleCallbackQuery(context.Background(), cq)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	if mock.answerCBCalls != 1 {
		t.Fatalf("expected 1 AnswerCallbackQuery call, got %d", mock.answerCBCalls)
	}
	// The edited text should contain the full input and result
	if !strings.Contains(mock.lastEditText, "Result:") {
		t.Errorf("expected edit text to contain result, got: %s", mock.lastEditText)
	}
	if !strings.Contains(mock.lastEditText, "shell") {
		t.Errorf("expected edit text to contain full tool input, got: %s", mock.lastEditText)
	}
	// Button should now be "Hide"
	if mock.lastEditOpts == nil {
		t.Fatal("expected edit opts to be set")
	}
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatal("expected 1x1 inline keyboard")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Hide" {
		t.Errorf("button text = %q, want %q", btn.Text, "Hide")
	}
	if btn.CallbackData != "tc:hide" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "tc:hide")
	}
}

func TestHandleCallbackQuery_Hide(t *testing.T) {
	// Verifies that callback queries collapse
	// expanded tool call summaries.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	var msgID int64 = 42
	compactText := `▶️ <b>exec</b>: ls`
	fullInput := "▶️ <b>exec</b>\n<pre>ls</pre>"
	b.toolResults.Store(msgID, toolResultEntry{
		compactText: compactText,
		fullInput:   fullInput,
		result:      "file1.txt\nfile2.txt",
	})

	cq := &gotgbot.CallbackQuery{
		Id:   "cq2",
		From: gotgbot.User{Id: 111},
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
		Data: "tc:hide",
	}
	b.handleCallbackQuery(context.Background(), cq)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	// The edited text should be the compact summary (collapsed)
	if mock.lastEditText != compactText {
		t.Errorf("expected compact text %q, got: %s", compactText, mock.lastEditText)
	}
	// Button should be "Show full"
	kb := mock.lastEditOpts.ReplyMarkup
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show full")
	}
}

func TestHandleCommandCallback_HTMLFallback(t *testing.T) {
	// Verifies that command callbacks
	// fall back to plain text when HTML parsing fails.
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "test",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "result with <bad> html & stuff"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, reg)

	// First EditMessageText call (HTML) fails, second (plain text) succeeds
	mock.editErr = fmt.Errorf("Bad Request: can't parse entities")
	mock.editErrOnce = true

	b.handleCommandCallback(context.Background(), 12345, 1, "/test")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Should have tried twice: once HTML, once plain text
	if mock.edits != 2 {
		t.Fatalf("expected 2 edits (HTML + fallback), got %d", mock.edits)
	}
	// The last edit should be plain text (no ParseMode)
	if mock.lastEditOpts != nil && mock.lastEditOpts.ParseMode != "" {
		t.Errorf("fallback edit should have no ParseMode, got %q", mock.lastEditOpts.ParseMode)
	}
}

func TestHandleCommandCallback_Chain(t *testing.T) {
	// Verifies that command callbacks can trigger
	// chained keyboard menus.
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "executed: " + req.Args}, nil
		},
		ChainKeyboard: func(ctx context.Context, subcommand string, cc command.CommandContext) []command.KeyboardOption {
			if subcommand == "kill" {
				return []command.KeyboardOption{
					{Label: "sess-a", Data: "kill sess-a"},
					{Label: "sess-b", Data: "kill sess-b"},
				}
			}
			return nil
		},
	})

	b, mock := testBot([]string{"111"}, reg)

	// Callback for "/tmux kill" (bare subcommand) should chain to a second keyboard
	b.handleCommandCallback(context.Background(), 12345, 1, "/tmux kill")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	if mock.lastEditOpts == nil {
		t.Fatal("expected edit opts")
	}

	// Should have inline keyboard with 2 buttons
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) == 0 {
		t.Fatal("expected inline keyboard rows")
	}
	totalButtons := 0
	for _, row := range kb.InlineKeyboard {
		totalButtons += len(row)
	}
	if totalButtons != 2 {
		t.Fatalf("expected 2 buttons, got %d", totalButtons)
	}

	// Check button data format
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "sess-a" {
		t.Errorf("button text = %q, want sess-a", btn.Text)
	}
	if btn.CallbackData != "cmd:/tmux kill sess-a" {
		t.Errorf("callback data = %q, want cmd:/tmux kill sess-a", btn.CallbackData)
	}

	// Message text should be the prompt
	if !strings.Contains(mock.lastEditText, "/tmux kill") {
		t.Errorf("edit text = %q, want to contain /tmux kill", mock.lastEditText)
	}
}

func TestToolResultObserver_StoresResult(t *testing.T) {
	// Verifies that tool results are stored
	// for later display.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.ShowToolCalls = "full"

	// Simulate what the ToolResultObserver closure does.
	var toolMsgID int64 = 99
	var toolMsgText = `▶️ <b>exec</b>: ls`
	var toolMsgFullText = "▶️ <b>exec</b>\n<pre>ls</pre>"
	var toolMsgMu sync.Mutex

	observer := func(toolName string, result string, isError bool) {
		if b.display.ShowToolCalls != "full" {
			return
		}
		toolMsgMu.Lock()
		msgID := toolMsgID
		compact := toolMsgText
		full := toolMsgFullText
		toolMsgMu.Unlock()
		if msgID == 0 {
			return
		}
		b.toolResults.Store(msgID, toolResultEntry{
			compactText: compact,
			fullInput:   full,
			result:      result,
		})
	}

	observer("shell", "file1.txt\nfile2.txt", false)

	val, ok := b.toolResults.Load(int64(99))
	if !ok {
		t.Fatal("expected tool result to be stored")
	}
	entry := val.(toolResultEntry)
	if entry.result != "file1.txt\nfile2.txt" {
		t.Errorf("result = %q, want %q", entry.result, "file1.txt\nfile2.txt")
	}
}

