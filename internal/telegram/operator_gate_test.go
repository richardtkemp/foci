package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

const opRefusal = " is only for this agent's operators."

// gateTestBot returns a test bot whose registry has tmux (with keyboard and
// chain) and status, whose allowlist admits 111 (operator) and 222
// (non-operator), and whose dispatcher runs on the telegram transport with an
// operator policy permitting exactly the named commands.
func gateTestBot(permitted []string) (*Bot, *mockClient, *int) {
	var runs int
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs++
			return command.Response{Text: "tmux ran"}, nil
		},
		KeyboardOptions: func(context.Context, command.CommandContext) []command.KeyboardOption {
			return []command.KeyboardOption{{Label: "kill", Data: "kill"}}
		},
		ChainKeyboard: func(_ context.Context, sub string, _ command.CommandContext) []command.KeyboardOption {
			if sub == "kill" {
				return []command.KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
			}
			return nil
		},
	})
	reg.Register(&command.Command{
		Name: "status",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs++
			return command.Response{Text: "ok"}, nil
		},
	})

	b, mock := testBot([]string{"111", "222"}, reg)
	cc := command.CommandContext{
		Operators: command.OperatorPolicies{
			command.SourceTelegram: command.NewOperatorPolicy(reg, "test", "telegram",
				config.OperatorAccess{Users: []string{"111"}, Commands: permitted}),
		},
	}
	b.SetCommandContext(cc)
	return b, mock, &runs
}

func gateCallbackQuery(userID int64, data string) *gotgbot.CallbackQuery {
	return &gotgbot.CallbackQuery{
		Id:   fmt.Sprintf("cq-%d-%s", userID, data),
		From: gotgbot.User{Id: userID},
		Message: gotgbot.Message{
			MessageId: 55,
			Chat:      gotgbot.Chat{Id: 12345},
		},
		Data: data,
	}
}

func TestTryIntercept_NonOperatorRefused(t *testing.T) {
	// Proves the polling-goroutine path: a typed forbidden command from a
	// restricted allowed user is answered with the refusal, executes nothing
	// and never reaches the agent queue.
	b, mock, runs := gateTestBot([]string{"status"})

	msg := makeMsg(222, "guest", "/tmux")
	msg.Date = int64(time.Now().Unix())
	consumed := b.tryIntercept(context.Background(), &queuedMessage{msg: msg, userID: "222", text: "/tmux"})
	if !consumed {
		t.Fatal("forbidden command must be consumed, not enqueued to the agent")
	}
	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	if mock.sentCount() != 1 || !strings.Contains(mock.lastSendInjected, "/tmux"+opRefusal) {
		t.Errorf("reply = %q, want the operator-only refusal", mock.lastSendInjected)
	}

	// The permitted command still runs for the same user.
	msg = makeMsg(222, "guest", "/status")
	msg.Date = int64(time.Now().Unix())
	b.tryIntercept(context.Background(), &queuedMessage{msg: msg, userID: "222", text: "/status"})
	if *runs != 1 {
		t.Errorf("permitted /status executed %d times, want 1", *runs)
	}
}

func TestProcessQueuedCommand_NonOperatorRefused(t *testing.T) {
	// Proves the deferred worker path: a forbidden command routed through
	// the command channel is refused the same way.
	b, mock, runs := gateTestBot([]string{"status"})

	qm := platform.QueuedMessage{
		UserID:   "222",
		Text:     "/tmux",
		ChatID:   12345,
		Original: makeMsg(222, "guest", "/tmux"),
	}
	b.processQueuedCommand(context.Background(), qm)

	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	if mock.sentCount() != 1 || !strings.Contains(mock.lastSendInjected, "/tmux"+opRefusal) {
		t.Errorf("reply = %q, want the operator-only refusal", mock.lastSendInjected)
	}
}

