package nudge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"foci/internal/config"
	"foci/internal/delegator"
)

// validate.go is the one validator for nudge rules (#2223). Two callers use
// it: `foci check nudge` (ValidateFile: the whole file, strict about keys)
// and NewSchedulerOpts (RuleProblems: one WARN per broken character rule,
// keys stay tolerated at load). A rule it rejects never fires — today the
// scheduler drops a bad regex or an unknown trigger without a word.

// Severity says whether a Finding fails the check.
type Severity string

// Severities. An error is a rule (or file) that cannot work as written; a
// warning is a rule that is valid but will not run here, or a file the
// gateway will regenerate.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is one problem the validator found. Rule is the 0-based index of
// the rule in the file's "rules" array, or -1 for a file-level finding.
type Finding struct {
	Severity Severity
	Rule     int
	Text     string // the rule's text, truncated; empty for file-level findings
	Msg      string
}

// String renders a finding for the CLI: "error: rule 2 ("Verify bef..."): msg".
func (f Finding) String() string {
	return string(f.Severity) + ": " + f.where() + f.Msg
}

func (f Finding) where() string {
	if f.Rule < 0 {
		return ""
	}
	return ruleLabel(f.Rule, f.Text) + ": "
}

// ruleLabel names a rule by its index and a truncated text.
func ruleLabel(i int, text string) string {
	return fmt.Sprintf("rule %d (%q)", i, truncate(text, ruleTextLabelLen))
}

// ruleTextLabelLen is how much of a rule's text names it in a finding.
const ruleTextLabelLen = 40

// validTriggerTypes is every trigger type the scheduler can fire, in the
// order the error message lists them.
var validTriggerTypes = []string{"every_n_tools", "every_n_turns", "after_error", "regex", "pre_answer", "tool_pattern"}

// validPriorities is every priority a rule may carry.
var validPriorities = []string{"high", "medium", "low"}

// RuleProblems returns why rule r can never work as written, one reason per
// problem, or nil for a valid rule. It judges the decoded rule only: unknown
// keys are ValidateFile's concern. The scheduler warns with these; the CLI
// reports each as an error.
func RuleProblems(r Rule) []string {
	var out []string
	if strings.TrimSpace(r.Text) == "" {
		out = append(out, `"text" is empty`)
	}
	if !slices.Contains(validPriorities, r.Priority) {
		out = append(out, fmt.Sprintf(`priority %q is not one of %s`, r.Priority, strings.Join(validPriorities, ", ")))
	}
	t := r.Trigger
	switch t.Type {
	case "every_n_tools", "every_n_turns":
		if t.N <= 0 {
			out = append(out, fmt.Sprintf(`trigger %s needs "n" greater than 0 (got %d)`, t.Type, t.N))
		}
	case "regex":
		if t.Pattern == "" {
			out = append(out, `trigger regex has an empty "pattern"`)
		} else if _, err := regexp.Compile(t.Pattern); err != nil {
			out = append(out, fmt.Sprintf(`trigger regex "pattern" does not compile: %v`, err))
		}
	case "tool_pattern":
		if _, err := regexp.Compile(t.ToolPattern); t.ToolPattern != "" && err != nil {
			out = append(out, fmt.Sprintf(`"tool_pattern" does not compile: %v`, err))
		}
		if _, err := regexp.Compile(t.InputPattern); t.InputPattern != "" && err != nil {
			out = append(out, fmt.Sprintf(`"input_pattern" does not compile: %v`, err))
		}
	case "after_error", "pre_answer":
	default:
		out = append(out, fmt.Sprintf("trigger type %q is not one of %s", t.Type, strings.Join(validTriggerTypes, ", ")))
	}
	return out
}

// ValidateOpts is what ValidateFile judges the file against beyond its own
// content: the agent's backend capabilities (the same ones the gateway
// gives the scheduler, CapabilitiesFor) and its character files.
type ValidateOpts struct {
	CanPostTool  bool
	CanPreAnswer bool
	// WorkspaceDir and FileOrder locate the character files whose hash the
	// file's content_hash is compared with, exactly as the extractor
	// compares it. An empty FileOrder skips the comparison.
	WorkspaceDir string
	FileOrder    []string
}

