// Tests for #3085: every flag a foci_todo action ACCEPTS must reach the
// server. The per-action allowlist (todoActions) and the per-action request
// builder in the generated wrapper are written separately, so a flag could be
// accepted by the parser and then silently dropped from the request. #2817
// added --status to search's allowlist but not to search's request builder:
// `foci_todo search codex --status open` returned done and dropped items,
// because the server saw no status and applied search's default ("all").
// This test walks every (action, accepted flag) pair, so a flag added to an
// allowlist without its forwarding line fails here.
package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// todoFlagForwardCase says how to exercise one accepted flag and which
// request param it must produce. value "" means a boolean flag.
type todoFlagForwardCase struct {
	value   string
	wantKey string
}

// todoFlagNotForwarded lists accepted flags that are deliberately NOT a
// request param, with the reason. Anything else must be forwarded.
var todoFlagNotForwarded = map[string]string{
	"--format": "output form only: applied by the wrapper's env, never sent",
}

// todoFlagForward maps each accepted flag to its probe value and the param
// key it must set. Per-action entries ("action --flag") override the default
// ("--flag"), for aliases whose target param differs by action.
var todoFlagForward = map[string]todoFlagForwardCase{
	"--tag":      {"x", "tag"},
	"--status":   {"open", "status"},
	"--priority": {"high", "priority"},
	"--sort":     {"created", "sort"},
	"--reverse":  {"", "reverse"},
	"--limit":    {"3", "limit"},
	"--truncate": {"7", "truncate"},
	"--query":    {"needle", "query"},
	"--id":       {"5", "id"},
	"--ids":      {"1,2", "ids"},
	"--reason":   {"why", "reason"},
	"--title":    {"Head", "title"},
	"--text":     {"words", "text"},
	"--body":     {"long body", "text"},
	"--append":   {"", "append"},
	"--add-tag":  {"x", "add_tag"},
	// On complete/drop, --text/--note/--notes alias --reason.
	"complete --text":  {"why", "reason"},
	"complete --note":  {"why", "reason"},
	"complete --notes": {"why", "reason"},
	"drop --text":      {"why", "reason"},
	"drop --note":      {"why", "reason"},
	"drop --notes":     {"why", "reason"},
	// On edit, the append aliases become text with append set.
	"edit --append-text": {"more", "text"},
	"edit --add":         {"more", "text"},
	"edit --note":        {"more", "text"},
	"edit --notes":       {"more", "text"},
	"--remove-tag":       {"x", "remove_tag"},
}

// todoFlagForwardBase returns the arguments that make a call for action
// valid apart from the flag under test, avoiding a clash with that flag (an
// id given both positionally and by --id, the query given twice, ...).
func todoFlagForwardBase(action, flag string) string {
	switch action {
	case "add":
		if flag == "--text" || flag == "--body" {
			return ""
		}
		return "--text base"
	case "search":
		if flag == "--query" {
			return ""
		}
		return "base"
	case "get", "complete", "drop", "reopen", "start", "edit", "remove":
		if flag == "--id" || flag == "--ids" {
			return ""
		}
		return "9"
	}
	return ""
}

func TestTodoShellForwardsEveryAcceptedFlag(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	for _, a := range todoActions {
		for _, flag := range strings.Fields(a.Flags) {
			if _, skip := todoFlagNotForwarded[flag]; skip {
				continue
			}
			c, ok := todoFlagForward[a.Name+" "+flag]
			if !ok {
				c, ok = todoFlagForward[flag]
			}
			if !ok {
				t.Errorf("%s %s: accepted but has no forwarding case here; add one to todoFlagForward or a reason to todoFlagNotForwarded", a.Name, flag)
				continue
			}
			cmd := "foci_todo " + a.Name
			if base := todoFlagForwardBase(a.Name, flag); base != "" {
				cmd += " " + base
			}
			cmd += " " + flag
			if c.value != "" {
				cmd += " '" + c.value + "'"
			}
			req := todoShellSingleCall(t, body, cmd)
			var got struct {
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(req), &got); err != nil {
				t.Errorf("%s: request is not JSON: %v\nreq=%s", cmd, err, req)
				continue
			}
			if _, ok := got.Params[c.wantKey]; !ok {
				t.Errorf("%s: flag accepted but %q missing from request (flag silently dropped)\nreq=%s", cmd, c.wantKey, req)
			}
		}
	}
}

// TestTodoShellSearchStatusForwarded pins the exact #3085 repro payload.
func TestTodoShellSearchStatusForwarded(t *testing.T) {
	t.Parallel()
	body := todoIDShellBody(t)
	got := todoShellSingleCall(t, body, `foci_todo search --query codex --status open --limit 30`)
	want := `{"tool":"todo","params":{"action":"search","query":"codex","status":"open","limit":30}}`
	if got != want {
		t.Errorf("request = %s, want exactly %s", got, want)
	}
}
