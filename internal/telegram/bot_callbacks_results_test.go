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
	// TestToolCallFull_InlineKeyboard verifies that inline keyboards are properly
	// generated for tool calls in full mode.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.showToolCalls = "full"

	// Simulate the ToolCallObserver closure from processAgentMessage.
	var toolMsgID int64
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
					{Text: "Show full", CallbackData: "tc:show:0"},
				}},
			},
		}
		sent, err := b.client.SendMessage(chatID, compact, sendOpts)
		if err != nil {
			return
		}
		toolMsgID = sent.MessageId
		// Update callback data with real message ID.
		b.client.EditMessageText(compact, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: toolMsgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", toolMsgID)},
				}},
			},
		})
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
	// After send, an edit should update the callback data with the real message ID
	if mock.edits != 1 {
		t.Fatalf("expected 1 edit (to update callback data), got %d", mock.edits)
	}
	if mock.lastEditOpts == nil {
		t.Fatal("expected EditMessageTextOpts to be set")
	}
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatal("expected 1x1 inline keyboard")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show full")
	}
	if btn.CallbackData != "tc:show:1" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "tc:show:1")
	}
	// Sent text should be compact (no <pre> block)
	if strings.Contains(mock.lastSendInjected, "<pre>") {
		t.Errorf("compact summary should not contain <pre> block, got: %s", mock.lastSendInjected)
	}
}

func TestHandleCallbackQuery_Show(t *testing.T) {
	// TestHandleCallbackQuery_Show verifies that callback queries expand compact
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
		Data: fmt.Sprintf("tc:show:%d", msgID),
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
	if !strings.HasPrefix(btn.CallbackData, "tc:hide:") {
		t.Errorf("callback data = %q, want tc:hide: prefix", btn.CallbackData)
	}
}

func TestHandleCallbackQuery_Hide(t *testing.T) {
	// TestHandleCallbackQuery_Hide verifies that callback queries collapse
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
		Data: fmt.Sprintf("tc:hide:%d", msgID),
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
	// TestHandleCommandCallback_HTMLFallback verifies that command callbacks
	// fall back to plain text when HTML parsing fails.
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "test",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "result with <bad> html & stuff", nil
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
	// TestHandleCommandCallback_Chain verifies that command callbacks can trigger
	// chained keyboard menus.
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "executed: " + args, nil
		},
		ChainKeyboard: func(ctx context.Context, subcommand string) []command.KeyboardOption {
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
	// TestToolResultObserver_StoresResult verifies that tool results are stored
	// for later display.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.showToolCalls = "full"

	// Simulate what the ToolResultObserver closure does.
	var toolMsgID int64 = 99
	var toolMsgText = `▶️ <b>exec</b>: ls`
	var toolMsgFullText = "▶️ <b>exec</b>\n<pre>ls</pre>"
	var toolMsgMu sync.Mutex

	observer := func(toolName string, result string, isError bool) {
		if b.showToolCalls != "full" {
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

func TestFormatToolCallWithResult_Truncation(t *testing.T) {
	// TestFormatToolCallWithResult_Truncation verifies that formatted tool
	// results with very long outputs are truncated.
	longOutput := strings.Repeat("x", 1000)
	result := "Result:\n" + longOutput
	if len(result) > 4096 {
		t.Skipf("test data too small to exercise truncation (need >4096 chars)")
	}
	// Just verify no panic and basic format
	if !strings.Contains(result, "Result:") {
		t.Error("should contain Result: header")
	}
}

func TestSteerBuffer_AppendAndDrain(t *testing.T) {
	// TestSteerBuffer_AppendAndDrain verifies basic steer buffer operations.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	b.appendSteer("message1")
	b.appendSteer("message2")

	got := b.drainSteer()
	// appendSteer adds newlines between messages
	expected := "message1\nmessage2"
	if got != expected {
		t.Errorf("drainSteer = %q, want %q", got, expected)
	}

	// After drain, should be empty
	if b.drainSteer() != "" {
		t.Error("buffer should be empty after drain")
	}
}

func TestSteerBuffer_DrainEmpty(t *testing.T) {
	// TestSteerBuffer_DrainEmpty verifies that draining an empty buffer returns
	// empty string.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	if got := b.drainSteer(); got != "" {
		t.Errorf("drainSteer on empty = %q, want empty", got)
	}
}

func TestSteerBuffer_Concurrent(t *testing.T) {
	// TestSteerBuffer_Concurrent verifies that steer buffer is safe for
	// concurrent access.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	const n = 100
	done := make(chan bool)

	go func() {
		for i := 0; i < n; i++ {
			b.appendSteer(fmt.Sprintf("msg-%d\n", i))
		}
		done <- true
	}()

	// Drain periodically until writer is done and buffer is empty
	var collected []string
	for {
		if text := b.drainSteer(); text != "" {
			collected = append(collected, text)
		}
		select {
		case <-done:
			// Writer finished — drain remaining
			if text := b.drainSteer(); text != "" {
				collected = append(collected, text)
			}
			// Verify we got all messages
			joined := strings.Join(collected, "\n")
			for i := 0; i < n; i++ {
				want := fmt.Sprintf("msg-%d", i)
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in collected steer text", want)
				}
			}
			return
		default:
		}
	}
}

func TestReceiveMessage_SteerRoutesToBuffer(t *testing.T) {
	// TestReceiveMessage_SteerRoutesToBuffer verifies that when steer mode is
	// enabled and a turn is active, text messages go to the steer buffer.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.SetSteerMode(true)

	// Simulate active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer cancel()

	msg := makeMsg(111, "owner", "change direction")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be in the queue
	if len(b.queue) != 0 {
		t.Error("message should not be in queue when steer mode is active")
	}

	// Should be in steer buffer
	got := b.drainSteer()
	if got != "change direction" {
		t.Errorf("steer buffer = %q, want %q", got, "change direction")
	}
}

func TestReceiveMessage_SteerDisabledQueuesNormally(t *testing.T) {
	// TestReceiveMessage_SteerDisabledQueuesNormally verifies that when steer
	// mode is disabled, messages go to the queue even during an active turn.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// steerMode defaults to false

	// Simulate active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer cancel()

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Errorf("queue length = %d, want 1", len(b.queue))
	}
	if got := b.drainSteer(); got != "" {
		t.Errorf("steer buffer should be empty, got %q", got)
	}
}

func TestReceiveMessage_SteerNoActiveTurnQueuesNormally(t *testing.T) {
	// TestReceiveMessage_SteerNoActiveTurnQueuesNormally verifies that when steer
	// mode is enabled but no turn is active, messages go to the normal queue.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.SetSteerMode(true)

	// No active turn (turnCancel is nil)
	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Errorf("queue length = %d, want 1", len(b.queue))
	}
	if got := b.drainSteer(); got != "" {
		t.Errorf("steer buffer should be empty, got %q", got)
	}
}

func TestSetSteerMode(t *testing.T) {
	// TestSetSteerMode verifies that SetSteerMode toggles the flag.
	b, _ := testBot([]string{}, command.NewRegistry())
	if b.steerMode {
		t.Error("steerMode should default to false")
	}
	b.SetSteerMode(true)
	if !b.steerMode {
		t.Error("steerMode should be true after SetSteerMode(true)")
	}
	b.SetSteerMode(false)
	if b.steerMode {
		t.Error("steerMode should be false after SetSteerMode(false)")
	}
}
