package config

import "strings"

// DefaultCCAllowedTools is the factory default for
// CCBackendConfig.DefaultAllowedTools. It pre-approves file operations under
// /tmp so Claude Code agents can freely use the system scratch directory
// without a permission round-trip. Users can override or extend via
// [cc_backend] default_allowed_tools in foci.toml.
var DefaultCCAllowedTools = []string{
	"Read(/tmp/**)",
	"Edit(/tmp/**)",
	"MultiEdit(/tmp/**)",
}

// MergedAllowedTools combines the global DefaultAllowedTools with the
// per-agent backend_config.allowed_tools value. Duplicates are removed
// (first occurrence wins, preserving default ordering). The result is
// a comma-separated string suitable for Claude Code's --allowedTools argv.
// An empty string is returned when both inputs are empty.
//
// Accepts per-agent input as either a comma-separated string or a slice of
// strings (TOML []any), since backend_config is an untyped map and either
// form is a natural way to write permission rules in TOML.
func (c CCBackendConfig) MergedAllowedTools(perAgent any) string {
	var rules []string
	seen := make(map[string]bool)
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		rules = append(rules, r)
	}

	for _, r := range c.DefaultAllowedTools {
		add(r)
	}

	switch v := perAgent.(type) {
	case string:
		for _, r := range strings.Split(v, ",") {
			add(r)
		}
	case []string:
		for _, r := range v {
			add(r)
		}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				add(s)
			}
		}
	}

	return strings.Join(rules, ",")
}
