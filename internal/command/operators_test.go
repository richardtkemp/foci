package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/log"
)

// opTestRegistry builds a registry with a handful of commands: tmux (with a
// keyboard and a chain), status, reset, help, mana (alias usage) and a hidden
// wait alias of stop — the shapes the gate must handle. The wait alias
// mirrors production registration (cmd/foci-gw): its own hidden entry,
// immediate like /stop, linked to /stop by AliasOf.
func opTestRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(&Command{
		Name: "tmux",
		Execute: func(context.Context, Request, CommandContext) (Response, error) {
			return Response{Text: "tmux ran"}, nil
		},
		KeyboardOptions: func(context.Context, CommandContext) []KeyboardOption {
			return []KeyboardOption{{Label: "kill", Data: "kill"}}
		},
		ChainKeyboard: func(_ context.Context, sub string, _ CommandContext) []KeyboardOption {
			if sub == "kill" {
				return []KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
			}
			return nil
		},
	})
	reg.Register(&Command{
		Name: "status",
		Execute: func(context.Context, Request, CommandContext) (Response, error) {
			return Response{Text: "ok"}, nil
		},
	})
	reg.Register(&Command{
		Name:    "mana",
		Aliases: []string{"usage"},
		Execute: func(context.Context, Request, CommandContext) (Response, error) {
			return Response{Text: "plan"}, nil
		},
	})
	stop := StopCommand()
	reg.Register(stop)
	reg.Register(&Command{Name: "wait", Hidden: true, Immediate: stop.Immediate, Execute: stop.Execute, AliasOf: stop})
	reg.Register(&Command{
		Name: "reset",
		Execute: func(context.Context, Request, CommandContext) (Response, error) {
			return Response{Text: "reset"}, nil
		},
	})
	reg.Register(HelpCommand(reg))
	return reg
}

// opTestPolicy builds a telegram policy for agent "scout" over the test
// registry.
func opTestPolicy(reg *Registry, users, commands []string) *OperatorPolicy {
	return NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{Users: users, Commands: commands})
}

func TestNewOperatorPolicyNormalisesCommandList(t *testing.T) {
	// Proves non_operator_commands entries match case-insensitively, ignore a
	// leading / and surrounding space, resolve an alias to its command (a
	// Command.Aliases key and an AliasOf entry alike), and collapse
	// duplicates to the one canonical set the report shows.
	reg := opTestRegistry()
	p := NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users:    []string{"111"},
		Commands: []string{"/TMUX", " Tmux ", "usage", "tmux", "Wait"},
	})

	// Every entry above names tmux, mana or stop; /status must be forbidden.
	if !p.Forbids("222", reg.Get("status"), "") {
		t.Error("non-operator may not run /status")
	}
	if p.Forbids("222", reg.Get("tmux"), "") {
		t.Error("/TMUX, ' Tmux ' and tmux must all permit tmux")
	}
	if p.Forbids("222", reg.Get("usage"), "") {
		t.Error("the alias entry 'usage' must permit mana via its alias")
	}
	if p.Forbids("222", reg.Get("mana"), "") {
		t.Error("'usage' names mana, so mana itself is permitted")
	}
	if p.Forbids("222", reg.Get("stop"), "") || p.Forbids("222", reg.Get("wait"), "") {
		t.Error("'Wait' names stop, so stop and its alias entry are permitted")
	}

	var lines []string
	p.LogReport(&capturingReportLogger{info: func(line string) { lines = append(lines, line) }}, "scout")
	if len(lines) != 1 || !strings.Contains(lines[0], "non-operators may run: mana, stop, tmux") {
		t.Errorf("report = %v, want one line listing the deduplicated canonical names mana, stop, tmux", lines)
	}
}

