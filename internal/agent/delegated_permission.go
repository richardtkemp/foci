package agent

import (
	"context"
	"strings"

	"foci/internal/backend"
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
	if qr, ok := be.(backend.QuestionResponder); ok && requestID != "" {
		if choice == "qa:cancel" {
			log.Debugf("agent/perm", "cancelling question: reqID=%s", requestID)
			return qr.CancelQuestion(requestID)
		}
		if strings.HasPrefix(choice, "qa:") {
			log.Debugf("agent/perm", "answering question: reqID=%s choice=%q", requestID, choice)
			return qr.RespondToQuestion(requestID, choice)
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
				return rr.RespondToPermissionWithRule(requestID, prefix)
			}
			// Fall through to simple allow if rule not supported.
		}

		allow := choice == "allow" || strings.HasPrefix(choice, "allow")
		msg := ""
		if !allow {
			msg = "User denied permission"
		}
		log.Debugf("agent/perm", "responding via protocol: reqID=%s choice=%q allow=%v", requestID, choice, allow)
		return pr.RespondToPermission(requestID, allow, msg)
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
	qr, ok := be.(backend.QuestionResponder)
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
