package discord

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
)

// gateDiscordBot returns a test bot for agent "a" whose registry has tmux
// (with a chain) and status, and whose dispatcher runs on the discord
// transport with an operator policy: operator 111, everyone else exactly the
// permitted commands. The cc it was wired with is returned for rebuild tests.
func gateDiscordBot(t *testing.T, permitted []string) (*Bot, *fakeSession, *int, command.CommandContext) {
	t.Helper()
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
	reg.Register(&command.Command{
		Name: "status",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs++
			return command.Response{Text: "ok"}, nil
		},
	})

	b, fs, _ := newTestBot(t, "a")
	b.allowedUsers = map[string]bool{"111": true, "222": true}
	b.allowedUsersOnly = true
	b.commands = reg
	cc := command.CommandContext{
		Operators: command.OperatorPolicies{
			command.SourceDiscord: command.NewOperatorPolicy(reg, "a", "discord",
				config.OperatorAccess{Users: []string{"111"}, Commands: permitted}),
		},
	}
	b.SetCommandContext(cc)
	return b, fs, &runs, cc
}

func TestTryIntercept_NonOperatorRefused(t *testing.T) {
	// Proves the polling-goroutine path: a typed forbidden command from a
	// restricted allowed user is answered with the refusal, executes nothing
	// and never reaches the agent queue.
	b, fs, runs, _ := gateDiscordBot(t, []string{"status"})

	msg := testDiscordMessage("42", "222", "/tmux")
	qm := queuedMessage{msg: msg, userID: "222", text: "/tmux"}
	if consumed := b.tryIntercept(context.Background(), &qm); !consumed {
		t.Fatal("forbidden command must be consumed, not enqueued to the agent")
	}
	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	got := fs.lastSend(t)
	if !strings.Contains(got.content, "/tmux is only for this agent's operators.") {
		t.Errorf("reply = %q, want the operator-only refusal", got.content)
	}

	// The permitted command still runs for the same user.
	permitted := queuedMessage{msg: testDiscordMessage("42", "222", "/status"), userID: "222", text: "/status"}
	b.tryIntercept(context.Background(), &permitted)
	if *runs != 1 {
		t.Errorf("permitted /status executed %d times, want 1", *runs)
	}
}

func TestProcessQueuedCommand_NonOperatorRefused(t *testing.T) {
	// Proves the deferred worker path: a forbidden command routed through
	// the command channel is refused the same way.
	b, fs, runs, _ := gateDiscordBot(t, []string{"status"})

	b.processQueuedCommand(context.Background(), platform.QueuedMessage{
		UserID:   "222",
		Text:     "/tmux",
		ChatID:   42,
		Original: testDiscordMessage("42", "222", "/tmux"),
	})

	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	got := fs.lastSend(t)
	if !strings.Contains(got.content, "/tmux is only for this agent's operators.") {
		t.Errorf("reply = %q, want the operator-only refusal", got.content)
	}
}

