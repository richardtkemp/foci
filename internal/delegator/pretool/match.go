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
// and match, its command patterns (if any) must match some command in the
// script, its cwd patterns (if any) must match the cwd, and its when-check
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
	call       Call
	fields     map[string]json.RawMessage
	cmds       []shellCmd
	cmdsParsed bool

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
	if len(r.Input) > 0 || len(r.Command) > 0 || r.When != "" {
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
	// argSets are the positional parameters for the when-check: one set per
	// matched command, or a single empty set when the rule has no command
	// patterns.
	argSets := [][]string{nil}
	if len(r.Command) > 0 {
		res, ok := r.Command.compile(commandWrap)
		if !ok {
			return false
		}
		argSets = nil
		for _, cmd := range m.commands() {
			if anyMatch(res, cmd.text) {
				argSets = append(argSets, cmd.args)
			}
		}
		if len(argSets) == 0 {
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
	for _, args := range argSets {
		if m.when(r, args) {
			return true
		}
	}
	return false
}

// when runs r's when-check with args, recording a failure. A check that
// would start after the call's budget is spent is reported, not run.
func (m *matcher) when(r *Rule, args []string) bool {
	if m.budget == nil {
		m.budget, m.cancel = context.WithTimeout(context.Background(), whenBudget)
		m.env = whenEnv(m.call, m.fields)
	}
	if m.budget.Err() != nil {
		m.whenErrs = append(m.whenErrs, WhenError{Rule: r.Name, Err: fmt.Errorf("not run: the call's %s budget for when-checks is spent", whenBudget)})
		return false
	}
	deny, err := runWhen(m.budget, r.Name, r.When, args, m.call, m.env)
	if err != nil {
		m.whenErrs = append(m.whenErrs, WhenError{Rule: r.Name, Err: err})
	}
	return deny
}

func (m *matcher) commands() []shellCmd {
	if !m.cmdsParsed {
		m.cmdsParsed = true
		if raw, ok := m.fields["command"]; ok {
			var script string
			if json.Unmarshal(raw, &script) == nil {
				m.cmds, _ = parseCommands(script)
			}
		}
	}
	return m.cmds
}

func fieldText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return string(raw)
	}
	return s
}
