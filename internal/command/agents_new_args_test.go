package command

// #3122: /agents new with its answers as arguments — one step in chat, an
// approval request over the HTTP API.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"foci/internal/configapprove"
	"foci/internal/provision"
)

func TestParseAgentNew(t *testing.T) {
	a, err := parseAgentNew("Greek Tutor --backend claude-code --model=opus --chars blank --wait 30m")
	if err != nil {
		t.Fatal(err)
	}
	want := agentNewArgs{Name: "Greek Tutor", Backend: "claude-code", Model: "opus", Chars: "blank", Wait: 30 * time.Minute}
	if a != want {
		t.Errorf("parsed %+v, want %+v", a, want)
	}
	a, err = parseAgentNew("Stuart --copy cam")
	if err != nil || a.Chars != "copy cam" {
		t.Errorf("--copy: %+v, %v; want Chars \"copy cam\"", a, err)
	}
	for _, bad := range []string{
		"--backend api",                   // no name
		"Stuart --model",                  // flag without value
		"Stuart --colour blue",            // unknown flag
		"Stuart --wait soon",              // bad duration
		"Stuart --chars blank --copy cam", // two character-file choices
	} {
		if _, err := parseAgentNew(bad); err == nil {
			t.Errorf("parseAgentNew(%q) accepted, want an error", bad)
		}
	}
}

