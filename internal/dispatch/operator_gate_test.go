package dispatch

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/config"
)

// gateDispatcher builds a dispatcher for the telegram transport whose
// CommandContext carries an operator policy: operator 111, everyone else may
// run exactly the permitted commands. Commands report what ran through the
// shared runs recorder.
type opGateEnv struct {
	reg       *command.Registry
	d         *Dispatcher
	runs      map[string]int
	keyboards int
	chains    int
	lastUser  string // user ID of the last permitted command that ran
}

func newOpGateEnv(permitted []string) *opGateEnv {
	env := &opGateEnv{runs: map[string]int{}, reg: command.NewRegistry()}

	env.reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			env.runs["tmux"]++
			return command.Response{Text: "tmux ran"}, nil
		},
		KeyboardOptions: func(context.Context, command.CommandContext) []command.KeyboardOption {
			env.keyboards++
			return []command.KeyboardOption{{Label: "kill", Data: "kill"}}
		},
		ChainKeyboard: func(_ context.Context, sub string, _ command.CommandContext) []command.KeyboardOption {
			env.chains++
			if sub == "kill" {
				return []command.KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
			}
			return nil
		},
	})
	env.reg.Register(&command.Command{
		Name:    "status",
		Aliases: []string{"st"},
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			env.runs["status"]++
			env.lastUser = req.UserID
			return command.Response{Text: "ok"}, nil
		},
	})
	// The default permitted list names these three; register them so a
	// policy built from DefaultNonOperatorCommands can resolve every entry.
	env.reg.Register(command.StopCommand())
	// /reset with a declared "hard" subcommand, like the real one, so the
	// default list's subcommand granularity (#2279) is exercised. Neither
	// form is counted in runs: the default list permits bare /reset.
	env.reg.Register(&command.Command{
		Name: "reset",
		Subcommands: []command.Subcommand{{
			Name: "hard",
			Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
				env.runs["reset hard"]++
				return command.Response{Text: "hard reset done"}, nil
			},
		}},
		DefaultExecute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: "reset done"}, nil
		},
		KeyboardOptions: func(context.Context, command.CommandContext) []command.KeyboardOption { return nil },
	})
	env.reg.Register(command.HelpCommand(env.reg))

	policy := command.NewOperatorPolicy(env.reg, "scout", "telegram",
		config.OperatorAccess{Users: []string{"111"}, Commands: permitted})
	cc := command.CommandContext{Operators: command.OperatorPolicies{command.SourceTelegram: policy}}
	env.d = NewDispatcher(env.reg, cc, "scout", command.SourceTelegram)
	return env
}

func (e *opGateEnv) nothingRan(t *testing.T) {
	t.Helper()
	for name, n := range e.runs {
		t.Errorf("/%s executed %d times, want 0", name, n)
	}
	if e.keyboards != 0 {
		t.Errorf("KeyboardOptions ran %d times, want 0", e.keyboards)
	}
	if e.chains != 0 {
		t.Errorf("ChainKeyboard ran %d times, want 0", e.chains)
	}
}

func refusalText(name string) string {
	return "/" + name + " is only for this agent's operators."
}

func TestDispatchCommand_ForbiddenTypedCommandRefused(t *testing.T) {
	// Proves a typed forbidden command is refused on the full pipeline: no
	// keyboard, no execute, refusal reply — for the plain name, upper case,
	// an alias, and a bare command whose only interactive surface IS the
	// keyboard.
	for _, permitted := range [][]string{{"status"}, {}} {
		env := newOpGateEnv(permitted)
		for _, text := range []string{"/tmux", "/TMUX", "/tmux kill x"} {
			outcome := env.d.DispatchCommand(context.Background(), text, 1, "222")
			if outcome.NotHandled {
				t.Fatalf("%q must be handled (never reach the agent as text)", text)
			}
			if outcome.Keyboard != nil || outcome.Chain != nil {
				t.Errorf("%q produced a keyboard outcome, want a refusal", text)
				continue
			}
			if outcome.Response == nil {
				t.Fatalf("%q produced no response outcome", text)
			}
			if got := outcome.Response.Result.Response.Text; got != refusalText("tmux") {
				t.Errorf("%q reply = %q, want %q", text, got, refusalText("tmux"))
			}
		}
		env.nothingRan(t)
	}

	// The operator's typed commands all run under the same policy that
	// refused everyone above.
	env := newOpGateEnv([]string{"status"})
	opOutcome := env.d.DispatchCommand(context.Background(), "/tmux kill x", 1, "111")
	if opOutcome.NotHandled || opOutcome.Response == nil || opOutcome.Response.Result.Response.Text != "tmux ran" {
		t.Errorf("operator /tmux outcome = %+v, want it to run", opOutcome)
	}
	if env.runs["tmux"] != 1 {
		t.Errorf("/tmux ran %d times for the operator, want 1", env.runs["tmux"])
	}
}