// ValidateFile checks a nudge-rules.json body and returns every finding, in
// file order. Invalid JSON is one error and nothing else is judged. It reads
// the character files named by opts and nothing else; it writes nothing.
func ValidateFile(data []byte, opts ValidateOpts) []Finding {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return []Finding{{Severity: SeverityError, Rule: -1, Msg: fmt.Sprintf("invalid JSON: %v", err)}}
	}
	var rs RuleSet
	if err := json.Unmarshal(data, &rs); err != nil {
		// Valid JSON of the wrong shape (e.g. "rules" is a string).
		return []Finding{{Severity: SeverityError, Rule: -1, Msg: fmt.Sprintf("invalid JSON: %v", err)}}
	}

	var out []Finding
	fileErr := func(sev Severity, format string, args ...any) {
		out = append(out, Finding{Severity: sev, Rule: -1, Msg: fmt.Sprintf(format, args...)})
	}
	for _, k := range unknownKeys(raw, ruleSetKeys) {
		fileErr(SeverityError, "unknown key %q at top level", k)
	}

	var rawRules []map[string]json.RawMessage
	if r, ok := raw["rules"]; ok {
		// Each element decoded into RuleSet above, so each is an object.
		_ = json.Unmarshal(r, &rawRules)
	}
	for i, r := range rs.Rules {
		add := func(sev Severity, msg string) {
			out = append(out, Finding{Severity: sev, Rule: i, Text: r.Text, Msg: msg})
		}
		if i < len(rawRules) {
			for _, k := range unknownKeys(rawRules[i], ruleKeys) {
				add(SeverityError, fmt.Sprintf("unknown key %q", k))
			}
			var rawTrigger map[string]json.RawMessage
			if t, ok := rawRules[i]["trigger"]; ok && json.Unmarshal(t, &rawTrigger) == nil {
				for _, k := range unknownKeys(rawTrigger, triggerKeys) {
					add(SeverityError, fmt.Sprintf("unknown key %q in trigger", k))
				}
			}
		}
		for _, p := range RuleProblems(r) {
			add(SeverityError, p)
		}
		if TriggerRequiresPostTool(r.Trigger.Type) && !opts.CanPostTool {
			add(SeverityWarning, fmt.Sprintf("trigger %s needs post-tool injection, which this agent's backend does not support: the gateway skips this rule", r.Trigger.Type))
		}
		if TriggerRequiresPreAnswer(r.Trigger.Type) && !opts.CanPreAnswer {
			add(SeverityWarning, fmt.Sprintf("trigger %s needs pre-answer injection, which this agent's backend does not support: the gateway skips this rule", r.Trigger.Type))
		}
	}

	if len(opts.FileOrder) > 0 {
		if hash, ok := CharacterHash(opts.WorkspaceDir, opts.FileOrder); ok && hash != rs.ContentHash {
			fileErr(SeverityWarning, "content_hash does not match the current character files: the gateway will extract the rules again and overwrite this file")
		}
	}
	return out
}

// The JSON keys each level of the file may carry, read from the struct tags
// so a new field cannot be reported as unknown.
var (
	ruleSetKeys = jsonKeys(RuleSet{})
	ruleKeys    = jsonKeys(Rule{})
	triggerKeys = jsonKeys(Trigger{})
)

// jsonKeys returns the JSON names of v's struct fields (json:"-" excluded).
func jsonKeys(v any) map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// unknownKeys returns obj's keys that are not in allowed, sorted.
func unknownKeys(obj map[string]json.RawMessage, allowed map[string]bool) []string {
	var out []string
	for k := range obj {
		if !allowed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// CapabilitiesFor reports which mid-turn nudge triggers an agent's transport
// can deliver. Turn-start triggers (every_n_turns, regex) work everywhere;
// mid-turn triggers (every_n_tools, after_error, tool_pattern, pre_answer)
// need the transport to call back mid-turn. The API loop does both; a
// delegated backend does what it declares — the same declaration the turn
// reads when it arms the nudge funcs, so a rule the scheduler keeps can fire.
// The gateway's scheduler and `foci check nudge` both use it.
func CapabilitiesFor(acfg config.AgentConfig) (canPostTool, canPreAnswer bool) {
	if !acfg.IsDelegated() {
		return true, true
	}
	s, _ := delegator.SpecFor(acfg.Backend)
	return s.Supports(delegator.CapPostToolNudge), s.Supports(delegator.CapPreAnswerNudge)
}
