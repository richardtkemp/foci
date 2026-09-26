package pretool

import (
	"context"
	"encoding/json"
	"fmt"
)

// bashTool is the one tool whose input has a shell command for Command
// patterns to parse.
const bashTool = "Bash"

// commandWrap anchors a command pattern at the command's first word.
const commandWrap = `^(?:%s)`

// Call is one tool call as the PreToolUse hook sees it.
type Call struct {
	Tool string
	// Input is the raw tool_input object from the hook payload.
	Input json.RawMessage
	// Cwd is the payload's top-level cwd: the shell's working directory when
	// the call was made, so it follows an earlier call's `cd` but not a `cd`
	// inside this same command (probed live, CC 2.1.280, #2033).
	Cwd string
}

// Result is Match's verdict on one call.
type Result struct {
	// Rule is the rule that denies the call, or nil.
	Rule *Rule
	// WhenErrors lists when-checks that failed open while deciding.
	WhenErrors []WhenError
}

// Match returns the first rule that denies this call.
//
// A rule's constraints are ANDed: every input field it lists must be present
// and match, some command in the script must match its command patterns and
// have the background/subshell/output facts it sets (if it sets any of
// those), its cwd patterns (if any) must match the cwd, and its when-check
// (if any) must exit 0. The when-check runs last, only once everything else
// holds (see when.go). Within one pattern constraint any listed pattern may
// match. A string input field is matched as-is; any other JSON value is
// matched against its JSON text. A pattern that fails to compile never
// matches (Resolve already rejected it; this is the hook's own guard against
// a hand-edited command line).
func Match(rules []Rule, c Call) Result {
	m := matcher{call: c}
	defer m.close()
	for i := range rules {
		r := &rules[i]
		if r.Tool != c.Tool || r.Action != ActionDeny {
			continue
		}
		if m.matches(r) {
			return Result{Rule: r, WhenErrors: m.whenErrs}
		}
	}
	return Result{WhenErrors: m.whenErrs}
}

// matcher lazily decodes the parts of a call that rules need, once per call.
type matcher struct {
	call         Call
	fields       map[string]json.RawMessage
	script       Script
	scriptParsed bool

	// budget bounds every when-check for this call, from the first one.
	budget   context.Context
	cancel   context.CancelFunc
	env      []string
	whenErrs []WhenError
}

func (m *matcher) close() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *matcher) matches(r *Rule) bool {
	c := m.call
	cmdScoped := len(r.commandFields()) > 0
	if len(r.Input) > 0 || cmdScoped || r.When != "" {
		if m.fields == nil {
			m.fields = map[string]json.RawMessage{}
			_ = json.Unmarshal(c.Input, &m.fields)
		}
	}
	for field, pats := range r.Input {
		raw, ok := m.fields[field]
		if !ok {
			return false
		}
		res, ok := pats.compile("")
		if !ok || !anyMatch(res, fieldText(raw)) {
			return false
		}
	}
	// matched are the commands the when-check runs for, once each: the
	// commands the rule's per-command constraints select, or a single nil
	// (no command) when it has none.
	matched := []*Command{nil}
	if cmdScoped {
		res, ok := r.Command.compile(commandWrap)
		if !ok {
			return false
		}
		matched = nil
		cmds := m.commands()
		for i := range cmds {
			cmd := &cmds[i]
			if (len(res) == 0 || anyMatch(res, cmd.Text)) && factsMatch(r, cmd) {
				matched = append(matched, cmd)
			}
		}
		if len(matched) == 0 {
			return false
		}
	}
	if len(r.Cwd) > 0 {
		res, ok := r.Cwd.compile("")
		if !ok || c.Cwd == "" || !anyMatch(res, c.Cwd) {
			return false
		}
	}
	if r.When == "" {
		return true
	}
	for _, cmd := range matched {
		if m.when(r, cmd) {
			return true
		}
	}
	return false
}

// factsMatch reports whether cmd has every fact value r sets.
func factsMatch(r *Rule, cmd *Command) bool {
	for _, f := range []struct {
		want *bool
		got  bool
	}{{r.Background, cmd.Background}, {r.Subshell, cmd.Subshell}, {r.Output, cmd.Output}} {
		if f.want != nil && *f.want != f.got {
			return false
		}
	}
	return true
}

// when runs r's when-check for cmd (nil when the rule is not about one
// command), recording a failure. A check that would start after the call's
// budget is spent is reported, not run.
func (m *matcher) when(r *Rule, cmd *Command) bool {
	if m.budget == nil {
		m.budget, m.cancel = context.WithTimeout(context.Background(), whenBudget)
		m.env = whenEnv(m.call, m.fields)
		if m.call.Tool == bashTool {
			m.commands()
			m.env = append(m.env, scriptEnv(m.script, m.call.Cwd)...)
		}
	}
	if m.budget.Err() != nil {
		m.whenErrs = append(m.whenErrs, WhenError{Rule: r.Name, Err: fmt.Errorf("not run: the call's %s budget for when-checks is spent", whenBudget)})
		return false
	}
	env, args := m.env, []string(nil)
	if cmd != nil {
		args = cmd.Args
		env = append(env[:len(env):len(env)], commandEnv(cmd, m.call.Cwd)...)
	}
	deny, err := runWhen(m.budget, r.Name, r.When, args, m.call, env)
	if err != nil {
		m.whenErrs = append(m.whenErrs, WhenError{Rule: r.Name, Err: err})
	}
	return deny
}

// commands parses the call's command once, and returns its commands (none
// if it does not parse).
func (m *matcher) commands() []Command {
	if !m.scriptParsed {
		m.scriptParsed = true
		if raw, ok := m.fields["command"]; ok {
			var script string
			if json.Unmarshal(raw, &script) == nil {
				m.script, _ = Parse(script)
			}
		}
	}
	return m.script.Commands
}

func fieldText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return string(raw)
	}
	return s
}