func TestOperatorPolicyForbids(t *testing.T) {
	// Proves the rule itself: nobody is restricted when no operators are
	// configured (whatever the command list says), an operator may run
	// everything, a non-operator exactly the permitted commands, an empty
	// user ID is restricted, an alias follows its command, and an explicitly
	// empty list forbids everything including /help.
	reg := opTestRegistry()

	if p := opTestPolicy(reg, nil, []string{"status"}); p.Forbids("222", reg.Get("status"), "") || p.Forbids("222", reg.Get("tmux"), "") {
		t.Error("with no operators configured nobody is restricted, whatever the command list says")
	}
	if p := opTestPolicy(reg, nil, []string{}); p.Forbids("222", reg.Get("help"), "") {
		t.Error("an empty command list must not restrict anyone when there are no operators")
	}

	op := opTestPolicy(reg, []string{"111"}, []string{"help", "reset", "stop"})
	if op.Forbids("111", reg.Get("tmux"), "") || op.Forbids("111", reg.Get("status"), "") {
		t.Error("an operator may run every command")
	}
	for _, forbidden := range []string{"tmux", "status", "mana"} {
		if !op.Forbids("222", reg.Get(forbidden), "") {
			t.Errorf("non-operator may not run /%s", forbidden)
		}
	}
	for _, permitted := range []string{"help", "reset", "stop", "wait"} {
		if op.Forbids("222", reg.Get(permitted), "") {
			t.Errorf("non-operator may run /%s (the wait alias follows stop)", permitted)
		}
	}
	if !op.Forbids("", reg.Get("status"), "") {
		t.Error("an empty user ID is restricted when operators exist")
	}

	none := opTestPolicy(reg, []string{"111"}, []string{})
	for _, name := range []string{"help", "reset", "stop", "wait", "status", "tmux"} {
		if !none.Forbids("222", reg.Get(name), "") {
			t.Errorf("explicitly empty list: /%s must be forbidden", name)
		}
	}
}

func TestOperatorPoliciesForbidsBySource(t *testing.T) {
	// Proves the policy map is keyed by transport: a telegram policy
	// restricts only telegram callers — discord, http, app and unidentified
	// callers have no entry and are never gated by it.
	reg := opTestRegistry()
	ops := OperatorPolicies{SourceTelegram: opTestPolicy(reg, []string{"111"}, []string{"status"})}

	if !ops.Forbids(Caller{Source: SourceTelegram, UserID: "222"}, reg.Get("tmux"), "") {
		t.Error("telegram caller must be restricted by the telegram policy")
	}
	for _, src := range []string{SourceDiscord, SourceHTTP, "app", ""} {
		if ops.Forbids(Caller{Source: src, UserID: "222"}, reg.Get("tmux"), "") {
			t.Errorf("source %q must not be gated by the telegram policy", src)
		}
	}
}

func TestOperatorPolicies_NoOperatorsNoEntry(t *testing.T) {
	// Characterises the unset case: an agent with no operators configured
	// anywhere gets no policy entry (and the map itself may be nil).
	reg := opTestRegistry()
	got := NewOperatorPolicies(reg, &config.Config{}, config.AgentConfig{ID: "scout"})
	if len(got) != 0 {
		t.Errorf("NewOperatorPolicies = %v, want no entries with no operators configured", got)
	}
}

