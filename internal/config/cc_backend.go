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

// DefaultCCBatchModel is the factory default for CCBackendConfig.BatchModel:
// foci's own small background batches (foci_summary, the /prompts diff
// summary) on Claude Code agents run on haiku unless [cc_backend] batch_model
// says otherwise ("" = the agent's own model).
const DefaultCCBatchModel = "haiku"

// fociConfigApprovalSubcommands are the `foci config` subcommands that change
// foci.toml only after the user approves the change in chat (#3232).
var fociConfigApprovalSubcommands = []string{"set", "unset", "add"}

// FociConfigAllowedTools returns the Claude Code permission rules that
// pre-approve the approval-gated `foci config` subcommands, for the bare
// command name `foci` and for each extra spelling of the CLI's path in
// cliPaths (empty entries are skipped).
//
// The in-chat approval that `foci config set` asks for is the gate on the
// change. Without these rules, Claude Code's auto-mode classifier judges the
// call first and can refuse it as self-modification, so the user never sees
// the approval (#3232). A permission rule is checked before the classifier.
// Probed on claude 2.1.325: a matching rule decided in ~10 ms against the
// classifier's 1.6–2.5 s. The rule does not cover a compound command
// (`… && other`), a command substitution, or a `VAR=value` prefix: those
// still go to the classifier (auto mode) or to foci (default mode).
func FociConfigAllowedTools(cliPaths ...string) []string {
	names := append([]string{"foci"}, cliPaths...)
	var rules []string
	for _, n := range names {
		if n == "" {
			continue
		}
		for _, sub := range fociConfigApprovalSubcommands {
			rules = append(rules, "Bash("+n+" config "+sub+":*)")
		}
	}
	return rules
}

// MergedAllowedTools combines the global DefaultAllowedTools with the
// per-agent backend_config.allowed_tools value, then appends the builtin
// rules (foci's own, always on: FociConfigAllowedTools). Duplicates are
// removed (first occurrence wins, preserving default ordering). The result is
// a comma-separated string suitable for Claude Code's --allowedTools argv.
// An empty string is returned when all inputs are empty.
//
// Accepts per-agent input as either a comma-separated string or a slice of
// strings (TOML []any), since backend_config is an untyped map and either
// form is a natural way to write permission rules in TOML.
func (c CCBackendConfig) MergedAllowedTools(perAgent any, builtin ...string) string {
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

	for _, r := range builtin {
		add(r)
	}

	return strings.Join(rules, ",")
}
