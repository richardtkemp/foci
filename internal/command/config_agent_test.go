package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
)

// execConfigCommand drives /config <args> through ConfigCommand's Execute —
// the same dispatch path platforms take (subcommand stripping included) —
// and returns the reply text, failing on error replies.
func execConfigCommand(t *testing.T, cc CommandContext, args string) string {
	t.Helper()
	resp, err := ConfigCommand().Execute(context.Background(), Request{Name: "config", Args: args}, cc)
	if err != nil {
		t.Fatalf("/config %s: %v", args, err)
	}
	return resp.Text
}

// agentFormDeps builds deps for the per-agent form tests: the chat belongs
// to agent "main", both "main" and "clutch" are running, and setFn (nil →
// no-op) backs SetInFileFn.
func agentFormDeps(setFn func(path string, target config.SetTarget, value string) (string, error)) ConfigSetDeps {
	deps := testConfigSetDeps(setFn)
	deps.AgentID = "main"
	deps.AgentIDsFn = func() []string { return []string{"main", "clutch"} }
	return deps
}

// writeAgentConfigFile writes a minimal foci.toml with a global [keepalive]
// section and two agents, then writes main's per-agent override through the
// real config.SetInFile — the same writer the per-agent set uses — so get
// reads a file shaped exactly like a post-set one. Returns the path.
func writeAgentConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	base := "[keepalive]\ninterval = \"30m\"\n\n[[agents]]\nid = \"main\"\n\n[[agents]]\nid = \"clutch\"\n"
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetInFile(path, config.SetTarget{Section: "agents", AgentID: "main", Key: "keepalive.interval"}, `"10m"`, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfigSetAgentTargetsNamedAgent proves the ticket's example works:
// from ANOTHER agent's chat ("main"), `set clutch keepalive.interval=1m`
// writes agent clutch's [[agents]] block (SetTarget Section "agents",
// AgentID "clutch") instead of erroring on the space-containing path, and
// the reply names the agent, key, new value, old value and live apply.
func TestConfigSetAgentTargetsNamedAgent(t *testing.T) {
	var captured config.SetTarget
	var capturedValue string
	calls := 0
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		calls++
		return `"55m"`, nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set clutch keepalive.interval=1m")

	if captured.Section != "agents" || captured.AgentID != "clutch" || captured.Key != "keepalive.interval" {
		t.Errorf("target = %+v", captured)
	}
	if calls != 1 {
		t.Errorf("SetInFileFn calls = %d, want exactly 1", calls)
	}
	if capturedValue != `"1m"` {
		t.Errorf("value = %q, want TOML-quoted \"1m\"", capturedValue)
	}
	for _, want := range []string{"clutch", "keepalive.interval", `"1m"`, `"55m"`, "Applied live."} {
		if !strings.Contains(text, want) {
			t.Errorf("reply %q missing %q", text, want)
		}
	}
}

// TestConfigSetAgentStripsAgentPrefix proves "agent.<key>" and "<key>" name
// the same field in the per-agent form: the prefix is stripped before the
// registry lookup and the write targets the bare key.
func TestConfigSetAgentStripsAgentPrefix(t *testing.T) {
	var captured config.SetTarget
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	execConfigCommand(t, cc, "set clutch agent.keepalive.interval=1m")

	if captured.AgentID != "clutch" || captured.Key != "keepalive.interval" {
		t.Errorf("target = %+v", captured)
	}
}

// TestConfigSetAgentValueIsEverythingAfterFirstEquals proves the value is
// everything after the FIRST "=", trimmed at its ends only: spaces and
// further "=" characters inside the value survive intact.
func TestConfigSetAgentValueIsEverythingAfterFirstEquals(t *testing.T) {
	var capturedValue string
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		capturedValue = value
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	execConfigCommand(t, cc, "set clutch keepalive.prompt=a prompt with spaces = and equals")

	if capturedValue != `"a prompt with spaces = and equals"` {
		t.Errorf("value = %q", capturedValue)
	}
}

// TestConfigSetAgentInvalidValueWritesNothing proves a value failing the
// field's type check writes nothing and says why, under the same
// format-then-validate contract as ConfigSetDirect.
func TestConfigSetAgentInvalidValueWritesNothing(t *testing.T) {
	called := false
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		called = true
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set clutch loop.max_output_tokens=nope")

	if called {
		t.Error("SetInFileFn must not be called for an invalid value")
	}
	if !strings.Contains(text, "Invalid value") {
		t.Errorf("reply = %q", text)
	}
}

// TestConfigSetAgentRefusesRestartOnlyField proves the per-agent set is
// hot-only: keepalive.warm_open_app_chats (no hot tag) is refused with a
// message saying it needs a restart and cannot be set per agent from chat,
// nothing is written, and the refusal is distinguishable from the
// not-a-per-agent-setting one.
func TestConfigSetAgentRefusesRestartOnlyField(t *testing.T) {
	called := false
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		called = true
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set clutch keepalive.warm_open_app_chats=true")

	if called {
		t.Error("SetInFileFn must not be called for a restart-only field")
	}
	if !strings.Contains(text, "needs a restart") || !strings.Contains(text, "cannot be set per agent from chat") {
		t.Errorf("reply = %q", text)
	}
	if strings.Contains(text, "not a per-agent setting") {
		t.Errorf("needs-restart refusal must be distinct from not-a-per-agent-setting: %q", text)
	}
}

// TestConfigSetAgentRefusesRestartOnlyFieldForCurrentAgent proves the
// hot-only gate applies even when <agent> IS the current agent — writing
// restart-only per-agent fields stays with the existing current-agent
// forms, which reply "Restart to take effect.".
func TestConfigSetAgentRefusesRestartOnlyFieldForCurrentAgent(t *testing.T) {
	called := false
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		called = true
		return "", nil
	})
	deps.AgentID = "clutch" // the named agent IS the current agent
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set clutch keepalive.warm_open_app_chats=true")

	if called {
		t.Error("SetInFileFn must not be called for a restart-only field")
	}
	if !strings.Contains(text, "cannot be set per agent from chat") {
		t.Errorf("reply = %q", text)
	}
}