func TestDispatchCommand_ForbiddenDotCommandRefused(t *testing.T) {
	// Proves dot-commands are gated like their slash form, in any case.
	env := newOpGateEnv([]string{"status"})
	for _, text := range []string{".tmux", ".TMUX"} {
		outcome := env.d.DispatchCommand(context.Background(), text, 1, "222")
		if outcome.NotHandled {
			t.Fatalf("%q must be handled", text)
		}
		if outcome.Response == nil {
			t.Errorf("%q produced %+v, want a response refusal", text, outcome)
			continue
		}
		if got := outcome.Response.Result.Response.Text; got != refusalText("tmux") {
			t.Errorf("%q reply = %q, want the refusal", text, got)
		}
	}
	env.nothingRan(t)
}

func TestDispatchCommand_LeadingSpaceDotCommandFindsKeyboard(t *testing.T) {
	// Proves leading whitespace can no longer make a dot-command skip its
	// keyboard: DispatchCommand classifies trimmed text, exactly like
	// DispatchText, so " .tmux" behaves as ".tmux" and shows the keyboard
	// instead of silently running the bare command.
	env := newOpGateEnv([]string{"status"})

	for _, text := range []string{"/tmux", ".tmux", " .tmux", "  .tmux ", "\t.tmux"} {
		outcome := env.d.DispatchCommand(context.Background(), text, 1, "111")
		if outcome.Keyboard == nil {
			t.Errorf("%q outcome = %+v, want the keyboard outcome (as for the trimmed form)", text, outcome)
		}
	}
	if env.runs["tmux"] != 0 {
		t.Errorf("bare /tmux ran %d times behind whitespace, want 0 (keyboard first)", env.runs["tmux"])
	}
	if env.keyboards == 0 {
		t.Error("KeyboardOptions never ran; every form above must show the keyboard")
	}
}

func TestDispatchCommand_ForbiddenChainKeyboardNotListed(t *testing.T) {
	// Proves a forbidden chained step (e.g. /tmux kill) lists no data: the
	// chain keyboard never runs and the press lands as a refusal.
	env := newOpGateEnv([]string{"status"})

	outcome := env.d.DispatchCommand(context.Background(), "/tmux kill", 1, "222")
	if outcome.Chain != nil {
		t.Fatal("forbidden chained command must not produce a chain keyboard")
	}
	if outcome.Response == nil {
		t.Fatal("forbidden chained command produced no response outcome")
	}
	if got := outcome.Response.Result.Response.Text; got != refusalText("tmux") {
		t.Errorf("reply = %q, want the refusal", got)
	}
	env.nothingRan(t)
}

func TestInterceptorTransformedCommandRefused(t *testing.T) {
	// Proves text a message transform rewrites into a command is gated too:
	// the transformed /tmux is refused and the agent never sees it.
	env := newOpGateEnv([]string{"status"})
	ic := &Interceptor{
		Commands:     env.reg,
		LastMsgStore: command.NewLastMessageStore(),
		Handler:      &fakeHandler{transform: func(string) string { return "/tmux" }},
		Dispatcher:   env.d,
	}

	res := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "panes please", UserID: "222", ChatID: 1})
	if !res.Consumed || res.Outcome == nil {
		t.Fatalf("transformed command must be consumed with an outcome, got %+v", res)
	}
	if res.Outcome.Response == nil {
		t.Fatalf("transformed command outcome = %+v, want a response outcome", res.Outcome)
	}
	if got := res.Outcome.Response.Result.Response.Text; got != refusalText("tmux") {
		t.Errorf("reply = %q, want the refusal", got)
	}
	env.nothingRan(t)
}