func TestHandleComponentInteraction_CommandPressChecksPresser(t *testing.T) {
	// Proves a cmd: press is gated by who pressed it, through the real
	// component-interaction path: the allowed non-operator's press (guild
	// member and DM form) runs nothing and edits the pressed message to the
	// refusal while still acknowledging the press; the operator's press runs.
	b, fs, runs, _ := gateDiscordBot(t, []string{"status"})

	// Guild-member press from the restricted user.
	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/tmux kill", "g1", "222"))
	if *runs != 0 {
		t.Errorf("forbidden guild press executed the command %d times, want 0", *runs)
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
	got := fs.lastEdit(t)
	if got.msgID != "100" || got.content != "/tmux is only for this agent's operators." {
		t.Errorf("pressed message edited to %+v, want the operator-only refusal", got)
	}

	// DM press from the restricted user: same refusal.
	b.handleComponentInteraction(context.Background(), dmComponentInteraction("42", "101", "cmd:/tmux kill", "222"))
	if *runs != 0 {
		t.Errorf("forbidden DM press executed the command %d times, want 0", *runs)
	}

	// The operator's press on the same chained button gets the chain
	// keyboard — the data listing the non-operator was refused.
	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/tmux kill", "g1", "111"))
	opEdit := fs.lastEdit(t)
	if !strings.HasPrefix(opEdit.content, "/tmux kill") {
		t.Errorf("operator press edited to %q, want the chained keyboard label", opEdit.content)
	}
	if len(opEdit.components) == 0 {
		t.Error("operator press must carry the chained keyboard components")
	}

	// The operator's press of a plain command runs it.
	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/status", "g1", "111"))
	if *runs != 1 {
		t.Errorf("operator press executed the command %d times, want 1", *runs)
	}
}

func TestHandleComponentInteraction_ForbiddenChainStepNotShown(t *testing.T) {
	// Proves a forbidden chained keyboard step lists no data: the press of
	// "cmd:/tmux kill" by a non-operator produces no second keyboard and no
	// pane listing.
	b, fs, runs, _ := gateDiscordBot(t, []string{"status"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/tmux kill", "g1", "222"))

	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
	if len(fs.edits) != 1 {
		t.Fatalf("edits = %d, want exactly the refusal edit", len(fs.edits))
	}
	got := fs.lastEdit(t)
	if got.content != "/tmux is only for this agent's operators." {
		t.Errorf("edit = %q, want the refusal", got.content)
	}
	if len(got.components) != 0 {
		t.Errorf("refusal edit carried %d component rows, want none", len(got.components))
	}
}

func TestHandleComponentInteraction_NoOperatorsPressRuns(t *testing.T) {
	// Characterises the unset case: with no operators configured a cmd:
	// press from any allowed user produces the chained keyboard as before.
	b, fs, _ := newTestBot(t, "a")
	b.allowedUsers = map[string]bool{"222": true}
	b.allowedUsersOnly = true
	b.commands.Register(&command.Command{
		Name: "tmux",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: "tmux ran"}, nil
		},
		ChainKeyboard: func(_ context.Context, sub string, _ command.CommandContext) []command.KeyboardOption {
			if sub == "kill" {
				return []command.KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
			}
			return nil
		},
	})
	b.SetCommandContext(commandTestContext())

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/tmux kill", "g1", "222"))

	got := fs.lastEdit(t)
	if !strings.HasPrefix(got.content, "/tmux kill") {
		t.Errorf("edit = %q, want the chain-keyboard label", got.content)
	}
	if len(got.components) == 0 {
		t.Error("expected the chained keyboard components")
	}
}

func TestSetupAgentResultClosures_FacetBotRestricted(t *testing.T) {
	// Proves a facet bot wired through ConfigureFacetConn receives the
	// agent's CommandContext and is restricted identically to the primary.
	mgr := NewBotManager()
	primary, _, _ := newTestBot(t, "a")
	mgr.AddPrimary("a", primary)

	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: "tmux ran"}, nil
		},
	})
	cc := command.CommandContext{
		Operators: command.OperatorPolicies{
			command.SourceDiscord: command.NewOperatorPolicy(reg, "a", "discord",
				config.OperatorAccess{Users: []string{"111"}, Commands: []string{"status"}}),
		},
	}
	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:    config.AgentConfig{ID: "a"}, // no discord platform: setup skips bot creation
		GlobalConfig:   &config.Config{},
		Commands:       reg,
		CommandContext: cc,
		LastMsgStore:   command.NewLastMessageStore(),
		Resolved:       config.Resolve(&config.Config{}, config.AgentConfig{ID: "a"}),
		SecretStore:    testSecretStore(t, ""),
	})
	if res == nil {
		t.Fatal("expected result for existing primary bot")
	}

	facet, _, _ := newTestBot(t, "")
	res.ConfigureFacetConn(facet)
	if facet.dispatcher == nil {
		t.Fatal("expected facet dispatcher configured")
	}

	outcome := facet.dispatcher.DispatchCommand(context.Background(), "/tmux", 42, "222")
	if outcome.NotHandled || outcome.Response == nil ||
		outcome.Response.Result.Response.Text != "/tmux is only for this agent's operators." {
		t.Errorf("facet dispatch outcome = %+v, want the refusal", outcome)
	}
	if outcome.Keyboard != nil {
		t.Error("facet must not show a keyboard for a forbidden command")
	}

	// The operator runs the same command through the same facet.
	opOutcome := facet.dispatcher.DispatchCommand(context.Background(), "/tmux", 42, "111")
	if opOutcome.NotHandled || opOutcome.Response == nil || opOutcome.Response.Result.Response.Text != "tmux ran" {
		t.Errorf("facet operator outcome = %+v, want the command to run", opOutcome)
	}
}