func TestHandleCallbackQuery_CommandPressChecksPresser(t *testing.T) {
	// Proves a cmd: button press is gated by who pressed it: the allowed
	// non-operator's press runs nothing and edits the pressed message to the
	// refusal (the press is still answered), while the operator's press on
	// the same button runs the command.
	b, mock, runs := gateTestBot([]string{"status"})

	b.handleCallbackQuery(context.Background(), gateCallbackQuery(222, "cmd:/tmux kill"))

	if *runs != 0 {
		t.Errorf("forbidden press executed the command %d times, want 0", *runs)
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("press answered %d times, want 1", mock.answerCBCalls)
	}
	if mock.edits != 1 || !strings.Contains(mock.lastEditText, "/tmux"+opRefusal) {
		t.Errorf("pressed message edited to %q, want the operator-only refusal", mock.lastEditText)
	}

	// The operator's press on the same chained button gets the chain
	// keyboard — the data listing the non-operator was refused.
	b.handleCallbackQuery(context.Background(), gateCallbackQuery(111, "cmd:/tmux kill"))
	if *runs != 0 {
		t.Errorf("operator chain press executed the command %d times, want the chain keyboard, not a run", *runs)
	}
	if mock.lastEditOpts == nil || len(mock.lastEditOpts.ReplyMarkup.InlineKeyboard) == 0 {
		t.Error("operator press must edit the message with the chained keyboard")
	}
	if got := mock.lastEditText; !strings.HasPrefix(got, "/tmux kill") {
		t.Errorf("operator press edited to %q, want the chained keyboard label", got)
	}

	// The operator's press of a plain command runs it.
	b.handleCallbackQuery(context.Background(), gateCallbackQuery(111, "cmd:/status"))
	if *runs != 1 {
		t.Errorf("operator press executed the command %d times, want 1", *runs)
	}
}

func TestHandleCallbackQuery_ForbiddenChainStepNotShown(t *testing.T) {
	// Proves a forbidden chained keyboard step shows no data listing: the
	// non-operator's press of "cmd:/tmux kill" produces no second keyboard.
	b, mock, runs := gateTestBot([]string{"status"})

	b.handleCallbackQuery(context.Background(), gateCallbackQuery(222, "cmd:/tmux kill"))

	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	if mock.edits != 1 {
		t.Fatalf("edits = %d, want exactly the refusal edit", mock.edits)
	}
	if mock.lastEditOpts != nil && len(mock.lastEditOpts.ReplyMarkup.InlineKeyboard) != 0 {
		t.Error("refusal edit must carry no keyboard")
	}
	if !strings.Contains(mock.lastEditText, "/tmux"+opRefusal) {
		t.Errorf("edit = %q, want the refusal", mock.lastEditText)
	}
}

func TestHandleCallbackQuery_NoOperatorsPressRuns(t *testing.T) {
	// Characterises the unset case: with no operators configured a cmd:
	// press from any allowed user runs the command as before.
	var runs int
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs++
			return command.Response{Text: "tmux ran"}, nil
		},
		ChainKeyboard: func(_ context.Context, sub string, _ command.CommandContext) []command.KeyboardOption {
			if sub == "kill" {
				return []command.KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
			}
			return nil
		},
	})
	b, mock := testBot([]string{"222"}, reg)
	b.SetCommandContext(command.CommandContext{})

	b.handleCallbackQuery(context.Background(), gateCallbackQuery(222, "cmd:/tmux kill"))

	if runs != 0 {
		t.Errorf("chain press executed %d times, want the chain keyboard instead", runs)
	}
	if mock.edits != 1 {
		t.Fatalf("edits = %d, want the chain-keyboard edit", mock.edits)
	}
	if mock.lastEditOpts == nil || len(mock.lastEditOpts.ReplyMarkup.InlineKeyboard) == 0 {
		t.Error("expected the chained keyboard on the edit")
	}
}

func TestSetupAgent_OperatorGateCoversPrimaryAndFacetBots(t *testing.T) {
	// Proves the policy riding in CommandContext covers both the primary
	// bot and a facet bot wired through ConfigureFacetConn: the facet's
	// dispatcher refuses the same command for the same non-operator.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "bot-" + token}, BotClient: &fakeBotClient{}}, nil
	})

	p := setupAgentFixture(t)
	var runs int
	p.Commands.Register(&command.Command{
		Name: "tmux",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs++
			return command.Response{Text: "tmux ran"}, nil
		},
	})
	p.CommandContext.Operators = command.OperatorPolicies{
		command.SourceTelegram: command.NewOperatorPolicy(p.Commands, "scout", "telegram",
			config.OperatorAccess{Users: []string{"111"}, Commands: []string{"status"}}),
	}

	mgr := NewBotManager()
	res := SetupAgent(mgr, p)
	if res == nil {
		t.Fatal("SetupAgent returned nil")
	}
	primary := mgr.RegisteredPrimary("scout")
	if primary == nil {
		t.Fatal("primary bot not registered")
	}

	var facet *Bot
	if pool := mgr.Pool("scout"); pool != nil {
		pool.ForEach(func(b *Bot) { facet = b })
	}
	if facet == nil {
		t.Fatal("facet bot not registered")
	}
	res.ConfigureFacetConn(facet)

	for name, bot := range map[string]*Bot{"primary": primary, "facet": facet} {
		outcome := bot.dispatcher.DispatchCommand(context.Background(), "/tmux", 12345, "222")
		if outcome.NotHandled || outcome.Response == nil ||
			outcome.Response.Result.Response.Text != "/tmux"+opRefusal {
			t.Errorf("%s bot outcome = %+v, want the refusal", name, outcome)
		}
		if outcome.Keyboard != nil {
			t.Errorf("%s bot must not show a keyboard for a forbidden command", name)
		}
	}
	if runs != 0 {
		t.Errorf("command executed %d times, want 0", runs)
	}
}

