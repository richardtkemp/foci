package command

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
)

// unsetFixture is a foci.toml with the #2716 live case (a default value
// written into [resources]), a global [keepalive] value, and a per-agent
// override for clutch.
const unsetFixture = `# top comment
[resources]
# keep this comment
goroutine_monitor_enabled = false
max_procs = 4

[keepalive]
interval = "30m"

[[agents]]
id = "main"

[[agents]]
id = "clutch"
keepalive.interval = "10m"
`

// unsetDeps builds deps over a real temp foci.toml with UnsetInFileFn
// backed by the real config.UnsetInFile (the writer the gateway wires),
// counting calls. The chat belongs to "main"; "main" and "clutch" run.
func unsetDeps(t *testing.T) (ConfigSetDeps, string, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(unsetFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps := agentFormDeps(nil)
	deps.ConfigPath = path
	deps.UnsetInFileFn = func(p string, target config.SetTarget) (string, error) {
		calls++
		return config.UnsetInFile(p, target, 0o644)
	}
	return deps, path, &calls
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestConfigUnsetGlobalKey is the ticket's live case: /config unset
// resources.goroutine_monitor_enabled removes the line (only that line —
// comments and siblings stay) and reports the old value, the default it
// reverts to, and live apply.
func TestConfigUnsetGlobalKey(t *testing.T) {
	deps, path, calls := unsetDeps(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "unset resources.goroutine_monitor_enabled")

	if *calls != 1 {
		t.Fatalf("UnsetInFileFn calls = %d, want 1", *calls)
	}
	got := readFile(t, path)
	want := strings.Replace(unsetFixture, "goroutine_monitor_enabled = false\n", "", 1)
	if got != want {
		t.Errorf("file after unset:\n%s\nwant:\n%s", got, want)
	}
	for _, w := range []string{"Unset resources.goroutine_monitor_enabled", "was false", "default: false", "Applied live."} {
		if !strings.Contains(text, w) {
			t.Errorf("reply missing %q: %q", w, text)
		}
	}
}

// TestConfigUnsetKeyNotInFile: an absent key (and an absent section) is a
// plain "nothing to unset" reply, not an error, and the file is untouched.
func TestConfigUnsetKeyNotInFile(t *testing.T) {
	deps, path, _ := unsetDeps(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	for _, args := range []string{"unset resources.goroutine_monitor_threshold", "unset agent_loop.max_tool_loops"} {
		if _, ok := deps.LookupFn(strings.TrimPrefix(args, "unset ")); !ok {
			t.Fatalf("fixture key %q is not a registry field", args)
		}
		text := execConfigCommand(t, cc, args)
		if !strings.Contains(text, "is not set in foci.toml") {
			t.Errorf("%s: reply = %q", args, text)
		}
	}
	if got := readFile(t, path); got != unsetFixture {
		t.Errorf("file changed:\n%s", got)
	}
}

// TestConfigUnsetAgentPrefixTargetsOwnBlock: "agent.<key>" removes the key
// from THIS chat's agent block, as the set direct form writes it. "main"
// has no override, so it reports nothing to unset; clutch's stays.
func TestConfigUnsetAgentPrefixTargetsOwnBlock(t *testing.T) {
	deps, path, _ := unsetDeps(t)
	deps.AgentID = "clutch"
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "unset agent.keepalive.interval")
	if !strings.Contains(text, `Unset agent.keepalive.interval (was "10m")`) || !strings.Contains(text, "inherits the global value") {
		t.Errorf("reply = %q", text)
	}
	if strings.Contains(readFile(t, path), `"10m"`) {
		t.Error("clutch's override is still in the file")
	}
}

// TestConfigUnsetNamedAgent: "unset <agent> <key>" removes the named
// agent's override and reports what it now resolves to — the global value
// read back from the file.
func TestConfigUnsetNamedAgent(t *testing.T) {
	deps, path, _ := unsetDeps(t)
	cc := CommandContext{ConfigSetDeps: &deps}

	text := execConfigCommand(t, cc, "unset clutch keepalive.interval")

	got := readFile(t, path)
	if strings.Contains(got, `"10m"`) || !strings.Contains(got, `interval = "30m"`) {
		t.Errorf("file after unset:\n%s", got)
	}
	for _, w := range []string{`Unset clutch keepalive.interval (was "10m")`, "Now 30m (inherited from [keepalive] in foci.toml)", "Applied live."} {
		if !strings.Contains(text, w) {
			t.Errorf("reply missing %q: %q", w, text)
		}
	}
}

// TestConfigUnsetNamedAgentRefusals mirrors the per-agent set's refusals:
// unknown agent, not a per-agent key, restart-only field. None writes.
func TestConfigUnsetNamedAgentRefusals(t *testing.T) {
	cases := []struct{ args, want string }{
		{"unset ghost keepalive.interval", `Unknown agent "ghost"`},
		{"unset clutch debug.enable_pprof", "not a per-agent setting"},
		{"unset clutch behavior.enable_stop_aliases", "needs a restart"},
		{"unset main keepalive.interval", "is not set in foci.toml"},
	}
	for _, c := range cases {
		deps, path, calls := unsetDeps(t)
		cc := CommandContext{ConfigSetDeps: &deps}
		text := execConfigCommand(t, cc, c.args)
		if !strings.Contains(text, c.want) {
			t.Errorf("%s: reply = %q, want %q", c.args, text, c.want)
		}
		if got := readFile(t, path); got != unsetFixture {
			t.Errorf("%s: file changed", c.args)
		}
		if c.want != "is not set in foci.toml" && *calls != 0 {
			t.Errorf("%s: UnsetInFileFn called %d times, want 0", c.args, *calls)
		}
	}
}

// TestConfigUnsetUsageAndUnknown: bare, malformed and unknown forms answer
// with usage or an unknown-field reply and never write.
func TestConfigUnsetUsageAndUnknown(t *testing.T) {
	cases := []struct{ args, want string }{
		{"unset", "Usage:"},
		{"unset resources", "Usage:"},
		{"unset a b c", "Usage:"},
		{"unset resources.x clutch", "Usage:"},
		{"unset resources.no_such_key", "Unknown config field"},
	}
	for _, c := range cases {
		deps, _, calls := unsetDeps(t)
		cc := CommandContext{ConfigSetDeps: &deps}
		text := execConfigCommand(t, cc, c.args)
		if !strings.Contains(text, c.want) {
			t.Errorf("%q: reply = %q, want %q", c.args, text, c.want)
		}
		if *calls != 0 {
			t.Errorf("%q: UnsetInFileFn called", c.args)
		}
	}
}

// TestConfigUnsetWriteErrorIsReturned: a failure other than "not set" is
// an error reply, not swallowed as "nothing to unset".
func TestConfigUnsetWriteErrorIsReturned(t *testing.T) {
	deps, _, _ := unsetDeps(t)
	deps.UnsetInFileFn = func(string, config.SetTarget) (string, error) {
		return "", errors.New("disk on fire")
	}
	_, err := configUnset(&deps, "resources.goroutine_monitor_enabled")
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("err = %v, want the write error", err)
	}
}
