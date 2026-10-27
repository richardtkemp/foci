package ccstream

import (
	"encoding/json"
	"fmt"
)

// PermissionDeniedMessage is CC's system/permission_denied event: CC refused a
// tool call itself, without asking foci (auto mode's classifier, or a built-in
// safety check). Probed on 2.1.311.
type PermissionDeniedMessage struct {
	ToolName           string `json:"tool_name"`
	ToolUseID          string `json:"tool_use_id"`
	DecisionReasonType string `json:"decision_reason_type"` // "classifier", "safetyCheck", ...
	DecisionReason     string `json:"decision_reason"`
}

// ccDeniedLog is the WARN line for a tool call CC itself denied in an
// unattended session. Same shape as delegator.UnattendedDeniedLog, so one
// grep for "unattended: session" finds every unattended denial. It names the
// part of CC that refused and says outright that neither foci's rules nor a
// user did, so a reader never mistakes it for a foci rule denial.
func ccDeniedLog(sessionKey, label string, d PermissionDeniedMessage) string {
	return fmt.Sprintf("unattended: session %s (trigger=%s) tried to use %s; blocked by %s, not by foci's rules or a user (%s: %s)",
		sessionKey, label, d.ToolName, ccDenier(d.DecisionReasonType), d.DecisionReasonType, d.DecisionReason)
}

// ccDenier names the part of Claude Code that refused, from the event's
// decision_reason_type (values seen on 2.1.311: "classifier", "safetyCheck").
func ccDenier(reasonType string) string {
	switch reasonType {
	case "classifier":
		return "Claude Code's auto mode classifier"
	case "safetyCheck":
		return "Claude Code's built-in safety check"
	default:
		return "Claude Code itself"
	}
}

// onPermissionDenied logs a WARN for a tool call CC denied on its own, when
// nobody is watching this session or turn. In auto mode (a batch session)
// these denials never reach foci as an ask, so this is the only trace foci
// keeps of them. An attended session sees the denial in its own chat, so it
// gets no WARN.
func (b *Backend) onPermissionDenied(raw json.RawMessage) {
	var d PermissionDeniedMessage
	if err := json.Unmarshal(raw, &d); err != nil {
		b.logger().Warnf("drop permission_denied message (unmarshal failed): %v", err)
		return
	}
	label, ok := b.unattended()
	if !ok {
		return
	}
	b.logger().Warnf("%s", ccDeniedLog(b.startOpts.SessionKey, label, d))
}

// checkPermissionMode logs a WARN when CC started in a permission mode other
// than the one foci asked for. CC falls back silently: a batch session asks for
// auto mode, and a model without it (claude-haiku-4-5 on 2.1.311) starts in
// "default" with no error. Behaviour stays safe — its asks then reach foci
// and answerUnattended decides them — so this is visibility only.
func (b *Backend) checkPermissionMode(got, model string) {
	want := b.requestedPermMode
	if want == "" || got == want {
		return
	}
	b.logger().Warnf("session %s: asked Claude Code for permission mode %q, but it started in %q (model %s); asks it does not decide itself will reach foci's rules",
		b.startOpts.SessionKey, want, got, model)
}