// TestConfigSetAgentNotAPerAgentSetting proves the "no agent.<key> registry
// row" refusal: a field whose scope excludes agents (debug.enable_pprof)
// and a non-registry key (backend) both answer "not a per-agent setting",
// write nothing, and never claim a restart is the issue.
func TestConfigSetAgentNotAPerAgentSetting(t *testing.T) {
	called := false
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		called = true
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	for _, args := range []string{
		"set clutch debug.enable_pprof=true",
		"set clutch backend=claude-code",
	} {
		text := execConfigCommand(t, cc, args)
		if called {
			t.Errorf("%s: SetInFileFn must not be called", args)
		}
		if !strings.Contains(text, "not a per-agent setting") {
			t.Errorf("%s: reply = %q", args, text)
		}
		if strings.Contains(text, "needs a restart") {
			t.Errorf("%s: not-per-agent refusal must not mention restart: %q", args, text)
		}
	}
}

// TestConfigSetAgentUnknownAgent proves <agent> must exactly match a
// running agent id: an unknown id (including a case-mismatched one) writes
// nothing, is named in the reply, and the valid ids are listed.
func TestConfigSetAgentUnknownAgent(t *testing.T) {
	called := false
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		called = true
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	for _, agent := range []string{"scout", "Clutch"} { // absent, and case-mismatched
		text := execConfigCommand(t, cc, "set "+agent+" keepalive.interval=1m")
		if called {
			t.Errorf("%s: SetInFileFn must not be called", agent)
		}
		for _, want := range []string{agent, "main", "clutch"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: reply %q missing %q", agent, text, want)
			}
		}
	}
}