// directDeps is AgentNewDeps over a temp home with crontab writes stubbed.
func directDeps(t *testing.T, agents []AgentInfo) (AgentNewDeps, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "foci.toml")
	if err := os.WriteFile(cfg, []byte("# existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := provision.AppendCrontab
	t.Cleanup(func() { provision.AppendCrontab = orig })
	provision.AppendCrontab = func([]string) error { return nil }
	return AgentNewDeps{
		ConfigPath:        cfg,
		DefaultsDir:       filepath.Join(tmp, "no-defaults"),
		HomeDir:           tmp,
		ListFn:            func() []AgentInfo { return agents },
		AvailableBackends: []string{"claude-code", "codex"},
	}, tmp
}

func runAgents(t *testing.T, deps *AgentNewDeps, args, source string) string {
	t.Helper()
	cc := CommandContext{AgentListFn: func() []AgentInfo { return nil }, AgentNewDeps: deps}
	resp, err := AgentsCommand().Execute(context.Background(), Request{Args: args, SessionKey: "cam/app/1", Source: source}, cc)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Text
}

func TestAgentsNewDirect_ChatCreatesAtOnce(t *testing.T) {
	deps, home := directDeps(t, []AgentInfo{{ID: "cam"}})
	var started []string
	deps.StartAgent = func(id string) ([]string, error) { started = append(started, id); return nil, nil }

	reply := runAgents(t, &deps, "new Stuart --backend claude-code --model opus --chars blank", "app")
	if !strings.Contains(reply, "is running") {
		t.Fatalf("reply = %q, want the creation report", reply)
	}
	cfg, _ := os.ReadFile(deps.ConfigPath)
	for _, want := range []string{`id = "stuart"`, `backend = "claude-code"`, `backend_config.model = "opus"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("foci.toml misses %s:\n%s", want, cfg)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "stuart", "character")); err != nil {
		t.Errorf("workspace not created: %v", err)
	}
	if len(started) != 1 || started[0] != "stuart" {
		t.Errorf("started = %v, want [stuart]", started)
	}
}

func TestAgentsNewDirect_RefusesWhatTheWizardRefuses(t *testing.T) {
	deps, _ := directDeps(t, []AgentInfo{{ID: "stuart"}, {ID: "cam"}})
	for args, want := range map[string]string{
		"new Stuart":                         "already exists",
		"new Bob --backend nope":             "Must be one of",
		"new Bob --backend api --model opus": "no model of its own",
		"new Bob --copy ghost":               "`ghost` not found",
		"new Bob --chars fancy":              "Must be `defaults`",
		"new Bob --frob":                     "Usage: /agents new",
	} {
		reply := runAgents(t, &deps, args, "app")
		if !strings.Contains(reply, want) {
			t.Errorf("/agents %s: reply %q, want it to contain %q", args, reply, want)
		}
	}
	cfg, _ := os.ReadFile(deps.ConfigPath)
	if string(cfg) != "# existing\n" {
		t.Errorf("a refused create wrote foci.toml:\n%s", cfg)
	}
}

func TestAgentsNewDirect_ConfigCheckRefuses(t *testing.T) {
	deps, _ := directDeps(t, nil)
	deps.CheckConfig = func(content string) error {
		if strings.Contains(content, `id = "bob"`) {
			return errors.New("no [groups] powerful")
		}
		return nil
	}
	reply := runAgents(t, &deps, "new Bob --backend api", "app")
	if !strings.Contains(reply, "no [groups] powerful") || !strings.Contains(reply, "nothing written") {
		t.Errorf("reply = %q, want the config check's refusal", reply)
	}
}

func TestAgentsNewDirect_HTTPAsksApprovalThenCreates(t *testing.T) {
	deps, home := directDeps(t, []AgentInfo{{ID: "cam"}})
	var sub configapprove.ActionSubmission
	deps.RequestApproval = func(a configapprove.ActionSubmission) configapprove.Status {
		sub = a
		return configapprove.Status{ID: "cfg-1", Outcome: configapprove.Pending, Path: a.Title}
	}
	reply := runAgents(t, &deps, "new Stuart --model opus --chars blank --wait 30m", SourceHTTP)
	if !strings.Contains(reply, "cfg-1") || !strings.Contains(reply, "foci config wait cfg-1") {
		t.Errorf("reply = %q, want the request id and how to follow it", reply)
	}
	if sub.SessionKey != "cam/app/1" || sub.Timeout != 30*time.Minute || sub.Title != "new agent stuart" {
		t.Errorf("submission = %+v", sub)
	}
	for _, want := range []string{`id = "stuart"`, `backend = "claude-code"`, filepath.Join(home, "stuart"), "blank"} {
		if !strings.Contains(sub.Detail, want) {
			t.Errorf("prompt detail misses %q:\n%s", want, sub.Detail)
		}
	}
	// Nothing is written until the owner approves.
	if cfg, _ := os.ReadFile(deps.ConfigPath); string(cfg) != "# existing\n" {
		t.Fatalf("foci.toml written before approval:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(home, "stuart")); !os.IsNotExist(err) {
		t.Fatalf("workspace created before approval (stat err %v)", err)
	}
	result, err := sub.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result, "Config: appended") {
		t.Errorf("Run result = %q, want the creation report", result)
	}
	if cfg, _ := os.ReadFile(deps.ConfigPath); !strings.Contains(string(cfg), `id = "stuart"`) {
		t.Errorf("approved create did not write the block:\n%s", cfg)
	}
}

func TestAgentsNewDirect_HTTPRunRechecksAtApproval(t *testing.T) {
	agents := []AgentInfo{{ID: "cam"}}
	deps, _ := directDeps(t, nil)
	deps.ListFn = func() []AgentInfo { return agents }
	var sub configapprove.ActionSubmission
	deps.RequestApproval = func(a configapprove.ActionSubmission) configapprove.Status {
		sub = a
		return configapprove.Status{ID: "cfg-2", Outcome: configapprove.Pending}
	}
	runAgents(t, &deps, "new Stuart --chars blank", SourceHTTP)
	agents = append(agents, AgentInfo{ID: "stuart"}) // created meanwhile, e.g. by the wizard
	if _, err := sub.Run(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Run err = %v, want the name refused at approval", err)
	}
	if cfg, _ := os.ReadFile(deps.ConfigPath); string(cfg) != "# existing\n" {
		t.Errorf("foci.toml written:\n%s", cfg)
	}
}

func TestAgentsNewDirect_HTTPWithoutApprovalRoute(t *testing.T) {
	deps, _ := directDeps(t, nil)
	reply := runAgents(t, &deps, "new Stuart", SourceHTTP)
	if !strings.Contains(reply, "not available") {
		t.Errorf("reply = %q, want the HTTP form refused", reply)
	}
	if cfg, _ := os.ReadFile(deps.ConfigPath); string(cfg) != "# existing\n" {
		t.Errorf("foci.toml written:\n%s", cfg)
	}
}

func TestAgentsNewBare_HTTPRefusalPointsAtOneStepForm(t *testing.T) {
	deps, _ := directDeps(t, nil)
	reply := runAgents(t, &deps, "new", SourceHTTP)
	if !strings.Contains(reply, "/agents new <name>") {
		t.Errorf("reply = %q, want it to name the one-step form", reply)
	}
}

// stuartBC is the ticket's own case: the agent's Claude Code account.
const stuartBC = `'{accounts = ["/Users/x/.claude-stuart"]}'`

func TestParseAgentNew_BackendConfig(t *testing.T) {
	a, err := parseAgentNew("Greek Tutor --backend-config " + stuartBC + " --model opus")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Greek Tutor" || a.Model != "opus" || a.BackendConfig != `{accounts = ["/Users/x/.claude-stuart"]}` {
		t.Errorf("parsed %+v", a)
	}
	// Unquoted, with spaces, and the --flag=value form keep the table whole.
	for _, args := range []string{
		`Stuart --backend-config {env = {A = "b c"}}`,
		`Stuart --backend-config={env = {A = "b c"}}`,
	} {
		a, err := parseAgentNew(args)
		if err != nil || a.Name != "Stuart" || a.BackendConfig != `{env = {A = "b c"}}` {
			t.Errorf("parseAgentNew(%s) = %+v, %v", args, a, err)
		}
	}
	for args, want := range map[string]string{
		`Stuart --backend-config '"just a string"'`:  "inline table",
		`Stuart --backend-config '{}'`:               "inline table",
		`Stuart --backend-config '{env = }'`:         "not a TOML value",
		`Stuart --backend-config '{model = "opus"}'`: "--model",
		`Stuart --backend-config '{env = {A = 1}`:    "unclosed",
		`Stuart --backend-config {env = {A = 1}`:     "unbalanced",
	} {
		if _, err := parseAgentNew(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseAgentNew(%s) err = %v, want it to contain %q", args, err, want)
		}
	}
}

// The extras are in foci.toml BEFORE the live start, so the agent starts on
// the right account with no restart.
func TestAgentsNewDirect_BackendConfigWrittenBeforeStart(t *testing.T) {
	deps, _ := directDeps(t, nil)
	var atStart string
	deps.StartAgent = func(string) ([]string, error) {
		data, err := os.ReadFile(deps.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		atStart = string(data)
		return nil, nil
	}
	reply := runAgents(t, &deps, "new Stuart --model opus --chars blank --backend-config "+stuartBC, "app")
	if !strings.Contains(reply, "is running") {
		t.Fatalf("reply = %q", reply)
	}
	var doc struct {
		Agents []struct {
			ID            string         `toml:"id"`
			BackendConfig map[string]any `toml:"backend_config"`
		} `toml:"agents"`
	}
	if _, err := toml.Decode(atStart, &doc); err != nil || len(doc.Agents) != 1 {
		t.Fatalf("config at start does not hold one agent (%v):\n%s", err, atStart)
	}
	bc := doc.Agents[0].BackendConfig
	accounts, _ := bc["accounts"].([]any)
	if len(accounts) != 1 || accounts[0] != "/Users/x/.claude-stuart" || bc["model"] != "opus" {
		t.Errorf("backend_config at start = %v, want the account and model opus", bc)
	}
}

func TestAgentsNewDirect_BackendConfigGoesThroughConfigCheck(t *testing.T) {
	deps, _ := directDeps(t, nil)
	var checked string
	deps.CheckConfig = func(content string) error {
		checked = content
		if strings.Contains(content, ".claude-stuart") {
			return errors.New("bad account")
		}
		return nil
	}
	reply := runAgents(t, &deps, "new Stuart --chars blank --backend-config "+stuartBC, "app")
	if !strings.Contains(reply, "bad account") || !strings.Contains(reply, "nothing written") {
		t.Errorf("reply = %q, want the config check's refusal", reply)
	}
	if !strings.Contains(checked, ".claude-stuart") {
		t.Errorf("the config check never saw the extras:\n%s", checked)
	}
	if cfg, _ := os.ReadFile(deps.ConfigPath); string(cfg) != "# existing\n" {
		t.Errorf("foci.toml written:\n%s", cfg)
	}
}

func TestAgentsNewDirect_BackendConfigInApprovalPrompt(t *testing.T) {
	deps, _ := directDeps(t, nil)
	var sub configapprove.ActionSubmission
	deps.RequestApproval = func(a configapprove.ActionSubmission) configapprove.Status {
		sub = a
		return configapprove.Status{ID: "cfg-3", Outcome: configapprove.Pending}
	}
	runAgents(t, &deps, "new Stuart --model opus --backend-config "+stuartBC, SourceHTTP)
	// Inside the fenced [[agents]] block, not just somewhere in the text.
	block := sub.Detail[strings.Index(sub.Detail, "```toml"):]
	if !strings.Contains(block, `backend_config = {accounts = ["/Users/x/.claude-stuart"], model = "opus"}`) {
		t.Errorf("prompt block misses the backend_config:\n%s", sub.Detail)
	}
}

func TestAgentsNewDirect_MalformedBackendConfigWritesNothing(t *testing.T) {
	deps, home := directDeps(t, nil)
	for _, bc := range []string{`'{env = }'`, `'"str"'`, `'{model = "x"}'`} {
		for _, src := range []string{"app", SourceHTTP} {
			called := false
			deps.RequestApproval = func(configapprove.ActionSubmission) configapprove.Status {
				called = true
				return configapprove.Status{ID: "x", Outcome: configapprove.Pending}
			}
			reply := runAgents(t, &deps, "new Stuart --backend-config "+bc, src)
			if !strings.Contains(reply, "--backend-config") || called {
				t.Errorf("%s via %s: reply %q (approval asked: %v), want a refusal naming --backend-config", bc, src, reply, called)
			}
		}
	}
	reply := runAgents(t, &deps, "new Stuart --backend api --backend-config "+stuartBC, "app")
	if !strings.Contains(reply, "no backend_config") {
		t.Errorf("api + --backend-config: reply %q", reply)
	}
	if cfg, _ := os.ReadFile(deps.ConfigPath); string(cfg) != "# existing\n" {
		t.Errorf("foci.toml written:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(home, "stuart")); !os.IsNotExist(err) {
		t.Errorf("workspace created (stat err %v)", err)
	}
}
