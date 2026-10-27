package opencode

import "foci/internal/delegator"

// PermList is opencode's permission category for its directory-listing tool.
const PermList = "list"

// readOnlyBatchActions is a read-only batch session's ruleset, as
// mapPermissionMode-style category actions: everything denied, then the
// reading and searching tools allowed, anywhere on disk (external_directory)
// and on the web. bash is denied with the rest, so foci's exec-bridge shell
// functions are out of reach.
var readOnlyBatchActions = map[string]any{
	"*":                   "deny",
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
// rules (answerUnattended), never a prompt.
func batchAccessRules(batch bool, access delegator.BatchAccess) []permissionRule {
	if batch && access == delegator.BatchAccessReadOnly {
		return permissionRuleset(readOnlyBatchActions)
	}
	return nil
}