func TestSetCommandContext_RebuiltDispatcherStaysRestricted(t *testing.T) {
	// Proves rebuilding a bot's dispatcher (SetCommandContext runs on facet
	// claim and restore) keeps the gate: the policy rides in the
	// CommandContext, not the dispatcher.
	b, _, runs, cc := gateDiscordBot(t, []string{"status"})

	b.SetCommandContext(cc)
	outcome := b.dispatcher.DispatchCommand(context.Background(), "/tmux", 42, "222")
	if outcome.NotHandled || outcome.Response == nil ||
		outcome.Response.Result.Response.Text != "/tmux is only for this agent's operators." {
		t.Errorf("rebuilt dispatcher outcome = %+v, want the refusal", outcome)
	}
	if *runs != 0 {
		t.Errorf("command executed %d times, want 0", *runs)
	}
}

func TestSetupAgentOperatorStartupReport(t *testing.T) {
	// Proves bot setup logs one report line naming the agent, platform,
	// operator count and permitted commands when the agent has operators —
	// the CommandContext carrying the policy exactly as
	// registerAgentCommands builds it in production.
	mgr := NewBotManager()
	cfg := &config.Config{Platforms: []config.PlatformConfig{{
		ID:     "discord",
		Access: config.AccessConfig{OperatorUsers: []string{"111"}},
	}}}
	acfg := config.AgentConfig{
		ID:        "clutch",
		Platforms: []config.PlatformConfig{{ID: "discord", Bot: "clutch"}},
	}
	reg := command.NewRegistry()
	for _, name := range config.DefaultNonOperatorCommands() {
		reg.Register(&command.Command{
			Name: name,
			Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
				return command.Response{Text: name}, nil
			},
		})
	}
	cc := command.CommandContext{Operators: command.NewOperatorPolicies(reg, cfg, acfg)}

	var buf gateLogBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:    acfg,
		GlobalConfig:   cfg,
		Commands:       reg,
		CommandContext: cc,
		Resolved:       config.Resolve(cfg, acfg),
		SecretStore:    testSecretStore(t, "[discord]\nclutch = \"token\"\n"),
	})
	if res == nil {
		t.Fatal("SetupAgent returned nil")
	}

	out := buf.String()
	for _, want := range []string{`agent "clutch"`, "discord", "operators=1", "help, reset, stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup report %q missing %q", out, want)
		}
	}
}

func TestSetupAgentNoOperatorsNoReportLine(t *testing.T) {
	// Characterises the unset case: no operator settings, no new log lines
	// at bot setup.
	mgr := NewBotManager()
	cfg := &config.Config{}
	acfg := config.AgentConfig{
		ID:        "clutch",
		Platforms: []config.PlatformConfig{{ID: "discord", Bot: "clutch"}},
	}

	var buf gateLogBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:  acfg,
		GlobalConfig: cfg,
		Commands:     command.NewRegistry(),
		Resolved:     config.Resolve(cfg, acfg),
		SecretStore:  testSecretStore(t, "[discord]\nclutch = \"token\"\n"),
	})
	if res == nil {
		t.Fatal("SetupAgent returned nil")
	}
	if out := buf.String(); strings.Contains(out, "operators") {
		t.Errorf("unset operators produced a report line: %q", out)
	}
}

// gateLogBuffer is a minimal log capture buffer.
type gateLogBuffer struct {
	lines []string
}

func (b *gateLogBuffer) Write(p []byte) (int, error) {
	b.lines = append(b.lines, string(p))
	return len(p), nil
}

func (b *gateLogBuffer) String() string { return strings.Join(b.lines, "") }
