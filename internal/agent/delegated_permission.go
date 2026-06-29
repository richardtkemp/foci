package agent

import (
	"context"
	"strings"

	"foci/internal/delegator"
	"foci/internal/log"
)

// SendPermissionResponse sends the user's permission decision back to the
// delegated backend. For ccstream (protocol-based), it sends a JSON
// control_response via stdin using the requestID. For tmux backends, it
// falls back to sending a keystroke.
func (a *Agent) SendPermissionResponse(ctx context.Context, sessionKey string, requestID string, choice string) error {
	if a.DelegatedManager == nil {
		return nil
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}

	// AskUserQuestion routing — button clicks ("qa:*") and cancellation.
	if qr, ok := be.(delegator.QuestionResponder); ok && requestID != "" {
		if choice == "qa:cancel" {
			log.Debugf("agent/perm", "cancelling question: reqID=%s", requestID)
			return qr.CancelQuestion(requestID)
		}
		if strings.HasPrefix(choice, "qa:") {
			log.Debugf("agent/perm", "answering question: reqID=%s choice=%q", requestID, choice)
			return qr.RespondToQuestion(requestID, choice)
		}
	}

	// Elicitation routing — button clicks ("elic:*"). Free-text answers
	// arrive via the turn_delegated.go intercept below, not this path.
	if er, ok := be.(delegator.ElicitationResponder); ok && requestID != "" {
		if strings.HasPrefix(choice, "elic:") {
			log.Debugf("agent/perm", "answering elicitation: reqID=%s choice=%q", requestID, choice)
			return er.RespondToElicitation(requestID, choice)
		}
	}

	// Protocol-based response (ccstream) — use RespondToPermission if available.
	type permResponder interface {
		RespondToPermission(requestID string, allow bool, message string) error
	}
	type ruleResponder interface {
		RespondToPermissionWithRule(requestID string, prefix string) error
	}

	if pr, ok := be.(permResponder); ok && requestID != "" {
		// "allow_always:prefix" → permanent rule for this prefix.
		if strings.HasPrefix(choice, "allow_always:") {
			prefix := strings.TrimPrefix(choice, "allow_always:")
			if rr, ok := be.(ruleResponder); ok {
				log.Debugf("agent/perm", "responding with rule: reqID=%s prefix=%s", requestID, prefix)
				err := rr.RespondToPermissionWithRule(requestID, prefix)
				if err != nil {
					log.Errorf("agent/perm", "RespondToPermissionWithRule failed: reqID=%s sk=%s err=%v", requestID, sessionKey, err)
				}
				return err
			}
			// Fall through to simple allow if rule not supported.
		}

		allow := choice == "allow" || strings.HasPrefix(choice, "allow")
		msg := ""
		if !allow {
			msg = "User denied permission"
		}
		log.Debugf("agent/perm", "responding via protocol: reqID=%s choice=%q allow=%v", requestID, choice, allow)
		err := pr.RespondToPermission(requestID, allow, msg)
		if err != nil {
			log.Errorf("agent/perm", "RespondToPermission failed: reqID=%s sk=%s err=%v", requestID, sessionKey, err)
		}
		return err
	}

	// Remember-flag response (opencode). Distinct from permResponder above:
	// the third arg is a `remember` bool (persist the decision), not a denial
	// message — so opencode satisfies THIS interface, not permResponder. A
	// backend can only satisfy one (Go matches the exact method signature), so
	// these two blocks never both fire. opencode surfaces Allow/Deny/Always
	// buttons with Data "allow"/"deny"/"always".
	type rememberPermResponder interface {
		RespondToPermission(requestID string, allow bool, remember bool) error
	}
	if pr, ok := be.(rememberPermResponder); ok && requestID != "" {
		remember := choice == "always" || strings.HasPrefix(choice, "allow_always")
		allow := remember || choice == "allow" || strings.HasPrefix(choice, "allow")
		log.Debugf("agent/perm", "responding via remember-protocol: reqID=%s choice=%q allow=%v remember=%v", requestID, choice, allow, remember)
		err := pr.RespondToPermission(requestID, allow, remember)
		if err != nil {
			log.Errorf("agent/perm", "RespondToPermission (remember) failed: reqID=%s sk=%s err=%v", requestID, sessionKey, err)
		}
		return err
	}

	// Keystroke-based response (tmux backend).
	return be.SendKeystroke(ctx, choice)
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
	qr, ok := be.(delegator.QuestionResponder)
	if !ok {
		return false
	}
	reqID := qr.HasPendingQuestion()
	if reqID == "" {
		return false
	}
	log.Debugf("agent/perm", "cancelling pending question via /stop: reqID=%s session=%s", reqID, sessionKey)
	_ = qr.CancelQuestion(reqID)
	return true
}
