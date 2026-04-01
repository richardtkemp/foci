package agent

import (
	"context"
	"strings"

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
		log.Debugf("agent/perm", "responding via protocol: reqID=%s allow=%v", requestID, allow)
		return pr.RespondToPermission(requestID, allow, msg)
	}

	// Keystroke-based response (tmux backend).
	return be.SendKeystroke(ctx, choice)
}
