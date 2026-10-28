package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// SendPermissionResponse sends the user's permission decision back to the
// delegated backend over its protocol, keyed by requestID. A backend that can
// answer none of the prompt kinds (or an empty requestID) is an error: the
// decision would otherwise be dropped silently.
func (a *Agent) SendPermissionResponse(ctx context.Context, sessionKey string, requestID string, choice string) error {
	if a.DelegatedManager == nil {
		return nil
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}

	// AskUserQuestion routing — button clicks ("qa:*") and cancellation.
	if qr, ok := delegator.As[delegator.QuestionResponder](be); ok && requestID != "" {
		if choice == "qa:cancel" {
			a.taggedLog("agent/perm").Debugf("cancelling question: reqID=%s", requestID)
			return qr.CancelQuestion(requestID)
		}
		if strings.HasPrefix(choice, "qa:") {
			a.taggedLog("agent/perm").Debugf("answering question: reqID=%s choice=%q", requestID, choice)
			return qr.RespondToQuestion(requestID, choice)
		}
	}

	// Elicitation routing — button clicks ("elic:*"). Free-text answers
	// arrive via the turn_delegated.go intercept below, not this path.
	if er, ok := delegator.As[delegator.ElicitationResponder](be); ok && requestID != "" {
		if strings.HasPrefix(choice, "elic:") {
			a.taggedLog("agent/perm").Debugf("answering elicitation: reqID=%s choice=%q", requestID, choice)
			return er.RespondToElicitation(requestID, choice)
		}
	}

	pr, ok := delegator.As[delegator.PermissionResponder](be)
	if ok && requestID != "" {
		d := permissionDecision(choice)
		a.taggedLog("agent/perm").Debugf("responding: reqID=%s choice=%q allow=%v remember=%v rule=%q", requestID, choice, d.Allow, d.Remember, d.RulePrefix)
		err := pr.RespondToPermission(requestID, d)
		if err != nil {
			a.taggedLog("agent/perm").Errorf("RespondToPermission failed: reqID=%s sk=%s err=%v", requestID, sessionKey, err)
		}
		return err
	}

	return fmt.Errorf("agent: backend cannot answer permission prompt (reqID=%q choice=%q)", requestID, choice)
}

// FailUndeliverablePrompt answers a backend prompt that could not be shown to
// anyone (#2608, Dick 2026-10-28: "undeliverable must fail"). Without it the
// backend waits forever: a failed send leaves no prompt entry, so nothing ever
// expires or is clicked. The prompt is refused with reason as the message the
// backend sees; the kinds are tried in turn, permission (deny) then question
// (cancel) then elicitation (cancel), because the gateway does not know which
// kind requestID is. A backend that refuses all of them is an error.
func (a *Agent) FailUndeliverablePrompt(ctx context.Context, sessionKey, requestID, reason string) error {
	if a.DelegatedManager == nil {
		return nil
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}
	var errs []error
	if pr, ok := delegator.As[delegator.PermissionResponder](be); ok {
		err := pr.RespondToPermission(requestID, delegator.PermissionDecision{Message: reason})
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if qr, ok := delegator.As[delegator.QuestionResponder](be); ok {
		err := qr.CancelQuestion(requestID)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if er, ok := delegator.As[delegator.ElicitationResponder](be); ok {
		err := er.RespondToElicitation(requestID, "elic:cancel")
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return fmt.Errorf("agent: backend cannot answer prompt %q", requestID)
	}
	return errors.Join(errs...)
}

// permissionDecision turns a prompt button's data into the unified decision
// every backend answers (#2154 Phase 3). The buttons differ per backend:
//   - "allow" / "deny"                — every backend
//   - "allow_always:<prefix>"         — ccstream (a persistent rule for prefix)
//   - "always"                        — opencode (remember this decision)
//
// Anything starting "allow" allows; "always" implies allow; a RulePrefix is
// carried only by "allow_always:"; a deny gets the standard message.
func permissionDecision(choice string) delegator.PermissionDecision {
	var d delegator.PermissionDecision
	if prefix, ok := strings.CutPrefix(choice, "allow_always:"); ok {
		d.RulePrefix = prefix
	}
	d.Remember = choice == "always" || strings.HasPrefix(choice, "allow_always")
	d.Allow = d.Remember || strings.HasPrefix(choice, "allow")
	if !d.Allow {
		d.Message = "User denied permission"
	}
	return d
}

// CancelPendingQuestion cancels an outstanding AskUserQuestion if one exists
// for the given session. Returns true if a question was cancelled.
// Used by /stop to cancel a question without stopping the CC session.
func (a *Agent) CancelPendingQuestion(ctx context.Context, sessionKey string) bool {
	if a.DelegatedManager == nil {
		return false
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return false
	}
	qr, ok := delegator.As[delegator.QuestionResponder](be)
	if !ok {
		return false
	}
	reqID := qr.HasPendingQuestion()
	if reqID == "" {
		return false
	}
	a.taggedLog("agent/perm").Debugf("cancelling pending question via /stop: reqID=%s session=%s", reqID, sessionKey)
	_ = qr.CancelQuestion(reqID)
	return true
}
