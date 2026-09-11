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
//
// WHEN THIS RUNS MATTERS AS MUCH AS WHAT IT CHECKS (#1900). It reads the
// process PATH, and foci-gw's PATH is written twice: once by the unit, and
// again by shellenv.Apply() partway through main(), which is the one tool
// shells inherit. So this pass is deliberately NOT called from config.Load —
// Load necessarily runs before shellenv can know which file to source — but
// from cmd/foci-gw/main.go immediately after shellenv.Apply(). Moving it back
// into Load would silently restore the defect: every verdict would describe a
// PATH nothing executes under, and the unresolved-command warning below would
// name commands that exist.

// DroppedAutoApproveRule records one entry removed by the startup check.
type DroppedAutoApproveRule struct {
	Rule   string // the config entry, verbatim
	Path   string // the executable that failed the check
	Reason string // human-readable cause
}

// UnlocatableAutoApproveCommand records an entry naming a bare command that no
// directory on PATH provides. The entry is KEPT — being unable to find a file
// is not evidence against it — but it is reported, because the guard's silence
// about such an entry means "I could not look", not "I looked and it was safe",
// and those read identically in a log that says nothing. #1900 is exactly that
// failure: `mds` was missing from the PATH the guard searched, yielded no
// verdict, and was auto-approved from an agent-writable directory for as long
// as the divergence lasted. Either the entry is dead or the guard is searching
// the wrong PATH; both are worth a line at boot.
type UnlocatableAutoApproveCommand struct {
	Rule    string // the config entry, verbatim
	Command string // the bare command name that could not be located
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

// filterWritableAutoApproveRules returns the entries that survive, those
// dropped, and those kept but whose command could not be located on PATH.
// Pure with respect to env: no direct filesystem access.
func filterWritableAutoApproveRules(rules []string, env execguard.Env) ([]string, []DroppedAutoApproveRule, []UnlocatableAutoApproveCommand) {
	if len(rules) == 0 {
		return rules, nil, nil
	}
	kept := make([]string, 0, len(rules))
	var dropped []DroppedAutoApproveRule
	var unlocatable []UnlocatableAutoApproveCommand
	for _, rule := range rules {
		var bad *DroppedAutoApproveRule
		var missing []UnlocatableAutoApproveCommand
		for _, token := range commandTokens(rule) {
			if substitutable, path, reason := execguard.Substitutable(token, env); substitutable {
				bad = &DroppedAutoApproveRule{Rule: rule, Path: path, Reason: reason}
				break
			}
			if execguard.UnresolvedBareName(token, env) {
				missing = append(missing, UnlocatableAutoApproveCommand{Rule: rule, Command: token})
			}
		}
		if bad != nil {
			dropped = append(dropped, *bad)
			continue
		}
		unlocatable = append(unlocatable, missing...)
		kept = append(kept, rule)
	}
	return kept, dropped, unlocatable
}

// DropSubstitutableAutoApproveRules applies the check to the global block and
// to every agent block, warning once per dropped entry and once per entry whose
// command could not be located.
//
// Call this from startup AFTER the process PATH is final — see the package-level
// note above. It is safe to call more than once: it only ever removes entries.
func (cfg *Config) DropSubstitutableAutoApproveRules(env execguard.Env) {
	warn := func(scope string, dropped []DroppedAutoApproveRule, unlocatable []UnlocatableAutoApproveCommand) {
		for _, d := range dropped {
			configLog.Warnf("[permissions] %s: ignoring auto_approve entry %q — %s (%s). Move the executable somewhere the foci process cannot write, or remove the entry.",
				scope, d.Rule, d.Reason, d.Path)
		}
		for _, u := range unlocatable {
			configLog.Warnf("[permissions] %s: auto_approve entry %q names %q, which is on no directory of this process's PATH. The substitutability check cannot judge a file it cannot find, so the entry is approved UNCHECKED. Either the entry is dead, or foci-gw's PATH differs from the shell's — compare os.Getenv(\"PATH\") here against a tool shell's (NOT /proc/<pid>/environ, which predates shellenv).",
				scope, u.Rule, u.Command)
		}
	}
	kept, dropped, unlocatable := filterWritableAutoApproveRules(cfg.Permissions.AutoApprove, env)
	cfg.Permissions.AutoApprove = kept
	warn("global", dropped, unlocatable)

	for i := range cfg.Agents {
		agentKept, agentDropped, agentUnlocatable := filterWritableAutoApproveRules(cfg.Agents[i].Permissions.AutoApprove, env)
		cfg.Agents[i].Permissions.AutoApprove = agentKept
		warn(fmt.Sprintf("agent %q", cfg.Agents[i].ID), agentDropped, agentUnlocatable)
	}
}
