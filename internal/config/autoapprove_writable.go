package config

import (
	"fmt"
	"strings"

	"foci/internal/execguard"
)

// Startup drops auto_approve entries whose command the foci process could
// substitute — see package execguard for what "substitute" means and why it
// matters. The entry stays in the config file; it is ignored, not rewritten,
// so the operator sees a warning rather than silently losing a rule.
//
// This startup pass is a config-hygiene check, not the security control: a
// verdict taken at startup goes stale the moment a file changes. The control
// is the match-time check in internal/delegator/autoapprove, which re-runs the
// same test against the live filesystem every time a command is approved. This
// pass exists so a permanently-untrustworthy entry is reported once, loudly, at
// boot rather than silently failing to auto-approve forever.
//
// Scope: Bash entries only. Read/Edit/Write entries name DATA, not code, and a
// writable data path is the normal case.

// DroppedAutoApproveRule records one entry removed by the startup check.
type DroppedAutoApproveRule struct {
	Rule   string // the config entry, verbatim
	Path   string // the executable that failed the check
	Reason string // human-readable cause
}

// commandTokens extracts the command token from every segment of an entry.
// Entries are composable with && || ; | so each segment carries its own command.
func commandTokens(rule string) []string {
	toolName, pattern, found := strings.Cut(rule, ":")
	if !found || toolName != "Bash" {
		return nil
	}
	var tokens []string
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	for _, segment := range strings.Split(replacer.Replace(pattern), "\x00") {
		token := strings.TrimSpace(segment)
		if idx := strings.IndexAny(token, " \t"); idx >= 0 {
			token = token[:idx]
		}
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// filterWritableAutoApproveRules returns the entries that survive plus those
// dropped. Pure with respect to env: no direct filesystem access.
func filterWritableAutoApproveRules(rules []string, env execguard.Env) ([]string, []DroppedAutoApproveRule) {
	if len(rules) == 0 {
		return rules, nil
	}
	kept := make([]string, 0, len(rules))
	var dropped []DroppedAutoApproveRule
	for _, rule := range rules {
		var bad *DroppedAutoApproveRule
		for _, token := range commandTokens(rule) {
			if substitutable, path, reason := execguard.Substitutable(token, env); substitutable {
				bad = &DroppedAutoApproveRule{Rule: rule, Path: path, Reason: reason}
				break
			}
		}
		if bad != nil {
			dropped = append(dropped, *bad)
			continue
		}
		kept = append(kept, rule)
	}
	return kept, dropped
}

// dropWritableAutoApproveRules applies the check to the global block and to
// every agent block, warning once per dropped entry.
func (cfg *Config) dropWritableAutoApproveRules(env execguard.Env) {
	warn := func(scope string, dropped []DroppedAutoApproveRule) {
		for _, d := range dropped {
			configLog.Warnf("[permissions] %s: ignoring auto_approve entry %q — %s (%s). Move the executable somewhere the foci process cannot write, or remove the entry.",
				scope, d.Rule, d.Reason, d.Path)
		}
	}
	kept, dropped := filterWritableAutoApproveRules(cfg.Permissions.AutoApprove, env)
	cfg.Permissions.AutoApprove = kept
	warn("global", dropped)

	for i := range cfg.Agents {
		agentKept, agentDropped := filterWritableAutoApproveRules(cfg.Agents[i].Permissions.AutoApprove, env)
		cfg.Agents[i].Permissions.AutoApprove = agentKept
		warn(fmt.Sprintf("agent %q", cfg.Agents[i].ID), agentDropped)
	}
}