// TestConfigGetAgentKeyFromFile proves get reads the config FILE at call
// time through the full value ladder: main's explicit per-agent 10m
// (written by the real SetInFile), clutch inheriting 30m from the global
// [keepalive] section, and clutch's scheduler.tick_interval falling through
// to the built-in default 30s — plus the leading "agent." being optional.
func TestConfigGetAgentKeyFromFile(t *testing.T) {
	deps := agentFormDeps(nil)
	deps.ConfigPath = writeAgentConfigFile(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "get main keepalive.interval")
	for _, want := range []string{"main keepalive.interval", "10m", "set for main in foci.toml", "Settable live"} {
		if !strings.Contains(text, want) {
			t.Errorf("explicit value: reply %q missing %q", text, want)
		}
	}

	text = execConfigCommand(t, cc, "get clutch keepalive.interval")
	for _, want := range []string{"30m", "inherited from [keepalive] in foci.toml"} {
		if !strings.Contains(text, want) {
			t.Errorf("inherited value: reply %q missing %q", text, want)
		}
	}

	text = execConfigCommand(t, cc, "get clutch scheduler.tick_interval")
	for _, want := range []string{"30s", "built-in default"} {
		if !strings.Contains(text, want) {
			t.Errorf("default value: reply %q missing %q", text, want)
		}
	}

	text = execConfigCommand(t, cc, "get clutch agent.keepalive.interval")
	if !strings.Contains(text, "30m") {
		t.Errorf("agent. prefix: reply = %q", text)
	}
}

// TestConfigGetReportsRestartOnlyField proves get reads restart-only fields
// too (reading is harmless) and says the field needs a restart to change.
func TestConfigGetReportsRestartOnlyField(t *testing.T) {
	deps := agentFormDeps(nil)
	deps.ConfigPath = writeAgentConfigFile(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "get clutch keepalive.warm_open_app_chats")

	if !strings.Contains(text, "keepalive.warm_open_app_chats") {
		t.Errorf("reply = %q", text)
	}
	if !strings.Contains(text, "Needs a restart to change") {
		t.Errorf("reply = %q", text)
	}
	if strings.Contains(text, "Settable live") {
		t.Errorf("restart-only field must not be advertised as settable live: %q", text)
	}
}

// TestConfigGetAgentListsHotKeys proves "get <agent>" lists the keys the
// per-agent set accepts — the hot agent-section rows — and excludes
// restart-only ones like keepalive.warm_open_app_chats.
func TestConfigGetAgentListsHotKeys(t *testing.T) {
	deps := agentFormDeps(nil)
	deps.ConfigPath = writeAgentConfigFile(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "get clutch")

	if !strings.Contains(text, "keepalive.interval") {
		t.Errorf("reply = %q, want keepalive.interval listed", text)
	}
	if strings.Contains(text, "warm_open_app_chats") {
		t.Errorf("reply = %q, restart-only keys must be excluded", text)
	}
}

// TestConfigGetUsage proves get's arity refusals: bare "get" shows usage,
// and words beyond "<agent> <key>" are flagged before the usage follows —
// never silently ignored.
func TestConfigGetUsage(t *testing.T) {
	deps := agentFormDeps(nil)
	deps.ConfigPath = writeAgentConfigFile(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "get")
	if !strings.Contains(text, "Usage") || !strings.Contains(text, "get <agent>") || strings.Contains(text, "Too many") {
		t.Errorf("bare get reply = %q", text)
	}

	text = execConfigCommand(t, cc, "get main keepalive.interval extra")
	if !strings.Contains(text, "Too many arguments.") || !strings.Contains(text, "Usage") {
		t.Errorf("extra-args reply = %q", text)
	}
}

