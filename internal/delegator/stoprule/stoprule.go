// Package stoprule is the rule engine behind foci's Claude Code Stop hook
// (#2089). It catches a turn that ends by announcing work ("starting the
// build now", "I'm sending it") when nothing was started: the hook reads the
// turn's final assistant text and the turn's transcript, and when the text
// matches a rule and the turn launched no background job, it blocks the stop
// and hands the rule's reason to the agent, which then continues the turn.
//
// A rule is data: a name, one or more regexes matched against the final
// assistant text, and a reason. There are no preinstalled rules. Rules come
// from two config layers, merged by name like pretool rules (#3124): the
// global [[cc_backend.stop_rules]] for every CC-backed agent, then the
// agent's [[agents.backend_config.stop_rules]], whose entry for a name gives
// only the fields it changes (e.g. enabled = false to switch a global rule
// off for one agent).
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
// ([[cc_backend.stop_rules]] and [[agents.backend_config.stop_rules]]) and
// the JSON wire shape handed to the hook binary. A new field needs a line in
// overlay, or a later layer cannot change it.
//
// New keys here follow the hot-reload rule at the top of
// internal/config/types.go (#2603): hot:"<when>" tag + live read + applier.
type Rule struct {
	Name string `toml:"name" json:"name"`
	// Text holds regexes matched against the turn's final assistant text;
	// any one may match. Write (?i) for a case-insensitive pattern.
	Text    pretool.Patterns `toml:"text"    json:"text"`
	Reason  string           `toml:"reason"  json:"reason,omitempty"`
	Enabled *bool            `toml:"enabled" json:"-"`
}

// ValidateLayer checks one config layer's rules: every rule needs a unique
// name, and every pattern it gives must be non-empty and compile. A layer may
// leave text out (an override of a rule an earlier layer defines); whether
// the merged rule has text is checked by Resolve.
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

// Resolve merges layers by name (a later layer overrides an earlier one
// field by field: text, reason and enabled each only when set), drops
// disabled rules, and returns the enabled rules in first-seen order. A rule
// the merge leaves unusable (no text, or a pattern that fails validation) is
// returned in skipped with the reason instead of failing the whole set,
// matching pretool.Resolve.
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
		if err := ValidateLayer([]Rule{r}); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if len(r.Text) == 0 {
			skipped = append(skipped, fmt.Sprintf("stop_rules %q: text is required", name))
			continue
		}
		rules = append(rules, r)
	}
	return rules, skipped
}

// overlay applies the fields over sets onto base.
func overlay(base, over Rule) Rule {
	if over.Text != nil {
		base.Text = over.Text
	}
	if over.Reason != "" {
		base.Reason = over.Reason
	}
	if over.Enabled != nil {
		base.Enabled = over.Enabled
	}
	return base
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
