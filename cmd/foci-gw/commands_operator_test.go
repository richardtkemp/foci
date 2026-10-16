package main

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
)

// operatorRegParams builds the minimal cmdRegParams for an agent "a" with
// stop aliases enabled, mirroring what the config loader defaults produce.
func operatorRegParams(cfg *config.Config, acfg config.AgentConfig) cmdRegParams {
	return operatorRegParamsWithAliases(cfg, acfg, "stop", "wait")
}

// operatorRegParamsWithAliases is operatorRegParams with a custom alias list,
// for tests that pin how aliases interact with other registrations.
func operatorRegParamsWithAliases(cfg *config.Config, acfg config.AgentConfig, aliases ...string) cmdRegParams {
	return cmdRegParams{
		ag:   &agent.Agent{},
		acfg: acfg,
		cfg:  cfg,
		resolved: &config.ResolvedAgentConfig{Behavior: config.ResolvedBehavior{
			EnableStopAliases: true,
			StopAliases:       aliases,
		}},
	}
}

func TestRegisterAgentCommands_BuildsOperatorPolicies(t *testing.T) {
	// Proves registerAgentCommands resolves cc.Operators from both config
	// levels for both chat transports: global telegram operators, agent
	// discord operators, and the agent's command list replacing the global
	// one.
	cfg := &config.Config{Platforms: []config.PlatformConfig{
		{ID: "telegram", Access: config.AccessConfig{OperatorUsers: []string{"111"}}},
		{ID: "discord", Access: config.AccessConfig{NonOperatorCommands: []string{"ping"}}},
	}}
	acfg := config.AgentConfig{
		ID: "a",
		Platforms: []config.PlatformConfig{
			{ID: "discord", Access: config.AccessConfig{OperatorUsers: []string{"222"}}},
		},
	}

	cmds, cc := registerAgentCommands(operatorRegParams(cfg, acfg), command.NewLastMessageStore())

	tg, ok := cc.Operators[command.SourceTelegram]
	if !ok {
		t.Fatal("no telegram policy built despite global telegram operators")
	}
	if tg.Forbids("111", cmds.Get("status"), "") {
		t.Error("global telegram operator 111 must not be restricted")
	}
	if !tg.Forbids("222", cmds.Get("status"), "") {
		t.Error("non-operator 222 must be restricted on telegram")
	}

	dc, ok := cc.Operators[command.SourceDiscord]
	if !ok {
		t.Fatal("no discord policy built despite agent discord operators")
	}
	if dc.Forbids("222", cmds.Get("status"), "") {
		t.Error("agent discord operator 222 must not be restricted")
	}
	if !dc.Forbids("111", cmds.Get("status"), "") {
		t.Error("telegram-only operator 111 must be restricted on discord")
	}

	// The global non_operator_commands list applies where the agent sets none.
	if dc.Forbids("333", cmds.Get("ping"), "") {
		t.Error("ping is on the global permitted list, so a discord non-operator may run it")
	}
	if !dc.Forbids("333", cmds.Get("status"), "") {
		t.Error("tmux is not on the global permitted list, so a discord non-operator may not run it")
	}
}

func TestRegisterAgentCommands_NoOperatorsLeavesGateOff(t *testing.T) {
	// Characterises the unset case: with no operators configured anywhere,
	// cc.Operators is empty and nobody is restricted.
	cfg := &config.Config{Platforms: []config.PlatformConfig{
		{ID: "telegram", Access: config.AccessConfig{AllowedUsers: []string{"111"}, NonOperatorCommands: []string{"status"}}},
	}}
	acfg := config.AgentConfig{ID: "a"}

	cmds, cc := registerAgentCommands(operatorRegParams(cfg, acfg), command.NewLastMessageStore())

	if len(cc.Operators) != 0 {
		t.Errorf("cc.Operators = %v, want empty with no operators configured", cc.Operators)
	}
	if cc.Operators.Forbids(command.Caller{Source: command.SourceTelegram, UserID: "111"}, cmds.Get("status"), "") {
		t.Error("nobody may be restricted when no operators are configured, whatever the command list says")
	}
}

func TestRegisterAgentCommands_StopAliasKeepsBaseShape(t *testing.T) {
	// Proves a stop alias keeps its pre-#2279 shape — its own hidden
	// registry entry, Immediate like /stop (d98364901), its own /help line —
	// while AliasOf links it to /stop for the operator gate.
	cmds, cc := registerAgentCommands(operatorRegParams(&config.Config{}, config.AgentConfig{ID: "a"}),
		command.NewLastMessageStore())

	stop, wait := cmds.Get("stop"), cmds.Get("wait")
	if stop == nil || wait == nil {
		t.Fatalf("stop=%v wait=%v, want both registered", stop, wait)
	}
	if wait == stop {
		t.Fatal("/wait must be its own entry, not the stop command itself (base behaviour)")
	}
	if wait.Hidden != true {
		t.Error("/wait must stay hidden, as before")
	}
	if !cmds.IsImmediateText("/stop") || !cmds.IsImmediateText("/wait") {
		t.Error("/wait must be immediate like /stop: it stops the live turn, so it must interrupt it")
	}
	if wait.AliasOf != stop {
		t.Error("/wait must be AliasOf /stop so the gate judges it as /stop")
	}

	// /help describes each under its own name, and /stop carries no Aliases
	// line — the unset-case help text is byte-for-byte the pre-#2279 shape.
	helpFor := func(name string) string {
		req := command.Request{Name: "help", Args: name}
		resp, handled, err := cmds.Dispatch(context.Background(), req, cc)
		if err != nil || !handled {
			t.Fatalf("/help %s dispatch = (%q, %v, %v)", name, resp.Text, handled, err)
		}
		return resp.Text
	}
	if got := helpFor("wait"); !strings.HasPrefix(got, "**/wait**") || strings.Contains(got, "Aliases") {
		t.Errorf("/help wait = %q, want the alias's own line, no Aliases", got)
	}
	if got := helpFor("stop"); !strings.HasPrefix(got, "**/stop**") || strings.Contains(got, "Aliases") {
		t.Errorf("/help stop = %q, want no Aliases line", got)
	}
}