// TestConfigGetUnknownAgentAndUnknownKey proves get reports the same
// unknown-agent and not-a-per-agent-setting refusals as the per-agent set,
// for both of its forms.
func TestConfigGetUnknownAgentAndUnknownKey(t *testing.T) {
	deps := agentFormDeps(nil)
	deps.ConfigPath = writeAgentConfigFile(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	for _, args := range []string{"get scout keepalive.interval", "get scout"} {
		text := execConfigCommand(t, cc, args)
		if !strings.Contains(text, `Unknown agent "scout"`) || !strings.Contains(text, "main") {
			t.Errorf("%s: reply = %q", args, text)
		}
	}

	text := execConfigCommand(t, cc, "get main debug.enable_pprof")
	if !strings.Contains(text, "not a per-agent setting") {
		t.Errorf("reply = %q", text)
	}
}

// TestConfigGetRequiresConfigSetDeps proves the new forms ride the existing
// ConfigSetDeps gate: with no deps wired, get and the per-agent set answer
// "not available" exactly like the existing set forms — no new route,
// endpoint or bypass.
func TestConfigGetRequiresConfigSetDeps(t *testing.T) {
	cc := CommandContext{}

	if text := execConfigCommand(t, cc, "get main keepalive.interval"); text != "Config get is not available." {
		t.Errorf("get reply = %q", text)
	}
	if text := execConfigCommand(t, cc, "set clutch keepalive.interval=1m"); text != "Config set is not available." {
		t.Errorf("set reply = %q", text)
	}
}

// TestConfigCommandDescriptionsCoverAgentForms proves the command and
// subcommand descriptions advertise the new forms: the command lists get
// among its subcommands, get has a description, and set's description
// mentions the <agent> form.
func TestConfigCommandDescriptionsCoverAgentForms(t *testing.T) {
	cmd := ConfigCommand()
	if !strings.Contains(cmd.Description, "get") {
		t.Errorf("command description = %q", cmd.Description)
	}
	var get, set *Subcommand
	for i := range cmd.Subcommands {
		switch cmd.Subcommands[i].Name {
		case "get":
			get = &cmd.Subcommands[i]
		case "set":
			set = &cmd.Subcommands[i]
		}
	}
	if get == nil || get.Description == "" {
		t.Errorf("get subcommand = %+v", get)
	}
	if set == nil || !strings.Contains(set.Description, "<agent>") {
		t.Errorf("set description = %+v", set)
	}
}

// TestConfigSetDirectAgentFormStillWritesNonHot is a characterisation test
// (listed in .factory/red-gate-exempt.txt): the CURRENT-agent direct form
// "agent.<key>=value" must keep writing restart-only fields with the
// "Restart to take effect." reply — the hot-only rule applies only to the
// new per-agent form.
func TestConfigSetDirectAgentFormStillWritesNonHot(t *testing.T) {
	var captured config.SetTarget
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set agent.keepalive.warm_open_app_chats=true")

	if captured.Section != "agents" || captured.AgentID != "main" || captured.Key != "keepalive.warm_open_app_chats" {
		t.Errorf("target = %+v", captured)
	}
	if !strings.Contains(text, "Restart to take effect.") {
		t.Errorf("reply = %q", text)
	}
}

// TestConfigSetThreeWordFormStillWorks is a characterisation test (listed
// in .factory/red-gate-exempt.txt): the three-word keyboard form
// "set <section> <key> <value>" (no "=") must keep performing a direct
// write — the per-agent branch must never match it.
func TestConfigSetThreeWordFormStillWorks(t *testing.T) {
	var captured config.SetTarget
	var capturedValue string
	deps := agentFormDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return "", nil
	})
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "set debug messages_in_log true")

	if captured.Section != "debug" || captured.Key != "messages_in_log" {
		t.Errorf("target = %+v", captured)
	}
	if capturedValue != "true" {
		t.Errorf("value = %q", capturedValue)
	}
	if !strings.Contains(text, "Set debug.messages_in_log") {
		t.Errorf("reply = %q", text)
	}
}
