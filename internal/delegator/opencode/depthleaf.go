package opencode

import (
	"foci/internal/delegator"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// Depth leaves (#2625): no subagent path but foci_spawn
//
// opencode starts subagent work through one tool, task (PermTask). In a depth
// leaf (StartOptions.DepthLeaf) every session ruleset foci applies ends with
// depthLeafTaskRule, so opencode asks before every task call whatever the
// agent's permissions allow (rules resolve with findLast). foci answers that
// ask itself, before anything else sees it: a reject carrying
// DepthLeafRefusal, never a prompt or an auto-approve. Verified live on
// opencode 1.17.15 (2026-11-01): with {*,*,allow} then {task,*,ask} a task
// call raised permission.asked {permission:"task", patterns:["general"]}, and
// POST /permission/{id}/reply {reply:"reject", message} failed the tool call
// with "The user rejected permission to use this specific tool call with the
// following feedback: <message>", which the model read as the tool result.
// ---------------------------------------------------------------------------

// depthLeafTaskRule makes opencode ask before every task call in a leaf.
var depthLeafTaskRule = permissionRule{Permission: PermTask, Pattern: "*", Action: "ask"}

// depthLeafTaskRefusal is the tool result of a refused task call.
var depthLeafTaskRefusal = delegator.DepthLeafRefusal("The task tool (an opencode subagent)")

// withDepthLeafRule returns rules with depthLeafTaskRule last when the session
// is a depth leaf, else rules unchanged.
func (b *Backend) withDepthLeafRule(rules []permissionRule) []permissionRule {
	if !b.depthLeaf {
		return rules
	}
	return append(append([]permissionRule{}, rules...), depthLeafTaskRule)
}

// refuseDepthLeafTask answers a task ask in a depth leaf with a reject, and
// reports whether it did. Nothing is registered or prompted for it.
func (b *Backend) refuseDepthLeafTask(pp pendingPermission) bool {
	if !b.depthLeaf || pp.permType != PermTask {
		return false
	}
	lg := log.NewComponentLogger(b.logComponent())
	lg.Infof("depth leaf: refused task call (permission %s, patterns %v)", pp.id, pp.patterns)
	var err error
	if pp.replyNext {
		err = b.replyPermissionNextMessage(pp.id, "reject", depthLeafTaskRefusal)
	} else {
		// The legacy reply carries no message: the call is still refused.
		err = b.postPermissionResponse(pp.id, "deny", false)
	}
	if err != nil {
		lg.Warnf("depth leaf: refusing task call %s failed: %v", pp.id, err)
	}
	return true
}
