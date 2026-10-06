// examples.go is the per-rule test-example half of the rule engine (#2039).
// A rule can carry the calls it was written for — deny_examples it must stop,
// allow_examples it must leave alone — so a rule that stops matching what it
// was written for is caught by `foci pretool test --all` and by
// `foci-gw -check-config` instead of by a live agent misbehaving.
//
// Examples are checked against a RESOLVED rule set, with the same matching
// the gateway uses (Match), because what an example proves is what the next
// CC session will enforce: every layer merged, disabled rules dropped,
// defaults included. A deny example passes only if the set's matching rule
// is its own rule — a different rule matching first means the example now
// falls to that rule, which may not be what the author intended.
//
// WhenMode splits the two callers by what they may do with a rule's
// `when` script. `foci pretool test --all` runs as the invoking user and
// evaluates when-checks for real (WhenRun). `foci-gw -check-config` runs as
// ROOT during deploys and the scripts come from a config file the foci user
// can write, so it must never execute one (WhenSkip): rules with a when
// script get a patterns-only check and a WARN instead.

package pretool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExampleKindDeny and ExampleKindAllow name an outcome's example kind.
const (
	ExampleKindDeny  = "deny"
	ExampleKindAllow = "allow"
)

// Example is one test case of a rule (#2039). In config it is written
// either as a plain string — a bash command for a Bash rule, the tool_input
// JSON object for any other tool — or as the Bash-only table
// `{ cmd = "...", cwd = "/abs/path" }` when the working directory the call
// is made from matters (when rules and cwd patterns).
type Example struct {
	// Text is the string form verbatim (the bash command, or the
	// tool_input JSON), or the table form's cmd.
	Text string
	// Cwd is the table form's cwd; empty means the current directory.
	Cwd string

	// table records that the {cmd, cwd} form was used (Bash rules only).
	table bool
	// unknown lists keys seen in the table form that we don't decode, so
	// validation can name them. They do surface in the config's undefined
	// keys too, but that generic error cannot name the RULE, so config's
	// unknown-key check skips the example subtrees and this field is what
	// turns a typoed table key into a rule-named error (#2047 for examples).
	unknown []string
}

// String renders the example for reports: the command or JSON text, with
// the table form's cwd appended.
func (ex Example) String() string {
	if ex.Cwd != "" {
		return fmt.Sprintf("%s (cwd: %s)", ex.Text, ex.Cwd)
	}
	return ex.Text
}

// Examples is the deny_examples / allow_examples field: a list of Example.
type Examples []Example

// UnmarshalTOML implements toml.Unmarshaler. Each entry is a string or a
// {cmd, cwd} table; anything else is a decode error. Shape errors that need
// the rule's tool (a table on a non-Bash rule, a non-Bash string that is not
// the tool_input JSON) are left to validation, which names the rule.
func (e *Examples) UnmarshalTOML(v any) error {
	x, ok := v.([]any)
	if !ok {
		return fmt.Errorf("want an array of examples, got %T", v)
	}
	out := make(Examples, 0, len(x))
	for i, entry := range x {
		switch ev := entry.(type) {
		case string:
			out = append(out, Example{Text: ev})
		case map[string]any:
			ex := Example{table: true}
			for k, val := range ev {
				switch k {
				case "cmd":
					s, ok := val.(string)
					if !ok {
						return fmt.Errorf("example %d: cmd: want a string, got %T", i, val)
					}
					ex.Text = s
				case "cwd":
					s, ok := val.(string)
					if !ok {
						return fmt.Errorf("example %d: cwd: want a string, got %T", i, val)
					}
					ex.Cwd = s
				default:
					ex.unknown = append(ex.unknown, k)
				}
			}
			out = append(out, ex)
		default:
			return fmt.Errorf("example %d: want a string or a {cmd, cwd} table, got %T", i, entry)
		}
	}
	*e = out
	return nil
}

// ValidateRuleExamples checks one rule's examples. tool is the rule's tool
// as known at the call site; empty (a pure override layer) limits the check
// to the tool-independent rules (table without cmd, unknown table keys).
// It is deliberately NOT part of ValidateLayer: Resolve calls ValidateLayer
// on merged rules and skips what fails, while a malformed example must fail
// the config load naming its rule (config.validatePreToolRules calls this
// per layer, then ValidateExamples on every agent's resolved set).
func ValidateRuleExamples(tool, ruleName string, deny, allow Examples) error {
	for label, exs := range map[string]Examples{"deny_examples": deny, "allow_examples": allow} {
		for i, ex := range exs {
			if err := validateExample(tool, ex); err != nil {
				return fmt.Errorf("pretool_rules %q: %s[%d]: %w", ruleName, label, i, err)
			}
		}
	}
	return nil
}

// ValidateExamples checks every rule's examples in a resolved rule set,
// where each rule's tool is known. Called from config validation after the
// layer merge, so an override that only misshapes after merging (a table
// example added to a rule that resolves to a non-Bash tool) is a config
// error too.
func ValidateExamples(rules []Rule) error {
	for _, r := range rules {
		if err := ValidateRuleExamples(r.Tool, r.Name, r.DenyExamples, r.AllowExamples); err != nil {
			return err
		}
	}
	return nil
}

