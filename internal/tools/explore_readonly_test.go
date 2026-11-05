package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/execguard"
)

// recordingTodo is a stand-in todo tool that records the actions it ran.
func recordingTodo(ran *[]string) *Tool {
	return &Tool{
		Name: "todo", ExecExport: true, Description: "todo.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
		Execute: func(_ context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(params, &p)
			*ran = append(*ran, p.Action)
			return TextResult("ok"), nil
		},
	}
}

// TestReadOnlyTodo_RefusesWrites is #2702 finding 2: explore's todo (the API
// tool set and a read-only batch's bridge) runs list, search and get (and
// get's alias show) and refuses every action that changes a todo, without
// reaching the store.
func TestReadOnlyTodo_RefusesWrites(t *testing.T) {
	t.Parallel()
	var ran []string
	reg := NewRegistry()
	reg.Register(recordingTodo(&ran))
	_, apiSet := spawnExploreToolSet(reg)
	bridge := RegistryForAccess(reg, delegator.BatchAccessReadOnly)
	for name, todo := range map[string]*Tool{"api explore": apiSet["todo"], "read-only bridge": bridge.Get("todo")} {
		if todo == nil {
			t.Fatalf("%s: no todo tool", name)
		}
		for _, a := range []string{"list", "search", "get", "show"} {
			ran = nil
			if _, err := todo.Execute(context.Background(), json.RawMessage(`{"action":"`+a+`"}`)); err != nil || len(ran) != 1 {
				t.Errorf("%s: todo %s = %v (ran %v), want it run", name, a, err, ran)
			}
		}
		for _, a := range []string{"add", "create", "complete", "drop", "reopen", "start", "transition", "edit", "update", "remove"} {
			ran = nil
			_, err := todo.Execute(context.Background(), json.RawMessage(`{"action":"`+a+`"}`))
			if err == nil || len(ran) != 0 {
				t.Errorf("%s: todo %s err=%v ran=%v, want refused before the store", name, a, err, ran)
			}
		}
	}
	// The agent's own todo is untouched.
	ran = nil
	if _, err := reg.Get("todo").Execute(context.Background(), json.RawMessage(`{"action":"add"}`)); err != nil || len(ran) != 1 {
		t.Errorf("agent's own todo add = %v (ran %v), want it run", err, ran)
	}
}

// TestExploreShellPrompt_ListsTheRules proves the delegated explore prompt's
// whitelist is generated from readOnlyShellRules (every Bash
// rule appears), names the bridge's functions, says everything else is
// unavailable, and invites the spawn to name a missing tool.
func TestExploreShellPrompt_ListsTheRules(t *testing.T) {
	t.Parallel()
	var ran []string
	reg := NewRegistry()
	reg.Register(recordingTodo(&ran))
	reg.Register(&Tool{Name: "memory_search", ExecExport: true})
	reg.Register(&Tool{Name: "send_to_chat", ExecExport: true}) // not explore-allowed
	p := delegatedExplorePrompt(reg)
	n := 0
	for _, r := range readOnlyShellRules() {
		if !strings.HasPrefix(r, "Bash:") {
			continue
		}
		c := autoapprove.DisplayRule(r)
		n++
		if !strings.Contains(p, "- `"+c+"`\n") {
			t.Errorf("prompt lacks whitelisted command %q", c)
		}
	}
	if n < 10 {
		t.Fatalf("only %d Bash rules found; the rule list changed shape", n)
	}
	for _, want := range []string{"`foci_memory_search`", "`foci_todo` (list, search and get only)", "Everything else is unavailable", "missing from this whitelist", exploreRolePrompt} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.Contains(p, "- `git (read-only: blame, branch,") {
		t.Error("prompt does not spell out the read-only git rule's subcommands")
	}
	if strings.Contains(p, "git (read-only)`") {
		t.Error("prompt lists the read-only git rule by its raw pattern")
	}
	if strings.Contains(p, "foci_send_to_chat") {
		t.Error("prompt lists foci_send_to_chat, which the read-only bridge does not carry")
	}
	if !strings.Contains(exploreSystemPrompt, "is missing") || strings.Contains(exploreSystemPrompt, "update the todo") {
		t.Error("API explore prompt lacks the missing-tool invitation or still offers todo writes")
	}
}

