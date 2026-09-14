package ccstream

import (
	"encoding/json"

	"foci/internal/delegator/autoapprove"
)

// autoApproveRule is a type alias preserving backward compatibility with the
// ccstream Backend's existing field type. The engine itself lives in the
// shared autoapprove package.
type autoApproveRule = autoapprove.Rule

// CommonReadonlyRules re-exports the shared built-in readonly rules for
// backward compatibility with agents_delegated.go.
var CommonReadonlyRules = autoapprove.CommonReadonlyRules

// CommonSafeWriteRules re-exports the shared built-in safe-write rules.
var CommonSafeWriteRules = autoapprove.CommonSafeWriteRules

// FociShellRulesFor re-exports the shared shell-rules generator.
func FociShellRulesFor(execNames []string) []string {
	return autoapprove.FociShellRulesFor(execNames)
}

// parseAutoApproveRules wraps the shared Compile for ccstream-internal use.
func parseAutoApproveRules(rules []string) []autoApproveRule {
	return autoapprove.Compile(rules)
}

// autoApprovePermission checks the request against compiled rules and, if
// matched, sends an allow response directly. Returns true if auto-approved.
// The second return is the substitutability veto's reason, set only when a rule
// DID match but the guard refused it (#1906). Before this, that refusal was
// entirely silent: the user got an ordinary prompt for a command sitting on
// their own allowlist, with nothing anywhere saying why.
func (b *Backend) autoApprovePermission(msg *PermissionRequest) (bool, string) {
	if len(b.autoApproveRules) == 0 {
		return false, ""
	}

	matched, vetoReason := autoapprove.MatchWithEnv(b.autoApproveRules, msg.Request.ToolName, msg.Request.Input, b.autoApproveEnv)
	if !matched {
		if vetoReason != "" {
			b.logger().Warnf("auto-approve VETOED: tool=%s summary=%q req_id=%s — %s", msg.Request.ToolName, msg.Request.Summary(), msg.RequestID, vetoReason)
		}
		return false, vetoReason
	}

	summary := msg.Request.Summary()
	b.logger().Infof("auto-approved: tool=%s summary=%q req_id=%s", msg.Request.ToolName, summary, msg.RequestID)

	resp := &PermissionAllow{
		Behavior:               "allow",
		UpdatedInput:           json.RawMessage(`{}`),
		ToolUseID:              msg.Request.ToolUseID,
		DecisionClassification: "user_temporary",
	}
	if err := b.writer.SendControlResponse(msg.RequestID, resp); err != nil {
		b.logger().Warnf("auto-approve send failed: %v", err)
		return false, ""
	}

	return true, ""
}
