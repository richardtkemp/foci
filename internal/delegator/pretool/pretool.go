// Package pretool is the rule engine behind foci's configurable Claude Code
// PreToolUse hook (#2028). Rules are data: each names a tool, optionally
// constrains fields of that tool's input with regexes, and carries an action.
// The only action is "deny" — the hook refuses the call and hands the rule's
// reason to the model in place of the tool result.
//
// There is deliberately no "allow". A PreToolUse allow short-circuits CC's
// permission check, so emitting one would let a rule bypass foci's approval
// flow; a rule that doesn't match simply stays silent and the normal permission
// path runs.
//
// The package is a leaf (stdlib only) because it is linked into the
// foci-cc-hook helper, which CC spawns once per matched tool call.
package pretool

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// ActionDeny refuses the tool call and returns Reason to the model.
const ActionDeny = "deny"

// Rule is one PreToolUse rule. The same struct is the TOML config shape
// ([[cc_backend.pretool_rules]] / [[agents.backend_config.pretool_rules]]) and
// the JSON wire shape handed to the hook binary.
//
// Rules merge by Name across layers (built-in defaults, then global, then
// per-agent): a later layer's non-empty fields override the earlier rule's, so
// `{name = "ask_user_question", enabled = false}` switches a default off and
// `{name = "cron_create", reason = "..."}` rewords one without restating it.
type Rule struct {
	Name    string            `toml:"name"    json:"name"`
	Tool    string            `toml:"tool"    json:"tool"`
	Input   map[string]string `toml:"input"   json:"input,omitempty"`
	Action  string            `toml:"action"  json:"action,omitempty"`
	Reason  string            `toml:"reason"  json:"reason"`
	Enabled *bool             `toml:"enabled" json:"-"`
}

// Defaults are the rules foci ships preinstalled. Config disables one with
// `enabled = false` under the same name.
var Defaults = []Rule{
	{
		Name:   "ask_user_question",
		Tool:   "AskUserQuestion",
		Action: ActionDeny,
		Reason: "AskUserQuestion is disabled in foci. To ask the user something, use the foci_ask shell " +
			"function instead: it delivers the question to the user's chat and returns their answer.",
	},
	{
		Name:   "cron_create",
		Tool:   "CronCreate",
		Action: ActionDeny,
		Reason: "CronCreate is disabled in foci: its jobs live only in this session and are lost on restart, " +
			"reload or compaction. For REPEATING events use crontab (see the crontab-edit skill). " +
			"For SINGLE (one-shot) events use the foci_remind shell function.",
	},
}

// toolNameRe is the shape CC's hook matcher treats as an exact name list when
// joined with "|" (anything else is compiled as a regex). Holding tool names
// to it keeps the generated matcher an exact list.
var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ValidateLayer checks one config layer's rules in isolation. A rule may omit
// tool/action/reason when it overrides a rule of the same name from an earlier
// layer, so completeness is checked after merging (Resolve), not here.
func ValidateLayer(rules []Rule) error {
	seen := map[string]bool{}
	for i, r := range rules {
		if r.Name == "" {
			return fmt.Errorf("pretool_rules[%d]: name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("pretool_rules %q: duplicate name", r.Name)
		}
		seen[r.Name] = true
		if r.Tool != "" && !toolNameRe.MatchString(r.Tool) {
			return fmt.Errorf("pretool_rules %q: tool %q must be an exact tool name (letters, digits, underscore)", r.Name, r.Tool)
		}
		if r.Action != "" && r.Action != ActionDeny {
			return fmt.Errorf("pretool_rules %q: action %q unsupported (only %q)", r.Name, r.Action, ActionDeny)
		}
		for field, pat := range r.Input {
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("pretool_rules %q: input.%s: %w", r.Name, field, err)
			}
		}
	}
	return nil
}

// Resolve merges layers by name (later layers override earlier ones field by
// field), drops disabled rules, and returns the enabled rules in first-seen
// order. A rule left incomplete by the merge (no tool or reason, or an invalid
// regex) is returned in skipped with the reason, rather than failing the
// whole set: one bad rule must not strip the defaults.
func Resolve(layers ...[]Rule) (rules []Rule, skipped []string) {
	var order []string
	byName := map[string]Rule{}
	for _, layer := range layers {
		for _, r := range layer {
			prev, ok := byName[r.Name]
			if !ok {
				order = append(order, r.Name)
				byName[r.Name] = r
				continue
			}
			byName[r.Name] = overlay(prev, r)
		}
	}
	for _, name := range order {
		r := byName[name]
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		if r.Action == "" {
			r.Action = ActionDeny
		}
		if err := ValidateLayer([]Rule{r}); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if r.Tool == "" || r.Reason == "" {
			skipped = append(skipped, fmt.Sprintf("pretool_rules %q: tool and reason are required", name))
			continue
		}
		rules = append(rules, r)
	}
	return rules, skipped
}

func overlay(base, over Rule) Rule {
	if over.Tool != "" {
		base.Tool = over.Tool
	}
	if over.Input != nil {
		base.Input = over.Input
	}
	if over.Action != "" {
		base.Action = over.Action
	}
	if over.Reason != "" {
		base.Reason = over.Reason
	}
	if over.Enabled != nil {
		base.Enabled = over.Enabled
	}
	return base
}

// ToolNames returns the distinct tool names the rules match, sorted — the
// set the PreToolUse hook matcher must cover.
func ToolNames(rules []Rule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if !seen[r.Tool] {
			seen[r.Tool] = true
			out = append(out, r.Tool)
		}
	}
	sort.Strings(out)
	return out
}

// Encode serialises rules for the hook command line. base64url keeps the
// argument free of shell metacharacters, so the command string needs no
// quoting beyond what it already has.
func Encode(rules []Rule) (string, error) {
	body, err := json.Marshal(rules)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// Decode is Encode's inverse.
func Decode(s string) ([]Rule, error) {
	body, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := json.Unmarshal(body, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// Match returns the first rule that denies this call, or nil. toolInput is
// the raw tool_input object from the hook payload.
//
// Input constraints are ANDed: every listed field must be present and its
// value must match the regex. A string field is matched as-is; any other JSON
// value is matched against its JSON text. A rule whose regex fails to
// compile never matches (Resolve already rejected it; this is the hook's own
// guard against a hand-edited command line).
func Match(rules []Rule, toolName string, toolInput json.RawMessage) *Rule {
	var fields map[string]json.RawMessage
	for i := range rules {
		r := &rules[i]
		if r.Tool != toolName || r.Action != ActionDeny {
			continue
		}
		if len(r.Input) > 0 && fields == nil {
			fields = map[string]json.RawMessage{}
			_ = json.Unmarshal(toolInput, &fields)
		}
		if inputMatches(r.Input, fields) {
			return r
		}
	}
	return nil
}

func inputMatches(want map[string]string, fields map[string]json.RawMessage) bool {
	for field, pat := range want {
		raw, ok := fields[field]
		if !ok {
			return false
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return false
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			s = string(raw)
		}
		if !re.MatchString(s) {
			return false
		}
	}
	return true
}