func TestOperatorPolicyLogReport(t *testing.T) {
	// Proves the startup report: one INFO line naming the agent, platform,
	// operator count and the permitted commands ((none) for an empty set),
	// one WARN per unknown command-list entry, and silence from a nil policy.
	reg := opTestRegistry()
	p := NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users:    []string{"111", "222"},
		Commands: []string{"help", "reset", "stop", "tmx", "zz z"},
	})

	fl := fakeReportLogger{}
	p.LogReport(&fl, "scout")

	wantInfo := `agent "scout": telegram operators=2, non-operators may run: help, reset, stop`
	if len(fl.infos) != 1 || fl.infos[0] != wantInfo {
		t.Errorf("INFO lines = %q, want exactly [%q]", fl.infos, wantInfo)
	}
	wantWarns := []string{
		`agent "scout": telegram non_operator_commands: "tmx" names no registered command, ignored`,
		`agent "scout": telegram non_operator_commands: "zz z" names no registered command, ignored`,
	}
	if strings.Join(fl.warns, "\n") != strings.Join(wantWarns, "\n") {
		t.Errorf("WARN lines = %q, want %q", fl.warns, wantWarns)
	}

	// An explicitly empty permitted set prints (none).
	empty := fakeReportLogger{}
	NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users:    []string{"111"},
		Commands: []string{},
	}).LogReport(&empty, "scout")
	if len(empty.infos) != 1 || !strings.Contains(empty.infos[0], "(none)") {
		t.Errorf("empty set report = %q, want (none)", empty.infos)
	}

	// A nil policy is silent.
	silent := fakeReportLogger{}
	var nilPolicy *OperatorPolicy
	nilPolicy.LogReport(&silent, "scout")
	if len(silent.infos) != 0 || len(silent.warns) != 0 {
		t.Errorf("nil policy logged %q / %q, want silence", silent.infos, silent.warns)
	}
}

// The report logger contract is satisfied by the repo's component logger.
var _ OperatorReportLogger = (*log.ComponentLogger)(nil)

// fakeReportLogger captures LogReport output.
type fakeReportLogger struct {
	infos []string
	warns []string
}

func (f *fakeReportLogger) Infof(format string, args ...any) {
	f.infos = append(f.infos, fmt.Sprintf(format, args...))
}

func (f *fakeReportLogger) Warnf(format string, args ...any) {
	f.warns = append(f.warns, fmt.Sprintf(format, args...))
}

// capturingReportLogger hands each INFO line to a callback.
type capturingReportLogger struct {
	info func(line string)
}

func (c *capturingReportLogger) Infof(format string, args ...any) {
	c.info(fmt.Sprintf(format, args...))
}
func (c *capturingReportLogger) Warnf(string, ...any) {}

// subTestRegistry builds a registry with one auto-wired subcommand command,
// /cfg (get; set with alias "s"; a hidden "wipe"), plus /stop and its wait
// alias entry, for the subcommand-granularity tests (#2279).
func subTestRegistry() *Registry {
	reg := NewRegistry()
	run := func(text string) func(context.Context, Request, CommandContext) (Response, error) {
		return func(context.Context, Request, CommandContext) (Response, error) { return Response{Text: text}, nil }
	}
	reg.Register(&Command{
		Name: "cfg",
		Subcommands: []Subcommand{
			{Name: "get", Description: "read", Execute: run("got")},
			{Name: "set", Aliases: []string{"s"}, Description: "write", Execute: run("set")},
			{Name: "wipe", Hidden: true, Description: "erase", Execute: run("wiped")},
		},
		DefaultExecute: run("cfg bare"),
	})
	stop := StopCommand()
	reg.Register(stop)
	reg.Register(&Command{Name: "wait", Hidden: true, Immediate: stop.Immediate, Execute: stop.Execute, AliasOf: stop})
	reg.Register(HelpCommand(reg))
	return reg
}

