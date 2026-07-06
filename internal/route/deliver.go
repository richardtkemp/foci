package route

import (
	"foci/internal/platform"
	"foci/internal/session"
)

// DeliveryOutcome classifies how an outbound delivery connection was resolved,
// so senders and logs can tell "landed in the session's own chat" from "fell
// back to the agent's primary" from "deliberately not delivered".
type DeliveryOutcome string

const (
	// DeliveredToSession: the session's own live connection.
	DeliveredToSession DeliveryOutcome = "session"
	// DeliveredViaPrimary: no session connection; the owning platform's
	// primary connection was used (fallback policies only).
	DeliveredViaPrimary DeliveryOutcome = "primary"
	// DeliverySuppressed: a branch session with no dedicated connection —
	// delivering via the primary would leak the branch's output into the
	// parent's chat, so the send is deliberately dropped (conn is nil).
	DeliverySuppressed DeliveryOutcome = "suppressed"
	// DeliveryNone: nothing live to deliver through (conn is nil).
	DeliveryNone DeliveryOutcome = "none"
)

const (
	// PolicyRootFallback falls back to the agent's primary connection for
	// ROOT sessions only; branch sessions without their own connection are
	// suppressed rather than leaked into the parent's chat. This is the
	// right policy for agent-initiated replies (async notify, cross-session
	// sends): a facet's reply belongs to the facet, not the main chat.
	PolicyRootFallback Policy = "root-fallback"
)

// ConnFor resolves the delivery connection for a session through the ONE
// outbound cascade:
//
//  1. The session's own live connection (facet bot, app conversation binding,
//     the chat embedded in the key) — every policy.
//  2. Policy-dependent fallback: PolicyStrict stops (DeliveryNone);
//     PolicyRootFallback suppresses branch sessions then falls back to the
//     owning platform's primary; PolicyFallback always falls back.
//
// The returned connection is nil exactly when the outcome is
// DeliverySuppressed or DeliveryNone. Callers should still run the agent turn
// where applicable (the session JSONL records it) and log the outcome — a
// message that fell back or went undelivered must never look delivered.
func ConnFor(cm platform.ConnectionManager, agentID, sessionKey string, policy Policy) (platform.Connection, DeliveryOutcome) {
	if cm == nil {
		return nil, DeliveryNone
	}
	if conn := cm.ForSession(sessionKey); conn != nil {
		return conn, DeliveredToSession
	}
	if policy == PolicyStrict {
		return nil, DeliveryNone
	}
	if policy == PolicyRootFallback {
		if sk, err := session.ParseSessionKey(sessionKey); err == nil && !sk.IsRoot() {
			return nil, DeliverySuppressed
		}
	}
	if conn := cm.ForSessionOrPrimary(sessionKey, agentID); conn != nil {
		return conn, DeliveredViaPrimary
	}
	return nil, DeliveryNone
}

// Broadcast returns every live connection for an agent across all platforms
// — the delivery set for PolicyBroadcast targets and for agent-wide notices
// (rate-limit, max-tokens warnings). Callers choose the send method
// (SendNotification for notices, SendText for messages) per connection.
func Broadcast(cm platform.ConnectionManager, agentID string) []platform.Connection {
	if cm == nil {
		return nil
	}
	return cm.AllForAgent(agentID)
}