func TestDispatchCommandCallback_OperatorGate(t *testing.T) {
	// Proves a cmd: press is judged as the presser: a non-operator's press
	// of a chained step gets the refusal (no chain keyboard, nothing run),
	// and an operator's press gets the chain. The presser is only the GATE
	// identity — the dispatched request carries no user, exactly as before
	// the gate, so user-keyed commands (/repeat) answer for nobody.
	env := newOpGateEnv([]string{"status"})

	outcome := env.d.DispatchCommandCallback(context.Background(), 1, "/tmux kill", "222")
	if outcome.Chain != nil {
		t.Fatal("non-operator press must not produce a chain keyboard")
	}
	if outcome.Response == nil {
		t.Fatal("non-operator press produced no response outcome")
	}
	if got := outcome.Response.Result.Response.Text; got != refusalText("tmux") {
		t.Errorf("non-operator press reply = %q, want the refusal", got)
	}
	env.nothingRan(t)

	opOutcome := env.d.DispatchCommandCallback(context.Background(), 1, "/tmux kill", "111")
	if opOutcome.Chain == nil || len(opOutcome.Chain.Options) != 1 {
		t.Fatalf("operator press = %+v, want the chain keyboard", opOutcome)
	}
	if env.chains != 1 {
		t.Errorf("ChainKeyboard ran %d times for the operator, want 1", env.chains)
	}

	// A permitted command pressed by the restricted user runs, but not as
	// the presser: Request.UserID stays empty on the callback path, so
	// /repeat on a button keeps its "unable to determine user" answer.
	env.d.DispatchCommandCallback(context.Background(), 1, "/status", "222")
	if env.lastUser != "" {
		t.Errorf("pressed command ran as user %q, want no user identity", env.lastUser)
	}
}

func TestDispatchCommand_DefaultListRuns(t *testing.T) {
	// Proves the default permitted list (help, reset, stop) lets a
	// non-operator run each of those commands through the dispatcher — each
	// reaches its Execute — while anything else, and their subcommands
	// (/reset hard, /stop all), is still refused.
	env := newOpGateEnv(config.DefaultNonOperatorCommands())

	reply := func(text string) string {
		t.Helper()
		outcome := env.d.DispatchCommand(context.Background(), text, 1, "222")
		if outcome.NotHandled || outcome.Response == nil {
			t.Fatalf("%q outcome = %+v, want a handled response", text, outcome)
		}
		return outcome.Response.Result.Response.Text
	}

	if got := reply("/stop"); got != "Stopped." {
		t.Errorf("default-list /stop reply = %q, want the stop reply", got)
	}
	if got := reply("/reset"); got != "reset done" {
		t.Errorf("default-list /reset reply = %q, want the reset reply", got)
	}
	// /help's Execute renders the listing: exactly the permitted commands.
	got := reply("/help")
	for _, name := range []string{"/help", "/reset", "/stop"} {
		if !strings.Contains(got, name) {
			t.Errorf("default-list /help listing lacks %s:\n%s", name, got)
		}
	}
	for _, name := range []string{"/tmux", "/status"} {
		if strings.Contains(got, name) {
			t.Errorf("default-list /help listing offers forbidden %s:\n%s", name, got)
		}
	}

	// A bare name permits the command, not its subcommands.
	if got := reply("/reset hard"); got != refusalText("reset hard") {
		t.Errorf("/reset hard reply = %q, want %q", got, refusalText("reset hard"))
	}
	if got := reply("/stop all"); got != refusalText("stop all") {
		t.Errorf("/stop all reply = %q, want %q", got, refusalText("stop all"))
	}
	if env.runs["reset hard"] != 0 {
		t.Errorf("/reset hard ran %d times, want refused", env.runs["reset hard"])
	}

	outcome := env.d.DispatchCommand(context.Background(), "/status", 1, "222")

	outcome = env.d.DispatchCommand(context.Background(), "/status", 1, "222")
	if outcome.NotHandled || outcome.Response == nil {
		t.Fatal("/status outcome must be handled with a response")
	}
	if got := outcome.Response.Result.Response.Text; got != refusalText("status") {
		t.Errorf("/status reply = %q, want the refusal (not on the default list)", got)
	}
	if env.runs["status"] != 0 {
		t.Errorf("/status ran %d times, want refused", env.runs["status"])
	}
}

func TestDispatchCommand_NonCommandTextStillFallsThrough(t *testing.T) {
	// Characterises the unaffected paths: plain text and slash paths from a
	// restricted user are still NotHandled — they reach the agent.
	env := newOpGateEnv([]string{"status"})
	for _, text := range []string{"hello there", "/home/foci/x:12 - error"} {
		if outcome := env.d.DispatchCommand(context.Background(), text, 1, "222"); !outcome.NotHandled {
			t.Errorf("%q = %+v, want NotHandled (reaches the agent)", text, outcome)
		}
	}
}

func TestDispatchCommand_NoOperatorsKeyboardsAndRepliesUnchanged(t *testing.T) {
	// Characterises the unset case: with no operators configured the
	// dispatcher shows keyboards and runs commands exactly as before.
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
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
	d := NewDispatcher(reg, command.CommandContext{}, "scout", command.SourceTelegram)

	if outcome := d.DispatchCommand(context.Background(), "/tmux", 1, "222"); outcome.Keyboard == nil {
		t.Error("keyboard must still show with no operators configured")
	}
	if outcome := d.DispatchCommandCallback(context.Background(), 1, "/tmux kill", "222"); outcome.Chain == nil {
		t.Error("chain keyboard must still show with no operators configured")
	}
	if outcome := d.DispatchCommand(context.Background(), "/tmux kill x", 1, "222"); outcome.NotHandled || outcome.Response == nil {
		t.Error("command must still run with no operators configured")
	}
}