func TestRegisterCommands_StopAliasOverridesSameNamedCommand(t *testing.T) {
	// Proves an alias keeps its base precedence over a command of the same
	// name: aliases register last (Register is last-wins), so an alias named
	// "login" or "done" wins the registration and dispatching it stops the
	// agent instead of running that command.
	cmds, cc := registerAgentCommands(
		operatorRegParamsWithAliases(&config.Config{}, config.AgentConfig{ID: "a"}, "stop", "wait", "login", "done"),
		command.NewLastMessageStore())

	stop := cmds.Get("stop")
	for _, name := range []string{"wait", "login", "done"} {
		if got := cmds.Get(name); got == nil || got.AliasOf != stop {
			t.Errorf("/%s must be an alias entry of /stop, got %v", name, got)
		}
	}
	for _, name := range []string{"login", "done"} {
		req := command.Request{Name: name, Source: command.SourceTelegram, UserID: "999"}
		resp, handled, err := cmds.Dispatch(context.Background(), req, cc)
		if err != nil || !handled {
			t.Fatalf("/%s dispatch = (%q, %v, %v), want it handled", name, resp.Text, handled, err)
		}
		if resp.Text != "Stopped." {
			t.Errorf("/%s reply = %q, want the stop reply", name, resp.Text)
		}
	}
}

func TestDispatch_StopAliasFollowsStopPermission(t *testing.T) {
	// Proves a non-operator may run the stop alias /wait iff /stop is
	// permitted — an alias names its command, it cannot smuggle past the
	// gate nor be caught by a list that names only its parent. The refusal
	// names the canonical /stop whichever name was typed.
	cmds, cc := registerAgentCommands(operatorRegParams(&config.Config{}, config.AgentConfig{ID: "a"}),
		command.NewLastMessageStore())
	cc.Operators = command.OperatorPolicies{
		command.SourceTelegram: command.NewOperatorPolicy(cmds, "a", "telegram",
			config.OperatorAccess{Users: []string{"111"}, Commands: []string{"wait"}}),
	}

	for _, name := range []string{"stop", "wait"} {
		req := command.Request{Name: name, Source: command.SourceTelegram, UserID: "222"}
		resp, handled, _ := cmds.Dispatch(context.Background(), req, cc)
		if !handled {
			t.Fatalf("/%s must be handled", name)
		}
		if isOperatorRefusal(resp.Text) {
			t.Errorf("/%s refused for a non-operator despite /stop being permitted (via the alias entry): %q", name, resp.Text)
		}
	}

	cc.Operators = command.OperatorPolicies{
		command.SourceTelegram: command.NewOperatorPolicy(cmds, "a", "telegram",
			config.OperatorAccess{Users: []string{"111"}, Commands: []string{"status"}}),
	}
	for _, name := range []string{"stop", "wait"} {
		req := command.Request{Name: name, Source: command.SourceTelegram, UserID: "222"}
		resp, _, _ := cmds.Dispatch(context.Background(), req, cc)
		if resp.Text != "/stop is only for this agent's operators." {
			t.Errorf("/%s refusal = %q, want the canonical /stop refusal", name, resp.Text)
		}
	}
}

func TestRunAgentCommand_NotOperatorGated(t *testing.T) {
	// Characterises the exempt surfaces: runAgentCommand — the dispatch core
	// of /command, the deferred sweep and the suggestion buttons — stamps
	// Source http and is not gated by a telegram policy in the same cc.
	cmds, cc := registerAgentCommands(operatorRegParams(&config.Config{}, config.AgentConfig{ID: "a"}),
		command.NewLastMessageStore())
	cc.Operators = command.OperatorPolicies{
		command.SourceTelegram: command.NewOperatorPolicy(cmds, "a", "telegram",
			config.OperatorAccess{Users: []string{"111"}}),
	}

	// A nil *platform.Messaging yields the noop connection manager; /ping
	// answers with no doc, so the send is a no-op.
	var noProviders *platform.Messaging
	resp, ok := runAgentCommand(context.Background(), cmds, cc,
		noProviders.ConnectionManager(), "a", "agent:a/main", "/ping")
	if !ok || resp.Text == "" {
		t.Errorf("runAgentCommand = (%q, %v), want the HTTP surface to run commands ungated", resp.Text, ok)
	}
}

// isOperatorRefusal reports whether a command reply is the operator-only
// refusal (any command name).
func isOperatorRefusal(text string) bool {
	return strings.Contains(text, "is only for this agent's operators.")
}
