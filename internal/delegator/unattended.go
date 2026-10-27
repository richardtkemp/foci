package delegator

import "fmt"

// Unattended asks (Dick 2026-10-27): a permission ask raised where nobody can
// answer it is decided by foci's own auto-approve rules, the same on every
// backend. A rule permits it → allow. No rule matches, or the rule engine
// vetoes the match → deny, with a WARN naming what was refused. A question
// (CC AskUserQuestion, an MCP elicitation, opencode's question tool) has no one
// to answer it → decline. Nothing is ever prompted or left outstanding: a
// surfaced prompt would block the turn forever (incident 2026-10-27: an
// external_directory ask blocked a reflection turn 89 minutes).

// UnattendedSkipLabel is the label an unattended ask carries in logs when the
// whole session skips prompts (SkipPermissions) rather than one turn.
const UnattendedSkipLabel = "skip_permissions"

// UnattendedDenyMessage is the tool result the model sees for a denied
// unattended ask, so it says how to proceed.
const UnattendedDenyMessage = "Unattended: nobody is here to answer a permission prompt, and foci's auto-approve rules do not permit this call, so it was denied. Do it another way the rules allow (for rm, name the target path literally), or leave it for an attended turn."

// UnattendedQuestionMessage is the tool result the model sees for a declined
// unattended question.
const UnattendedQuestionMessage = "Unattended: nobody is here to answer a question. Decide for yourself, or leave it for an attended turn."

// Unattended reports whether an ask raised now has nobody to answer it, and
// the label its log lines carry. It does when the whole session skips prompts
// (skipPermissions: a batch session, or backend_config.skip_permissions) or
// when the in-flight turn is unattended (TurnEvents.Unattended). turn is the
// backend's current TurnEvents, read under the backend's own turn lock; nil
// between turns.
func Unattended(skipPermissions bool, turn *TurnEvents) (label string, ok bool) {
	if skipPermissions {
		return UnattendedSkipLabel, true
	}
	if turn != nil && turn.Unattended {
		return turn.Trigger, true
	}
	return "", false
}

// UnattendedDeniedLog is the WARN line for an unattended ask that the rules did
// not permit. action says what the session tried ("run Bash: rm -rf x", "use
// external_directory: /etc/*"); vetoReason is the rule engine's veto, "" when
// no rule matched at all.
func UnattendedDeniedLog(sessionKey, label, action, vetoReason string) string {
	if vetoReason != "" {
		return fmt.Sprintf("unattended: session %s (trigger=%s) tried to %s; a rule matched but was vetoed (%s), so it was denied", sessionKey, label, action, vetoReason)
	}
	return fmt.Sprintf("unattended: session %s (trigger=%s) tried to %s; no rule permits it, so it was denied", sessionKey, label, action)
}

// UnattendedAllowedLog is the INFO line for an unattended ask a rule permitted.
func UnattendedAllowedLog(sessionKey, label, action string) string {
	return fmt.Sprintf("unattended: session %s (trigger=%s) allowed by rule: %s", sessionKey, label, action)
}

// UnattendedQuestionLog is the WARN line for a declined unattended question.
func UnattendedQuestionLog(sessionKey, label, what string) string {
	return fmt.Sprintf("unattended: session %s (trigger=%s) asked a question nobody can answer, so it was declined: %s", sessionKey, label, what)
}
