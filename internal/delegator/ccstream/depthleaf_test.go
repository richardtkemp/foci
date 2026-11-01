package ccstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/hookbin"
	"foci/internal/delegator/pretool"
	"foci/internal/log"
)

// depthLaunch is what one Start handed the CC process: its environment and
// its argv.
type depthLaunch struct {
	env  []string
	args []string
}

// launchDepth starts a real Backend through Start, with the claude binary
// pointed at a recorder script and the hook binary present (FOCI_HOOK_BIN),
// and returns the environment and arguments that process saw. agentEnv is the
// per-agent backend env; agentRules the agent's own pretool rules.
func launchDepth(t *testing.T, leaf bool, agentEnv map[string]string, agentRules []pretool.Rule) depthLaunch {
	t.Helper()
	log.SetWarnHook(func(log.Level, string, string) {})
	dir := t.TempDir()
	hook := filepath.Join(dir, "foci-cc-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hook stub: %v", err)
	}
	t.Setenv(hookbin.EnvOverride, hook)
	clearEnvVar(t, disableBackgroundTasksEnv)
	envOut := filepath.Join(dir, "env.txt")
	argsOut := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-claude")
	// Each dump is written to a scratch file and renamed into place, so it
	// appears only once complete (#2593).
	body := "#!/bin/sh\nenv > '" + envOut + ".tmp' && mv '" + envOut + ".tmp' '" + envOut + "'\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argsOut + ".tmp' && mv '" + argsOut + ".tmp' '" + argsOut + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write recorder script: %v", err)
	}
	b, err := newFromConfig(map[string]any{"binary": script})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	be := b.(*Backend)
	be.SetHostHooks(delegator.HostHooks{PreToolRules: func() []pretool.Rule { return agentRules }})
	if err := be.Start(context.Background(), delegator.StartOptions{
		WorkDir: dir, AgentID: "depthtest", SessionKey: "depthtest/leaf", Env: agentEnv, DepthLeaf: leaf,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(10 * time.Second); be.IsRunning() && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		_ = be.Close()
		log.SetWarnHook(nil)
	})
	read := func(path string) []string {
		var data []byte
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if data, err = os.ReadFile(path); err == nil && len(data) > 0 {
				break
			}
		}
		if len(data) == 0 {
			t.Fatalf("recorder script produced no %s: subprocess never ran", filepath.Base(path))
		}
		return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	return depthLaunch{env: read(envOut), args: read(argsOut)}
}

// lastEnv is the value a process sees for key: the last assignment wins.
func lastEnv(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, e := range env {
		if v, found := strings.CutPrefix(e, key+"="); found {
			val, ok = v, true
		}
	}
	return val, ok
}

// rulesSeen decodes the pretool rules the CC process got, and the
// PreToolUse matcher of its --settings: exactly what foci-cc-hook evaluates
// and what CC calls it for.
func rulesSeen(t *testing.T, l depthLaunch) ([]pretool.Rule, string) {
	t.Helper()
	var rules []pretool.Rule
	if enc, ok := lastEnv(l.env, rulesEnv); ok {
		var err error
		if rules, err = pretool.Decode(enc); err != nil {
			t.Fatalf("decode %s: %v", rulesEnv, err)
		}
	}
	var settings struct {
		Hooks map[string][]hookMatcher `json:"hooks"`
	}
	raw := flagValue(l.args, "--settings")
	if raw == "" {
		t.Fatalf("no --settings in argv %q: the hooks were not installed", l.args)
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("parse --settings: %v", err)
	}
	pre := settings.Hooks[eventPreToolUse]
	if len(pre) != 1 {
		t.Fatalf("PreToolUse entries = %d, want 1", len(pre))
	}
	return rules, pre[0].Matcher
}

// leafCall is one tool call a depth leaf might make.
type leafCall struct {
	name  string
	tool  string
	input string
}

// refusedInLeaf are the subagent and background paths requirements 1-2 of
// #2625 name for Claude Code.
var refusedInLeaf = []leafCall{
	{"Agent foreground", "Agent", `{"description":"x","prompt":"do it","subagent_type":"general-purpose"}`},
	{"Agent background", "Agent", `{"description":"x","prompt":"do it","run_in_background":true}`},
	{"Task (old Agent name)", "Task", `{"description":"x","prompt":"do it"}`},
	{"Monitor", "Monitor", `{"command":"tail -f log","description":"watch"}`},
	{"Workflow", "Workflow", `{"script":"export const meta = {}"}`},
	{"Bash run_in_background", "Bash", `{"command":"make test","run_in_background":true}`},
	{"Bash trailing &", "Bash", `{"command":"sleep 100 &"}`},
	{"Bash & mid-script", "Bash", `{"command":"cd /tmp && (long-job &) ; echo started"}`},
	{"Bash nohup", "Bash", `{"command":"nohup ./server > log"}`},
	{"Bash setsid --fork", "Bash", `{"command":"setsid -f ./server"}`},
	{"Bash disown", "Bash", `{"command":"./server & disown"}`},
}

