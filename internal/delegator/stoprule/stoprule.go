// Package stoprule is the rule engine behind foci's Claude Code Stop hook
// (#2089). It catches a turn that ends by announcing work ("starting the
// build now", "I'm sending it") when nothing was started: the hook reads the
// turn's final assistant text and the turn's transcript, and when the text
// matches a rule and the turn launched no background job, it blocks the stop
// and hands the rule's reason to the agent, which then continues the turn.
//
// A rule is data: a name, one or more regexes matched against the final
// assistant text, and a reason. There are no preinstalled rules; an agent
// opts in with [[agents.backend_config.stop_rules]].
//
// The check can block at most once per turn: CC sets stop_hook_active on
// the Stop that follows a block, and the hook always allows that one.
//
// Like pretool, the package imports only the stdlib (and pretool's Patterns
// type) because it is linked into the foci-cc-hook helper.
package stoprule

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"foci/internal/delegator/pretool"
)

// DefaultReason is used when a rule gives no reason. MatchPlaceholder in a
// reason is replaced by the text the rule matched.
const DefaultReason = `Your reply says you are starting something ("{match}"), but nothing is running. ` +
	"Start it now, or say plainly that it is deferred."

// MatchPlaceholder in a rule's reason is replaced by the matched text.
const MatchPlaceholder = "{match}"

// Rule is one Stop rule. The same struct is the TOML config shape
// ([[agents.backend_config.stop_rules]]) and the JSON wire shape handed to
// the hook binary.
type Rule struct {
	Name string `toml:"name" json:"name"`
	// Text holds regexes matched against the turn's final assistant text;
	// any one may match. Write (?i) for a case-insensitive pattern.
	Text    pretool.Patterns `toml:"text"    json:"text"`
	Reason  string           `toml:"reason"  json:"reason,omitempty"`
	Enabled *bool            `toml:"enabled" json:"-"`
}

// ValidateLayer checks one config layer's rules: every rule needs a unique
// name, at least one text pattern, and patterns that compile.
func ValidateLayer(rules []Rule) error {
	seen := map[string]bool{}
	for i, r := range rules {
		if r.Name == "" {
			return fmt.Errorf("stop_rules[%d]: name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("stop_rules %q: duplicate name", r.Name)
		}
		seen[r.Name] = true
		if len(r.Text) == 0 {
			return fmt.Errorf("stop_rules %q: text is required", r.Name)
		}
		for _, pat := range r.Text {
			if pat == "" {
				return fmt.Errorf("stop_rules %q: text: empty pattern matches every reply", r.Name)
			}
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("stop_rules %q: text: %w", r.Name, err)
			}
		}
	}
	return nil
}

// Resolve drops disabled rules and returns the rest. A rule that fails
// validation is returned in skipped with the reason instead of failing the
// whole set, matching pretool.Resolve.
func Resolve(layer []Rule) (rules []Rule, skipped []string) {
	for _, r := range layer {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		if err := ValidateLayer([]Rule{r}); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		rules = append(rules, r)
	}
	return rules, skipped
}

// Hit is a rule that matched the final text.
type Hit struct {
	Rule *Rule
	// Match is the text the pattern matched.
	Match string
}

// Match returns the first rule whose text patterns match text, or nil. A
// pattern that fails to compile never matches (Resolve already rejected it;
// this guards a hand-edited command line).
func Match(rules []Rule, text string) *Hit {
	for i := range rules {
		r := &rules[i]
		for _, pat := range r.Text {
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			if loc := re.FindStringIndex(text); loc != nil {
				return &Hit{Rule: r, Match: text[loc[0]:loc[1]]}
			}
		}
	}
	return nil
}

// ReasonFor renders the block reason for a hit.
func (h *Hit) ReasonFor() string {
	reason := h.Rule.Reason
	if reason == "" {
		reason = DefaultReason
	}
	return strings.ReplaceAll(reason, MatchPlaceholder, Excerpt(h.Match, maxMatchInReason))
}

// maxMatchInReason caps the matched text quoted back in a reason.
const maxMatchInReason = 120

// Excerpt collapses whitespace in s and caps it at about max bytes (never
// splitting a UTF-8 sequence), marking a cut.
func Excerpt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}

// Encode serialises rules for the hook command line (base64url JSON, free of
// shell metacharacters).
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