func TestSetCommandContext_RebuiltDispatcherStaysRestricted(t *testing.T) {
	// Proves rebuilding a bot's dispatcher (SetCommandContext runs on facet
	// claim, facet restore and any re-wire) keeps the gate: the policy rides
	// in the CommandContext, not the dispatcher.
	b, _, runs := gateTestBot([]string{"status"})
	cc := b.cmdCtx

	b.SetCommandContext(cc)
	outcome := b.dispatcher.DispatchCommand(context.Background(), "/tmux", 1, "222")
	if outcome.NotHandled || outcome.Response == nil || outcome.Response.Result.Response.Text != "/tmux"+opRefusal {
		t.Errorf("rebuilt dispatcher outcome = %+v, want the refusal", outcome)
	}
	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
}

func TestSetupAgent_OperatorStartupReport(t *testing.T) {
	// Proves bot setup logs one report line naming the agent, platform,
	// operator count and permitted commands when the agent has operators —
	// the CommandContext carrying the policy exactly as
	// registerAgentCommands builds it in production.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "bot-" + token}, BotClient: &fakeBotClient{}}, nil
	})

	p := setupAgentFixture(t)
	p.GlobalConfig.Platforms[0].Access.OperatorUsers = []string{"111"}
	// The default list names help, reset and stop; register them so every
	// entry resolves and none is reported as unknown.
	for _, name := range config.DefaultNonOperatorCommands() {
		p.Commands.Register(&command.Command{
			Name: name,
			Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
				return command.Response{Text: name}, nil
			},
		})
	}
	p.CommandContext.Operators = command.NewOperatorPolicies(p.Commands, p.GlobalConfig, p.AgentConfig)
	mgr := NewBotManager()

	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	if res := SetupAgent(mgr, p); res == nil {
		t.Fatal("SetupAgent returned nil")
	}

	out := buf.String()
	for _, want := range []string{`agent "scout"`, "telegram", "operators=1", "help, reset, stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup report %q missing %q", out, want)
		}
	}
}

func TestSetupAgent_NoOperatorsNoReportLine(t *testing.T) {
	// Characterises the unset case: no operator settings, no new log lines
	// at bot setup.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "bot-" + token}, BotClient: &fakeBotClient{}}, nil
	})

	p := setupAgentFixture(t)
	mgr := NewBotManager()

	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	if res := SetupAgent(mgr, p); res == nil {
		t.Fatal("SetupAgent returned nil")
	}
	if out := buf.String(); strings.Contains(out, "operators") {
		t.Errorf("unset operators produced a report line: %q", out)
	}
}

func TestRegisterCommands_MenuStaysFullWithOperators(t *testing.T) {
	// Characterises the bot-wide Telegram menu: it lists what the agent as a
	// whole can run and is not filtered by the operator gate, so an operator
	// configuring operators does not shrink everyone's autocomplete.
	b, mock, _ := gateTestBot([]string{"status"})
	b.RegisterCommands()

	if len(mock.setCmds) == 0 {
		t.Fatal("no commands registered with BotFather")
	}
	names := map[string]bool{}
	for _, c := range mock.setCmds {
		names[c.Command] = true
	}
	for _, want := range []string{"tmux", "status"} {
		if !names[want] {
			t.Errorf("menu missing /%s (menu is bot-wide): %+v", want, mock.setCmds)
		}
	}
}

// syncBuffer is a concurrency-safe io.Writer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
