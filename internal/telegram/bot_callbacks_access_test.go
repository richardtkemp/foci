package telegram

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// callbackQueryFrom builds a callback query pressed by userID against chat
// 12345 (the same chat makeCallbackQuery uses), so tests can pick the presser.
func callbackQueryFrom(userID int64, msgID int64, data string) *gotgbot.CallbackQuery {
	return &gotgbot.CallbackQuery{
		Id:   "cq-access",
		From: gotgbot.User{Id: userID},
		Data: data,
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
	}
}

// countingPing registers a /ping command that counts its executions and
// returns the counter, so tests can prove a dispatch did (or did not) happen.
func countingPing(cmds *command.Registry) *atomic.Int32 {
	var runs atomic.Int32
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			runs.Add(1)
			return command.Response{Text: "pong"}, nil
		},
	})
	return &runs
}

func TestHandleCallbackQuery_RejectedCommandNotDispatched(t *testing.T) {
	// Proves a command-keyboard press from a user outside the allowlist runs
	// no command and edits nothing, but is still answered so the presser's
	// spinner stops (#2276).
	cmds := command.NewRegistry()
	runs := countingPing(cmds)
	b, mock := testBot([]string{"111"}, cmds)

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 55, "cmd:/ping"))

	if got := runs.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1 (spinner must stop)", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_RejectedInteractivePromptStaysAnswerable(t *testing.T) {
	// Proves an im: press from a disallowed user neither fires the registered
	// prompt callback nor consumes its registration: the prompt stays live so
	// an allowed user can still answer it (#2276).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	var fired atomic.Bool
	platform.RestoreInteractiveCallback("tg-access-im", "", nil,
		[]platform.ButtonChoice{{Label: "Allow", Data: "allow"}},
		func(platform.ButtonChoice) string { fired.Store(true); return "✅ Allow" },
		nil, time.Now())

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 66, "im:tg-access-im:0"))
	if fired.Load() {
		t.Error("disallowed press fired the interactive callback")
	}
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0 after rejected press", mock.editCount())
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1 after rejected press", mock.answerCBCalls)
	}

	// The prompt is still registered: a press from the allowed user answers it.
	b.handleCallbackQuery(context.Background(), callbackQueryFrom(111, 66, "im:tg-access-im:0"))
	if !fired.Load() {
		t.Error("allowed press did not fire the interactive callback")
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1 after allowed press", mock.editCount())
	}
	if mock.answerCBCalls != 2 {
		t.Errorf("answered = %d, want 2 after allowed press", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_RejectedToolCallNoEdit(t *testing.T) {
	// Proves a tc:show press from a disallowed user expands nothing (#2276).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.toolStore.Update("42", turn.ToolResultEntry{CompactText: "c", FullInput: "full input", Result: "r"})

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 42, "tc:show"))

	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_RejectedThinkingAndSubagentNoEffect(t *testing.T) {
	// Proves the remaining button kinds — th: thinking toggles and sa:
	// subagent hide — are gated by the same top-of-handler check (#2276):
	// a disallowed presser edits nothing and deletes nothing.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.thinkingStore.Store(int64(70), thinkingEntry{responseHTML: "r", thinkingText: "t"})
	token := subagentToken(12345, "toolu-access")
	b.subagentStore.Store(token, &subagentGroup{chatID: 12345, msgIDs: []int64{71}})

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 70, "th:show"))
	if mock.editCount() != 0 {
		t.Errorf("th: edits = %d, want 0", mock.editCount())
	}

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 71, "sa:"+token))
	if mock.deleteCount() != 0 {
		t.Errorf("sa: deletes = %d, want 0", mock.deleteCount())
	}
	if mock.answerCBCalls != 2 {
		t.Errorf("answered = %d, want 2 (each press answered once)", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_NilMessageAnsweredNoPanic(t *testing.T) {
	// Proves a callback with no Message (a press on an inline-mode message,
	// where the MaybeInaccessibleMessage interface is nil) is answered and
	// dropped instead of panicking — for a rejected presser (gate answers
	// before Message is ever read) and an allowed one (#2276).
	for _, userID := range []int64{999, 111} {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.handleCallbackQuery(context.Background(), &gotgbot.CallbackQuery{
			Id:   "cq-nilmsg",
			From: gotgbot.User{Id: userID},
			Data: "tc:show",
		})
		if mock.answerCBCalls != 1 {
			t.Errorf("user %d: answered = %d, want 1", userID, mock.answerCBCalls)
		}
		if mock.editCount() != 0 {
			t.Errorf("user %d: edits = %d, want 0", userID, mock.editCount())
		}
	}
}

func TestHandleCallbackQuery_RejectedEmptyDataStillAnswered(t *testing.T) {
	// Proves the access gate runs before the empty-data drop: a rejected
	// presser's query is still answered even with degenerate data. An
	// allowed presser's empty-data press is answered too (pinned by
	// TestHandleCallbackQuery_EmptyDataIgnored) (#2276).
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 55, ""))

	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1", mock.answerCBCalls)
	}
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
}