// allowedInLeaf are calls requirement 5 keeps working in a leaf.
var allowedInLeaf = []leafCall{
	{"foci_spawn", "Bash", `{"command":"foci_spawn --mode explore 'look into X'"}`},
	{"foci_spawn --background", "Bash", `{"command":"foci_spawn --background 'look into X'"}`},
	{"foci_depth_report", "Bash", `{"command":"foci_depth_report 'done'"}`},
	{"foci_remind wake", "Bash", `{"command":"foci_remind --wake 'check' --in 10m"}`},
	{"foreground Bash", "Bash", `{"command":"make -C /tmp/x test && echo ok","timeout":600000}`},
	{"Bash run_in_background false", "Bash", `{"command":"ls","run_in_background":false}`},
	{"Bash quoted ampersand", "Bash", `{"command":"echo 'a & b' | grep -c '&'"}`},
	{"Read", "Read", `{"file_path":"/tmp/x"}`},
	{"Edit", "Edit", `{"file_path":"/tmp/x","old_string":"a","new_string":"b"}`},
	{"WebFetch", "WebFetch", `{"url":"https://example.com","prompt":"x"}`},
	{"TaskOutput", "TaskOutput", `{"task_id":"x"}`},
}

// TestDepthLeaf_RefusesSubagentPaths (#2625, requirements 1, 4, 5, 10): a
// depth leaf's CC process gets rules that refuse every subagent and
// background path with the foci_spawn message, and leave every other call
// alone. Read back from a spawned process, through the hook's own matcher.
func TestDepthLeaf_RefusesSubagentPaths(t *testing.T) {
	rules, matcher := rulesSeen(t, launchDepth(t, true, nil, nil))
	listed := map[string]bool{}
	for _, n := range strings.Split(matcher, "|") {
		listed[n] = true
	}
	for _, c := range refusedInLeaf {
		t.Run("refused/"+c.name, func(t *testing.T) {
			if !listed[c.tool] {
				t.Errorf("PreToolUse matcher %q does not include %s: CC never calls the hook for it", matcher, c.tool)
			}
			res := pretool.Match(rules, pretool.Call{Tool: c.tool, Input: json.RawMessage(c.input), Cwd: "/tmp"})
			if res.Rule == nil {
				t.Fatalf("%s %s was not refused in a depth leaf", c.tool, c.input)
			}
			for _, want := range []string{"refused in a depth leaf", "foci_spawn", "after this leaf reports"} {
				if !strings.Contains(res.Rule.Reason, want) {
					t.Errorf("refusal %q lacks %q", res.Rule.Reason, want)
				}
			}
		})
	}
	for _, c := range allowedInLeaf {
		t.Run("allowed/"+c.name, func(t *testing.T) {
			if res := pretool.Match(rules, pretool.Call{Tool: c.tool, Input: json.RawMessage(c.input), Cwd: "/tmp"}); res.Rule != nil {
				t.Errorf("%s %s refused in a depth leaf by %s; it must keep working", c.tool, c.input, res.Rule.Name)
			}
		})
	}
}

// TestDepthLeaf_RootKeepsEveryTool (#2625, requirements 6 and 10): the same
// calls in a session that is not a depth leaf meet no foci rule, and the
// agent's own rules reach it unchanged.
// Characterisation (factory): pins main's behaviour as of 40252ce60; not necessarily specified anywhere.
func TestDepthLeaf_RootKeepsEveryTool(t *testing.T) {
	own := pretool.Rule{Name: "own_rule", Tool: "Grep", Action: pretool.ActionDeny, Reason: "no"}
	rules, _ := rulesSeen(t, launchDepth(t, false, nil, []pretool.Rule{own}))
	if len(rules) != 1 || rules[0].Name != own.Name {
		t.Fatalf("root session rules = %+v, want only the agent's own rule", rules)
	}
	for _, c := range append(append([]leafCall{}, refusedInLeaf...), allowedInLeaf...) {
		if res := pretool.Match(rules, pretool.Call{Tool: c.tool, Input: json.RawMessage(c.input), Cwd: "/tmp"}); res.Rule != nil {
			t.Errorf("root session: %s %s refused by %s", c.tool, c.input, res.Rule.Name)
		}
	}
}

// TestDepthLeaf_AgentRulesStillApply (#2625, requirement 8): in a leaf the
// agent's own pretool rules still reach the hook, after the leaf's.
// Characterisation (factory): pins main's behaviour as of 40252ce60; not necessarily specified anywhere.
func TestDepthLeaf_AgentRulesStillApply(t *testing.T) {
	own := pretool.Rule{Name: "own_rule", Tool: "Grep", Action: pretool.ActionDeny, Reason: "no"}
	rules, matcher := rulesSeen(t, launchDepth(t, true, nil, []pretool.Rule{own}))
	res := pretool.Match(rules, pretool.Call{Tool: "Grep", Input: json.RawMessage(`{"pattern":"x"}`)})
	if res.Rule == nil || res.Rule.Name != own.Name {
		t.Errorf("agent rule not applied in a leaf: got %+v", res.Rule)
	}
	if !strings.Contains(matcher, "Grep") {
		t.Errorf("matcher %q lost the agent's ruled tool", matcher)
	}
}

// TestDepthLeaf_DisablesBackgroundTasks (#2625, requirement 2): a depth
// leaf's CC process runs with CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1, so CC
// never moves a foreground command to the background at its timeout (nor
// accepts run_in_background); the per-agent env cannot switch it off. A
// root session's process does not get it.
func TestDepthLeaf_DisablesBackgroundTasks(t *testing.T) {
	leaf := launchDepth(t, true, map[string]string{disableBackgroundTasksEnv: "0"}, nil)
	if v, ok := lastEnv(leaf.env, disableBackgroundTasksEnv); !ok || v != "1" {
		t.Errorf("leaf CC process: %s=%q (set %v), want \"1\"", disableBackgroundTasksEnv, v, ok)
	}
	root := launchDepth(t, false, nil, nil)
	if v, ok := lastEnv(root.env, disableBackgroundTasksEnv); ok {
		t.Errorf("root CC process got %s=%q; background tasks must stay available outside a leaf", disableBackgroundTasksEnv, v)
	}
}
