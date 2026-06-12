package telegram

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// makeCallbackQuery builds a Telegram callback query against chat 12345.
func makeCallbackQuery(msgID int64, data string) *gotgbot.CallbackQuery {
	return &gotgbot.CallbackQuery{
		Id:   "cq1",
		Data: data,
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
	}
}

func TestHandleThinkingCallback_ShowAndHide(t *testing.T) {
	// Proves the thinking toggle: "show" edits the message to thinking +
	// divider + response with a "Hide thinking" button, and "hide" restores
	// the bare response with a "Show thinking" button.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.display.DisplayWidth = 10
	b.thinkingStore.Store(int64(100), thinkingEntry{
		responseHTML: "the answer",
		thinkingText: "the reasoning",
	})

	b.handleThinkingCallback(12345, "show", 100)
	if mock.editCount() != 1 {
		t.Fatalf("edits = %d, want 1", mock.editCount())
	}
	if !strings.Contains(mock.lastEditText, "the reasoning") || !strings.Contains(mock.lastEditText, "the answer") {
		t.Errorf("expanded text missing parts: %q", mock.lastEditText)
	}
	if btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]; btn.Text != "Hide thinking" || btn.CallbackData != "th:hide" {
		t.Errorf("button = %q/%q, want Hide thinking/th:hide", btn.Text, btn.CallbackData)
	}

	b.handleThinkingCallback(12345, "hide", 100)
	if mock.editCount() != 2 {
		t.Fatalf("edits = %d, want 2", mock.editCount())
	}
	if mock.lastEditText != "the answer" {
		t.Errorf("collapsed text = %q, want bare response", mock.lastEditText)
	}
	if btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]; btn.Text != "Show thinking" || btn.CallbackData != "th:show" {
		t.Errorf("button = %q/%q, want Show thinking/th:show", btn.Text, btn.CallbackData)
	}
}

func TestHandleThinkingCallback_UnknownMessage(t *testing.T) {
	// Proves a thinking callback for an unstored message ID is ignored
	// (no edit, no panic) — e.g. after a restart cleared the ephemeral store.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.handleThinkingCallback(12345, "show", 999)
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
}

func TestFormatThinkingExpanded_TruncatesLongThinking(t *testing.T) {
	// Proves over-long thinking text is truncated (with marker) so the
	// combined message fits Telegram's 4096-char limit, keeping the full
	// response intact.
	long := strings.Repeat("t", 6000)
	got := formatThinkingExpanded(long, "response", 10)
	if len(got) > 4096 {
		t.Errorf("len = %d, want <= 4096", len(got))
	}
	if !strings.Contains(got, "... (truncated)") {
		t.Error("missing truncation marker")
	}
	if !strings.Contains(got, "response") {
		t.Error("response lost during truncation")
	}

	short := formatThinkingExpanded("brief", "response", 10)
	if strings.Contains(short, "truncated") {
		t.Error("short thinking should not be truncated")
	}
}

func TestTruncateHTMLSafe_EntityBoundary(t *testing.T) {
	// Proves truncation never cuts inside an HTML entity: a cut mid-entity
	// backs up to before the '&'.
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"under limit unchanged", "abc&amp;", 20, "abc&amp;"},
		{"cut mid-entity backs up", "abc&amp;def", 6, "abc"},
		{"cut after complete entity keeps it", "abc&amp;def", 9, "abc&amp;d"},
		{"no entities plain cut", "abcdef", 3, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateHTMLSafe(tt.in, tt.maxLen); got != tt.want {
				t.Errorf("truncateHTMLSafe(%q, %d) = %q, want %q", tt.in, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestHandleCallbackQuery_RoutesThinkingAndAnswers(t *testing.T) {
	// Proves handleCallbackQuery routes th:-prefixed callbacks to the
	// thinking handler and always answers the callback query to dismiss the
	// client-side loading state.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.thinkingStore.Store(int64(100), thinkingEntry{responseHTML: "r", thinkingText: "t"})

	b.handleCallbackQuery(context.Background(), makeCallbackQuery(100, "th:show"))

	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1 (thinking expanded)", mock.editCount())
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_EmptyDataIgnored(t *testing.T) {
	// Proves callbacks with no data are dropped before answering (nothing to
	// route, nothing to dismiss).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.handleCallbackQuery(context.Background(), makeCallbackQuery(100, ""))
	if mock.answerCBCalls != 0 || mock.editCount() != 0 {
		t.Errorf("answered=%d edits=%d, want 0/0", mock.answerCBCalls, mock.editCount())
	}
}

func TestHandleCommandCallback_ExecutesAndEditsResult(t *testing.T) {
	// Proves a command keyboard press executes the command and edits the
	// keyboard message to show the command's response.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})
	b, mock := testBot([]string{"111"}, cmds)

	b.handleCallbackQuery(context.Background(), makeCallbackQuery(55, "cmd:/ping"))

	if mock.editCount() != 1 {
		t.Fatalf("edits = %d, want 1", mock.editCount())
	}
	if !strings.Contains(mock.lastEditText, "pong") {
		t.Errorf("edit text = %q, want pong", mock.lastEditText)
	}
	if mock.lastEditOpts.MessageId != 55 {
		t.Errorf("edited msg = %d, want 55", mock.lastEditOpts.MessageId)
	}
}

func TestHandleCommandCallback_UnknownCommand(t *testing.T) {
	// Proves an unknown command callback edits the message to an "Unknown
	// command" notice instead of silently eating the press.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	b.handleCommandCallback(context.Background(), 12345, 55, "/nope")

	if mock.editCount() != 1 {
		t.Fatalf("edits = %d, want 1", mock.editCount())
	}
	if !strings.Contains(mock.lastEditText, "Unknown command") {
		t.Errorf("edit text = %q, want unknown-command notice", mock.lastEditText)
	}
}

func TestHandleCommandCallback_NilDispatcher(t *testing.T) {
	// Proves a command callback without a dispatcher is a safe no-op.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.dispatcher = nil
	b.handleCommandCallback(context.Background(), 12345, 55, "/ping")
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
}

func TestHandleCommandCallback_ResponseWithKeyboard(t *testing.T) {
	// Proves a command whose response carries keyboard options re-edits the
	// message with both the response text and a fresh inline keyboard.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "pick",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{
				Text:     "choose one",
				Keyboard: []command.KeyboardOption{{Label: "A", Data: "a"}, {Label: "B", Data: "b"}},
			}, nil
		},
	})
	b, mock := testBot([]string{"111"}, cmds)

	b.handleCommandCallback(context.Background(), 12345, 55, "/pick x")

	if mock.editCount() != 1 {
		t.Fatalf("edits = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard == nil {
		t.Fatal("expected keyboard on response edit")
	}
	if got := len(mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0]); got != 2 {
		t.Errorf("keyboard buttons = %d, want 2", got)
	}
}
