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

// Broadcast returns every connection an agent-wide send can reach: all live
// connections for the agent across all platforms, MINUS connections that
// declare they drop agent-wide sends (platform.DropsAgentWideSends — e.g.
// Slack's unbound conn, whose conversations are never default chats):
// addressing one is a certain drop, so it is not a delivery target. Note the
// app's unbound conn is NOT a dropper — it routes agent-wide sends to the
// default conversation — and stays in the set. This is the delivery set for
// PolicyBroadcast targets and for the agent-wide notices fanned out by
// broadcastNotify: the rate-limit GATE notice (Agent.RateLimitFunc,
// "⚡ Rate limited (resets …)") and max-tokens warnings. Callers choose the
// send method (SendNotification for notices, SendText for messages) per
// connection.
//
// A session's OWN conversation is not this function's business: broadcastResponse
// delivers that through the session's ForSession connection, which must not
// rely on appearing in — or being identity-equal to anything in — this set.
//
// It is NOT the path for CC's usage-limit warning (ccstream's rate_limit_event,
// #1211/#1238). That one is delivered to a single chosen chat per
// [notify] rate_limit_notify_to — ConnFor for the session, Primary for the
// default, or both — and never broadcast (#1857).
func Broadcast(cm platform.ConnectionManager, agentID string) []platform.Connection {
	if cm == nil {
		return nil
	}
	var conns []platform.Connection
	for _, c := range cm.AllForAgent(agentID) {
		if platform.DropsAgentWideSends(c) {
			continue
		}
		conns = append(conns, c)
	}
	return conns
}