// TestReadOnlyBatchRules_DecideCommands proves the read-only rule set, run
// through the real matcher, allows whitelisted reads and the bridge's
// functions and denies writes and anything unlisted.
func TestReadOnlyBatchRules_DecideCommands(t *testing.T) {
	t.Parallel()
	var ran []string
	reg := NewRegistry()
	reg.Register(recordingTodo(&ran))
	reg.Register(&Tool{Name: "spawn", ExecExport: true}) // not explore-allowed
	rules := autoapprove.Compile(ReadOnlyBatchRules(reg))
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{"jq -r .a /tmp/f.jsonl | sort | uniq", true},
		{"jq -r .a /tmp/f.jsonl | sort", true},
		{"find /home -newermt 2026-10-21 -name '*.jsonl'", true},
		{"sqlite3 -readonly /tmp/x.db 'select 1'", true},
		{"foci_todo list", true},
		{"foci_spawn --mode raw x", false},
		{"sqlite3 /tmp/x.db 'delete from t'", false},
		{"rm -rf /tmp/x", false},
		{"sed -i s/a/b/ f", false},
		{"curl https://example.com", false},
		{"git log", true}, // the read-only git rule (#2734)
		{"git -C /tmp/r show HEAD", true},
		{"git log --output=/tmp/x", false},
		{"git branch newname", false},
		{"git commit -m x", false},
		{"go list -f {{.Dir}} ./...", true},
		{"go list -export -toolexec=/tmp/x ./...", false}, // #2736
		{"go vet --vettool=/tmp/x ./...", false},
		{"go env -w CC=/tmp/x", false},
	} {
		input, _ := json.Marshal(map[string]string{"command": c.cmd})
		if got, _ := autoapprove.MatchWithEnv(rules, "Bash", input, nil); got != c.want {
			t.Errorf("%q allowed = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestReadOnlyBatchRules_CDAndUniq proves the explore-only additions work in
// compound reads without broadening ordinary approvals or permitting writes.
func TestReadOnlyBatchRules_CDAndUniq(t *testing.T) {
	// This is a rule-policy test, not a check of the host's installed binaries
	// (e.g. Homebrew rg is substitutable). Keep it non-parallel while changing
	// the process-wide guard; parallel tests only run after cleanup restores it.
	enforced := !execguard.Live().Off
	execguard.SetEnforced(false)
	t.Cleanup(func() { execguard.SetEnforced(enforced) })
	explore := autoapprove.Compile(ReadOnlyBatchRules(nil))
	common := autoapprove.Compile(autoapprove.CommonReadonlyRules)
	for _, cmd := range []string{
		"cd /tmp && ls config data 2>/dev/null; ls test; wc -l README.md; cat render.yaml | head -50",
		"cd /tmp; rg -n needle test; cat README.md | sed -n 1,3p",
		"cat input | sort | uniq -c | sort -rn",
	} {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		if got, reason := autoapprove.MatchWithEnv(explore, "Bash", input, nil); !got {
			t.Errorf("explore denied %q: %s", cmd, reason)
		}
		if got, _ := autoapprove.MatchWithEnv(common, "Bash", input, nil); got {
			t.Errorf("ordinary rules newly allowed %q", cmd)
		}
	}
	for _, cmd := range []string{"cd /tmp; rm victim", "uniq a b", "cd /tmp; uniq a b", "sort input | uniq -c > output", "awk '{system($0)}' input"} {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		if got, _ := autoapprove.MatchWithEnv(explore, "Bash", input, nil); got {
			t.Errorf("explore allowed %q", cmd)
		}
	}
	// The matcher approves literal relative targets without tracking cd. This
	// demonstrates why putting cd into the shared list would broaden a normal
	// agent's path-scoped write permissions (nothing is actually executed).
	scoped := autoapprove.Compile(append(append([]string(nil), autoapprove.CommonReadonlyRules...), "Bash:cd", "Bash:rm victim"))
	input, _ := json.Marshal(map[string]string{"command": "cd /elsewhere; rm victim"})
	if got, _ := autoapprove.MatchWithEnv(scoped, "Bash", input, nil); !got {
		t.Error("cwd hazard characterization changed; reassess explore-only cd")
	}
	p := ExploreShellPrompt(nil)
	for _, entry := range []string{"- `cd`\n", "- `uniq`\n", "uniq INPUT OUTPUT"} {
		if !strings.Contains(p, entry) {
			t.Errorf("prompt missing %q", entry)
		}
	}
}
