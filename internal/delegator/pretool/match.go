package pretool

import "encoding/json"

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

// Match returns the first rule that denies this call, or nil.
//
// A rule's constraints are ANDed: every input field it lists must be present
// and match, its command patterns (if any) must match some command in the
// script, and its cwd patterns (if any) must match the cwd. Within one
// constraint any listed pattern may match. A string input field is matched
// as-is; any other JSON value is matched against its JSON text. A pattern that
// fails to compile never matches (Resolve already rejected it; this is the
// hook's own guard against a hand-edited command line).
func Match(rules []Rule, c Call) *Rule {
	var m matcher
	for i := range rules {
		r := &rules[i]
		if r.Tool != c.Tool || r.Action != ActionDeny {
			continue
		}
		if m.matches(r, c) {
			return r
		}
	}
	return nil
}

// matcher lazily decodes the parts of a call that rules need, once per call.
type matcher struct {
	fields     map[string]json.RawMessage
	cmds       []string
	cmdsParsed bool
}

func (m *matcher) matches(r *Rule, c Call) bool {
	if len(r.Input) > 0 || len(r.Command) > 0 {
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
	if len(r.Command) > 0 {
		res, ok := r.Command.compile(commandWrap)
		if !ok || !anyMatch(res, m.commands()...) {
			return false
		}
	}
	if len(r.Cwd) > 0 {
		res, ok := r.Cwd.compile("")
		if !ok || c.Cwd == "" || !anyMatch(res, c.Cwd) {
			return false
		}
	}
	return true
}

func (m *matcher) commands() []string {
	if !m.cmdsParsed {
		m.cmdsParsed = true
		if raw, ok := m.fields["command"]; ok {
			var script string
			if json.Unmarshal(raw, &script) == nil {
				m.cmds, _ = Commands(script)
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
