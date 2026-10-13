package autoapprove

import (
	"foci/internal/execguard"
)

// Names of the built-in rule groups, as both reporters (the startup WARN and
// the -check-config preview) print them.
const (
	GroupReadonly  = "read-only"
	GroupSafeWrite = "safe-write"
)

// SubstitutableBuiltin records one command of a built-in group whose
// executable the match-time veto will refuse.
type SubstitutableBuiltin struct {
	Group   string // GroupReadonly or GroupSafeWrite
	Command string // the command token the veto will refuse
	Path    string // the executable that failed the check
	Reason  string // human-readable cause
}

// SubstitutableBuiltins scans the built-in rule groups (CommonReadonlyRules,
// CommonSafeWriteRules) for commands the match-time veto will refuse, so the
// reporters can name them (#1897). The veto is deliberately independent of
// which rule matched — including these groups — which is why a vetoed
// built-in could otherwise stop auto-approving with no line anywhere saying
// so.
//
// One judgement, no second copy of the rules: each command token goes through
// the same tokenizeCommand the veto uses and the same execguard.Substitutable
// call, so the report cannot drift from the enforcer.
//
// Only substitutable commands are reported. A built-in naming a tool that is
// not installed on this host (`ack`, `yq`) is normal, not a config error, and
// Substitutable already answers false for a name with no PATH winner.
// FociShellRulesFor output is never scanned: those name bash functions, not
// files on PATH, and there is nothing to substitute.
//
// The findings are per COMMAND, not per rule: several rules share one command
// token (`go env`, `go list`, … are all `go`), the veto judges the token, and
// a report that prints the same command six times buries the real count.
func SubstitutableBuiltins(readonly, safeWrite bool, env execguard.Env) []SubstitutableBuiltin {
	var found []SubstitutableBuiltin
	seen := make(map[string]bool)
	scan := func(group string, rules []string) {
		for _, rule := range rules {
			r := parseAutoApproveRule(rule)
			if r.ToolName != "Bash" {
				continue // tool rules (Search, Read, …) name no executable
			}
			tokens := tokenizeCommand(r.Pattern)
			if len(tokens) == 0 {
				continue
			}
			if substitutable, path, reason := execguard.Substitutable(tokens[0], env); substitutable {
				key := group + "\x00" + tokens[0]
				if seen[key] {
					continue // same group, same command: already reported
				}
				seen[key] = true
				found = append(found, SubstitutableBuiltin{Group: group, Command: tokens[0], Path: path, Reason: reason})
			}
		}
	}
	if readonly {
		scan(GroupReadonly, CommonReadonlyRules)
	}
	if safeWrite {
		scan(GroupSafeWrite, CommonSafeWriteRules)
	}
	return found
}