func validateExample(tool string, ex Example) error {
	if ex.table {
		if strings.TrimSpace(ex.Text) == "" {
			return fmt.Errorf("the { cmd, cwd } form needs a cmd (a non-empty command string)")
		}
		if len(ex.unknown) > 0 {
			return fmt.Errorf("unknown key(s) %s (the table form is { cmd = \"...\", cwd = \"/abs/path\" })", strings.Join(ex.unknown, ", "))
		}
		if tool != "" && tool != bashTool {
			return fmt.Errorf("the { cmd, cwd } form applies only to tool %q, not %q", bashTool, tool)
		}
		return nil
	}
	if tool != "" && tool != bashTool && !json.Valid([]byte(ex.Text)) {
		return fmt.Errorf("not valid JSON (a non-Bash example is the tool's input object, e.g. \"{\\\"file_path\\\":\\\"/etc/passwd\\\"}\")")
	}
	return nil
}

// WhenMode says what RunExamples does with a rule's when-check.
type WhenMode int

const (
	// WhenRun evaluates when-checks for real (`foci pretool test --all`,
	// which runs as the invoking user), so each when rule's decision is
	// the one the next session will make.
	WhenRun WhenMode = iota
	// WhenSkip never executes a when-check (`foci-gw -check-config`, which
	// runs as root during deploys; the scripts come from a config file the
	// foci user can write).
	WhenSkip
)

// ExampleOutcome is RunExamples' verdict on one example.
type ExampleOutcome struct {
	// Rule is the name of the rule whose example this is.
	Rule string
	// Kind is ExampleKindDeny or ExampleKindAllow.
	Kind string
	// Example is the example's display text.
	Example string
	// Pass reports whether the example held. Never a verdict on its own
	// for WhenUnknown outcomes.
	Pass bool
	// Matched names the rule that matched the example instead of the
	// expected outcome: the thief of a failed deny example, the (legally
	// matching) other rule of an allow example, or empty for no match.
	Matched string
	// WhenUnknown marks outcomes that could not be decided without running
	// a when-script (WhenSkip on a rule that has one). Informational, never
	// a failure: check-config reports them as a WARN.
	WhenUnknown bool
	// WhenErrors lists when-checks that failed open while deciding
	// (WhenRun only; WhenSkip never runs one).
	WhenErrors []WhenError
}

// RunExamples runs every example of every rule in a resolved rule set, in
// set order, each rule's deny examples then its allow examples. Rules not
// in the set (disabled, or dropped by the merge) are never run — only the
// rules handed in are iterated.
func RunExamples(rules []Rule, mode WhenMode) []ExampleOutcome {
	// whenless is the subset WhenSkip scans for rules without a when
	// script: a script that cannot run must neither steal nor confirm a
	// match, so its rule can take no part in the first-match scan.
	var whenless []Rule
	if mode == WhenSkip {
		for _, r := range rules {
			if r.When == "" {
				whenless = append(whenless, r)
			}
		}
	}
	var out []ExampleOutcome
	for i := range rules {
		r := &rules[i]
		for _, kind := range []string{ExampleKindDeny, ExampleKindAllow} {
			exs := r.DenyExamples
			if kind == ExampleKindAllow {
				exs = r.AllowExamples
			}
			for _, ex := range exs {
				out = append(out, runExample(rules, whenless, r, kind, ex, mode))
			}
		}
	}
	return out
}

// runExample checks one example against the set. deny passes only if the
// set's matching rule is r itself; allow passes only if it is not.
func runExample(rules, whenless []Rule, r *Rule, kind string, ex Example, mode WhenMode) ExampleOutcome {
	o := ExampleOutcome{Rule: r.Name, Kind: kind, Example: ex.String()}
	deny := kind == ExampleKindDeny
	if mode == WhenSkip && r.When != "" {
		// Patterns only: the script that decides this rule cannot be run,
		// so the outcome is the rule's constraints minus when.
		o.WhenUnknown = true
		o.Pass = patternMatch(r, ex.call(r.Tool)) == deny
		return o
	}
	scan := rules
	if mode == WhenSkip {
		scan = whenless
	}
	res := Match(scan, ex.call(r.Tool))
	o.WhenErrors = res.WhenErrors
	if res.Rule != nil {
		o.Matched = res.Rule.Name
	}
	mine := res.Rule != nil && res.Rule.Name == r.Name
	if deny {
		o.Pass = mine
		if mine {
			o.Matched = ""
		}
	} else {
		o.Pass = !mine
	}
	return o
}

// call builds the tool call an example stands for, the same way
// `foci pretool test` builds one: a Bash example is its command, a table
// example additionally the cwd the call is made from; any other tool's
// example is the tool_input JSON itself (validation proved it parses).
func (ex Example) call(tool string) Call {
	if tool == bashTool {
		enc, _ := json.Marshal(map[string]string{"command": ex.Text})
		return Call{Tool: tool, Input: enc, Cwd: ex.Cwd}
	}
	return Call{Tool: tool, Input: json.RawMessage(ex.Text)}
}