func TestOperatorPolicy_SubcommandEntries(t *testing.T) {
	// Proves non_operator_commands entries can name subcommands (Dick
	// 2026-10-16: "Yes full control of subcommands"): a bare name permits
	// the command itself (and args that name no subcommand) but none of its
	// subcommands; "cmd sub" permits exactly that subcommand, matched by name
	// or alias in any case; an alias entry ("wait all") names its command's
	// subcommand; an entry naming no subcommand of the command is unknown.
	reg := subTestRegistry()
	p := NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users:    []string{"111"},
		Commands: []string{"cfg", "/CFG  S", "wait all", "cfg bogus", "cfg get x", "nope sub"},
	})
	cfg, stop, wait := reg.Get("cfg"), reg.Get("stop"), reg.Get("wait")

	for _, tc := range []struct {
		cmd    *Command
		args   string
		forbid bool
	}{
		{cfg, "", false},          // "cfg"
		{cfg, "whatever", false},  // args naming no subcommand invoke the command itself
		{cfg, "set a=b", false},   // "/CFG  S" names set via its alias
		{cfg, "S a=b", false},     // the alias, any case
		{cfg, "get", true},        // bare "cfg" does not permit a subcommand
		{cfg, "WIPE", true},       // nor a hidden one
		{stop, "all", false},      // "wait all" names stop's "all"
		{wait, "all", false},      // ... reached through the alias entry too
		{stop, "", true},          // but not bare /stop
		{stop, "subagents", true}, // nor another subcommand
	} {
		if got := p.Forbids("222", tc.cmd, tc.args); got != tc.forbid {
			t.Errorf("Forbids(222, /%s %s) = %v, want %v", tc.cmd.Name, tc.args, got, tc.forbid)
		}
		if p.Forbids("111", tc.cmd, tc.args) {
			t.Errorf("operator 111 forbidden /%s %s", tc.cmd.Name, tc.args)
		}
	}

	var rep fakeReportLogger
	p.LogReport(&rep, "scout")
	infos, warns := rep.infos, rep.warns
	if len(infos) != 1 || !strings.Contains(infos[0], "non-operators may run: cfg, cfg set, stop all") {
		t.Errorf("report = %v, want the permitted forms cfg, cfg set, stop all", infos)
	}
	if len(warns) != 3 {
		t.Errorf("warns = %v, want one each for \"cfg bogus\", \"cfg get x\" and \"nope sub\"", warns)
	}
}

func TestOperatorGate_SubcommandRefusalKeyboardAndHelp(t *testing.T) {
	// Proves the subcommand gate on every surface: Dispatch refuses the
	// forbidden form naming it ("/cfg get is only ..."); the bare keyboard
	// offers only permitted subcommands; /help still lists a command with
	// only a subcommand permitted, and /help <cmd> lists only permitted
	// subcommands; a command with no form permitted is hidden and refused.
	reg := subTestRegistry()
	cc := CommandContext{Operators: OperatorPolicies{SourceTelegram: NewOperatorPolicy(reg, "scout", "telegram",
		config.OperatorAccess{Users: []string{"111"}, Commands: []string{"cfg", "cfg set", "help", "stop all"}})}}
	restricted := Caller{Source: SourceTelegram, UserID: "222"}
	dispatch := func(name, args string) string {
		t.Helper()
		resp, _, err := reg.Dispatch(context.Background(),
			Request{Name: name, Args: args, Source: SourceTelegram, UserID: "222"}, cc)
		if err != nil {
			t.Fatalf("/%s %s: %v", name, args, err)
		}
		return resp.Text
	}

	if got := dispatch("cfg", "get"); got != "/cfg get is only for this agent's operators." {
		t.Errorf("/cfg get = %q, want the subcommand refusal", got)
	}
	if got := dispatch("cfg", "s x"); got != "set" {
		t.Errorf("/cfg s x = %q, want set to run", got)
	}
	if got := dispatch("stop", ""); got != "/stop is only for this agent's operators." {
		t.Errorf("bare /stop = %q, want the refusal", got)
	}

	_, _, opts, ok := reg.LookupKeyboard(context.Background(), "/cfg", restricted, cc)
	if !ok || len(opts) != 1 || opts[0].Data != "set" {
		t.Errorf("restricted /cfg keyboard = %v (ok=%v), want only the set button", opts, ok)
	}
	_, _, opts, _ = reg.LookupKeyboard(context.Background(), "/cfg", Caller{Source: SourceTelegram, UserID: "111"}, cc)
	if len(opts) != 2 {
		t.Errorf("operator /cfg keyboard = %v, want get and set", opts)
	}

	listing := dispatch("help", "")
	if !strings.Contains(listing, "/cfg") || !strings.Contains(listing, "/stop") {
		t.Errorf("/help must list /cfg and /stop (some form permitted):\n%s", listing)
	}
	detail := dispatch("help", "cfg")
	if !strings.Contains(detail, "| set") || strings.Contains(detail, "| get") || strings.Contains(detail, "| wipe") {
		t.Errorf("/help cfg must list only the permitted subcommand set:\n%s", detail)
	}
	if got := dispatch("help", "stop"); !strings.Contains(got, "| all") || strings.Contains(got, "| subagents") {
		t.Errorf("/help stop must list only the permitted all:\n%s", got)
	}

	cc.Operators[SourceTelegram] = NewOperatorPolicy(reg, "scout", "telegram",
		config.OperatorAccess{Users: []string{"111"}, Commands: []string{"help"}})
	if got := dispatch("help", "cfg"); got != "/cfg is only for this agent's operators." {
		t.Errorf("/help cfg with no cfg form permitted = %q, want the refusal", got)
	}
	if strings.Contains(dispatch("help", ""), "/cfg") {
		t.Error("/help must hide /cfg when no form of it is permitted")
	}
}