func TestHandleCallbackQuery_RejectionLoggedAtWarn(t *testing.T) {
	// Proves a rejected press logs one WARN naming the presser, mirroring the
	// message-rejection log operators watch for strangers finding the bot
	// (#2276).
	var (
		mu      sync.Mutex
		entries []struct {
			level     log.Level
			component string
			msg       string
		}
	)
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, struct {
			level     log.Level
			component string
			msg       string
		}{level, component, msg})
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b, _ := testBot([]string{"111"}, command.NewRegistry())
	cq := callbackQueryFrom(999, 55, "cmd:/ping")
	cq.From.Username = "intruder"
	b.handleCallbackQuery(context.Background(), cq)

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, e := range entries {
		if e.level == log.WARN &&
			strings.HasPrefix(e.component, "telegram:") &&
			strings.HasPrefix(e.msg, "rejected callback from ") &&
			strings.Contains(e.msg, "999 (intruder)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WARN 'rejected callback from 999 (intruder)', got %+v", entries)
	}
}

func TestHandleCallbackQuery_OpenAccessAnyPresserWorks(t *testing.T) {
	// Characterises open-access installs (empty allowlist, allowed_users_only
	// false): any presser still works — the access gate must not close them.
	cmds := command.NewRegistry()
	runs := countingPing(cmds)
	b, _ := testBot(nil, cmds)

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 55, "cmd:/ping"))

	if got := runs.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
}

func TestHandleCallbackQuery_EmptyDataAnsweredWithQueryID(t *testing.T) {
	// Proves an ALLOWED presser's query is answered even when Data is empty:
	// the deferred answer must be installed before the empty-data drop, or
	// the presser's spinner runs until Telegram times it out (#2303). The
	// answer carries the query's own id and nothing is edited or run.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(111, 55, ""))

	if mock.answerCBCalls != 1 {
		t.Errorf("answered = %d, want 1", mock.answerCBCalls)
	}
	if mock.lastAnswerCBID != "cq-access" {
		t.Errorf("answered id = %q, want cq-access", mock.lastAnswerCBID)
	}
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
}

func TestHandleCallbackQuery_AnswersWithQueryID(t *testing.T) {
	// Proves every press — rejected or allowed — is answered with the
	// pressed query's own id, so Telegram dismisses the right spinner
	// (#2303, shipping the #2276 reviewer's answer-id check).
	cmds := command.NewRegistry()
	runs := countingPing(cmds)
	b, mock := testBot([]string{"111"}, cmds)

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 55, "cmd:/ping"))
	if mock.answerCBCalls != 1 || mock.lastAnswerCBID != "cq-access" {
		t.Errorf("after rejected press: answered=%d id=%q, want 1/cq-access", mock.answerCBCalls, mock.lastAnswerCBID)
	}

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(111, 55, "cmd:/ping"))
	if mock.answerCBCalls != 2 || mock.lastAnswerCBID != "cq-access" {
		t.Errorf("after allowed press: answered=%d id=%q, want 2/cq-access", mock.answerCBCalls, mock.lastAnswerCBID)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1 (allowed press only)", got)
	}
}

func TestHandleCallbackQuery_LockdownEmptyAllowlistBlocksAll(t *testing.T) {
	// Proves lockdown (empty allowlist with allowed_users_only true) blocks
	// every presser — even a user id that appears in no list — while still
	// answering each press (#2303, shipping the #2276 reviewer's lockdown
	// check for telegram).
	cmds := command.NewRegistry()
	runs := countingPing(cmds)
	b, mock := testBot(nil, cmds)
	b.allowedUsersOnly = true

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(111, 55, "cmd:/ping"))
	b.handleCallbackQuery(context.Background(), callbackQueryFrom(999, 55, "cmd:/ping"))

	if got := runs.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
	if mock.editCount() != 0 {
		t.Errorf("edits = %d, want 0", mock.editCount())
	}
	if mock.answerCBCalls != 2 {
		t.Errorf("answered = %d, want 2 (each press answered once)", mock.answerCBCalls)
	}
}

func TestHandleCallbackQuery_FacetThinkingUsesFacetSessionKey(t *testing.T) {
	// Proves a facet (secondary) bot's thinking toggle resolves display
	// overrides with the bot's override session key — the key its turns
	// render with — not the per-chat key of the chat the button was pressed
	// in (#2303).
	var (
		ovMu  sync.Mutex
		ovKey string
	)
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKeyDirect("facet:key")
	b.displayOverrideFn = func(sessionKey string) DisplayOverrides {
		ovMu.Lock()
		defer ovMu.Unlock()
		ovKey = sessionKey
		return DisplayOverrides{}
	}
	b.thinkingStore.Store(int64(100), thinkingEntry{responseHTML: "r", thinkingText: "t"})

	b.handleCallbackQuery(context.Background(), callbackQueryFrom(111, 100, "th:show"))

	ovMu.Lock()
	defer ovMu.Unlock()
	if ovKey != "facet:key" {
		t.Errorf("display overrides resolved for %q, want facet:key", ovKey)
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1 (thinking expanded)", mock.editCount())
	}
}