func TestDispatchCommandCallback_HelpJudgesThePresser(t *testing.T) {
	// Proves a pressed /help is judged as the presser, like every other
	// press (#2279 review F1): an operator's press lists and describes the
	// operator-only /tmux, a non-operator's press does not. The presser rides
	// on the Request (PressedBy), so /help reads the same identity the gate
	// did.
	env := newOpGateEnv([]string{"help"})
	press := func(text, user string) string {
		t.Helper()
		outcome := env.d.DispatchCommandCallback(context.Background(), 1, text, user)
		if outcome.Response == nil {
			t.Fatalf("press %q by %s = %+v, want a response", text, user, outcome)
		}
		return outcome.Response.Result.Response.Text
	}

	if got := press("/help", "111"); !strings.Contains(got, "/tmux") {
		t.Errorf("operator's pressed /help omits /tmux:\n%s", got)
	}
	if got := press("/help tmux", "111"); got == refusalText("tmux") || !strings.HasPrefix(got, "**/tmux**") {
		t.Errorf("operator's pressed /help tmux = %q, want the /tmux detail", got)
	}
	if got := press("/help", "222"); strings.Contains(got, "/tmux") {
		t.Errorf("non-operator's pressed /help lists /tmux:\n%s", got)
	}
	if got := press("/help tmux", "222"); got != refusalText("tmux") {
		t.Errorf("non-operator's pressed /help tmux = %q, want the refusal", got)
	}
}

func TestDispatchCommand_SubcommandEntries(t *testing.T) {
	// Proves non_operator_commands can name subcommands (Dick 2026-10-16: "Yes
	// full control of subcommands"): "reset hard" alone permits /reset hard
	// but not bare /reset, in any case and on the press path too.
	env := newOpGateEnv([]string{"RESET Hard"})

	outcome := env.d.DispatchCommand(context.Background(), "/reset HARD", 1, "222")
	if outcome.Response == nil || outcome.Response.Result.Response.Text != "hard reset done" {
		t.Errorf("/reset HARD outcome = %+v, want it to run", outcome)
	}
	outcome = env.d.DispatchCommandCallback(context.Background(), 1, "/reset hard", "222")
	if outcome.Response == nil || outcome.Response.Result.Response.Text != "hard reset done" {
		t.Errorf("pressed /reset hard outcome = %+v, want it to run", outcome)
	}
	outcome = env.d.DispatchCommand(context.Background(), "/reset", 1, "222")
	if outcome.Response == nil || outcome.Response.Result.Response.Text != refusalText("reset") {
		t.Errorf("bare /reset outcome = %+v, want the refusal (only the subcommand is permitted)", outcome)
	}
	if env.runs["reset hard"] != 2 {
		t.Errorf("/reset hard ran %d times, want 2", env.runs["reset hard"])
	}
}

func TestDispatchCommand_SubcommandAfterOtherWhitespaceRefused(t *testing.T) {
	// Proves the subcommand gate splits words exactly as subcommand dispatch
	// does (strings.Fields): a newline, tab or other whitespace after the
	// subcommand cannot pass "/reset hard" off as bare /reset — typed, dot
	// form, or pressed — under the default list (#2279 final review B1).
	// (Whitespace other than a space between the command and the subcommand
	// is not a bypass: the command name is cut at the first space, so
	// "/reset\thard" names no command at all.)
	env := newOpGateEnv(config.DefaultNonOperatorCommands())
	for _, text := range []string{
		"/reset hard\nplease",
		"/reset hard\tx",
		"/reset hard\r\nx",
		"/reset hard x",
		".reset hard\nx",
	} {
		outcome := env.d.DispatchCommand(context.Background(), text, 1, "222")
		if outcome.Response == nil || outcome.Response.Result.Response.Text != refusalText("reset hard") {
			t.Errorf("typed %q outcome = %+v, want %q", text, outcome, refusalText("reset hard"))
		}
		outcome = env.d.DispatchCommandCallback(context.Background(), 1, text, "222")
		if outcome.Response != nil && outcome.Response.Result.Response.Text == "hard reset done" {
			t.Errorf("pressed %q ran /reset hard", text)
		}
	}
	if env.runs["reset hard"] != 0 {
		t.Errorf("/reset hard ran %d times for a non-operator, want 0", env.runs["reset hard"])
	}
}