func TestOperatorPolicy_HandParsedSubcommandsDeclared(t *testing.T) {
	// Proves the subcommands that /agents, /todo and /display parse
	// themselves are declared, so non_operator_commands controls them like
	// any other (Dick 2026-10-16: "Yes full control of subcommands"; #2279
	// final review B2): a bare name permits the command but not the
	// subcommand, and a "cmd sub" entry resolves (no startup WARN) and
	// permits exactly that subcommand, by name or alias.
	reg := NewRegistry()
	reg.Register(AgentsCommand())
	reg.Register(TodoCommand())
	reg.Register(DisplayCommand())
	agents, todo, display := reg.Get("agents"), reg.Get("todo"), reg.Get("display")

	bare := NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users: []string{"111"}, Commands: []string{"agents", "todo", "display"},
	})
	for _, tc := range []struct {
		cmd  *Command
		args string
	}{
		{agents, "new"}, {agents, "NEW"},
		{todo, "new buy milk"}, {todo, "add buy milk"}, {todo, "rm 3"}, {todo, "done 3"},
		{display, "reset"}, {display, "show_thinking off"},
		// displayKeyAliases: /display parses stream and width itself (re-review N1).
		{display, "stream on"}, {display, "width 80"},
	} {
		if !bare.Forbids("222", tc.cmd, tc.args) {
			t.Errorf("bare-name list permits /%s %s, want it refused (a subcommand needs its own entry)", tc.cmd.Name, tc.args)
		}
	}
	for _, tc := range []struct {
		cmd  *Command
		args string
	}{{agents, ""}, {todo, ""}, {todo, "open t:x"}, {display, ""}} {
		if bare.Forbids("222", tc.cmd, tc.args) {
			t.Errorf("bare-name list refuses /%s %s, want the command itself permitted", tc.cmd.Name, tc.args)
		}
	}

	subs := NewOperatorPolicy(reg, "scout", "telegram", config.OperatorAccess{
		Users: []string{"111"}, Commands: []string{"agents new", "todo add", "display reset", "display stream"},
	})
	if len(subs.unknown) != 0 {
		t.Errorf("unknown entries = %v, want every subcommand entry resolved", subs.unknown)
	}
	for _, tc := range []struct {
		cmd    *Command
		args   string
		forbid bool
	}{
		{agents, "new", false}, {agents, "", true},
		{todo, "create buy milk", false}, {todo, "rm 3", true}, {todo, "", true},
		{display, "reset", false}, {display, "show_thinking off", true},
		{display, "stream on", false}, {display, "stream_output on", false}, {display, "width 80", true},
	} {
		if got := subs.Forbids("222", tc.cmd, tc.args); got != tc.forbid {
			t.Errorf("subcommand list: Forbids(/%s %s) = %v, want %v", tc.cmd.Name, tc.args, got, tc.forbid)
		}
	}
}
