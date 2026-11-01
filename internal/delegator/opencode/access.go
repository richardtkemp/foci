package opencode

import "foci/internal/delegator"

// PermList is opencode's permission category for its directory-listing tool.
const PermList = "list"

// readOnlyBatchActions is a read-only batch session's ruleset, as
// mapPermissionMode-style category actions: everything denied, then the
// reading and searching tools allowed, anywhere on disk (external_directory)
// and on the web. bash asks (#2702): every command reaches foci, which
// decides it from the read-only rules alone (answerUnattended; the session
// is unattended), so the shell runs only foci's read-only set, foci's
// read-only exec-bridge functions included.
var readOnlyBatchActions = map[string]any{
	"*":                   "deny",
	PermBash:              "ask",
	PermRead:              "allow",
	PermGlob:              "allow",
	PermGrep:              "allow",
	PermList:              "allow",
	PermWebfetch:          "allow",
	PermWebsearch:         "allow",
	PermExternalDirectory: "allow",
}

// batchAccessRules returns the session ruleset a restricted batch session
// runs under, or nil for a session that keeps the agent's own
// (default_permission) rules. opencode resolves rules with findLast, and
// permissionRuleset emits the "*" catch-all first, so every category not
// allowed here stays denied — opencode refuses it without an ask. Its .env
// read guards ask, which the session's unattended handling decides from the
// rules (answerUnattended), never a prompt, or, for a spawn whose asks go up
// to its spawner (#2684), a prompt in the spawner's chat. Keyed on access
// alone: only a batch carries a non-default access, and a spawn that prompts
// is still restricted.
func batchAccessRules(access delegator.BatchAccess) []permissionRule {
	if access == delegator.BatchAccessReadOnly {
		return permissionRuleset(readOnlyBatchActions)
	}
	return nil
}
