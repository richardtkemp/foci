package route

import (
	"foci/internal/platform"
)

// DeliveryOutcome classifies how an outbound delivery connection was resolved,
// so senders and logs can tell "landed in the session's own chat" from "fell
// back to the agent's primary" from "nothing live to deliver through".
type DeliveryOutcome string

const (
	// DeliveredToSession: the session's own live connection.
	DeliveredToSession DeliveryOutcome = "session"
	// DeliveredViaPrimary: no session connection; the owning platform's
	// primary connection was used (fallback policies only).
	DeliveredViaPrimary DeliveryOutcome = "primary"
	// DeliveryNone: nothing live to deliver through (conn is nil).
	DeliveryNone DeliveryOutcome = "none"
)

// ConnFor resolves the delivery connection for a session through the ONE
// outbound cascade:
//
//  1. The session's own live connection (facet bot, app conversation binding,
//     the chat embedded in the key) — every policy.
//  2. Policy-dependent fallback: PolicyStrict stops (DeliveryNone);
//     PolicyFallback falls back to the owning platform's primary.
//
// The returned connection is nil exactly when the outcome is DeliveryNone.
// Callers should still run the agent turn where applicable (the session JSONL
// records it) and log the outcome — a message that fell back or went
// undelivered must never look delivered.
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
