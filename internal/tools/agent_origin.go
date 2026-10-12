package tools

import "foci/internal/session"

// AgentOrigin identifies the sending side of an agent-to-agent message
// (send_to_session): the calling session's key, the agent id parsed from that
// key, and the caller's raw message text — before prompts.FormatInjectedMessage
// wraps it in the "[SYSTEM INJECTION …]" note the receiving agent sees.
//
// It rides the delivery request through the notifier seam (InjectAgentMessage)
// and SessionNotifyFn, and is what the receiving chat's chit is built from.
// The injection's trigger name cannot tell an agent message from the async
// tool results, tmux watches and plan commands that share the plumbing, so a
// non-nil origin IS the discriminator: every non-agent injection path delivers
// with a nil origin and stays chit-free (#1286).
type AgentOrigin struct {
	// SessionKey is the CALLING session's key. Empty for an exec-bridge call
	// with no session in context — the chit then carries no sender fields.
	SessionKey string
	// AgentID is the calling session's agent id, parsed from SessionKey;
	// empty when the key does not parse.
	AgentID string
	// Text is the caller's raw message parameter.
	Text string
}

// NewAgentOrigin builds an AgentOrigin, deriving the agent id from sessionKey.
func NewAgentOrigin(sessionKey, text string) AgentOrigin {
	return AgentOrigin{SessionKey: sessionKey, AgentID: agentIDOf(sessionKey), Text: text}
}

// agentIDOf parses the agent id out of a session key, or "" when the key does
// not parse.
func agentIDOf(sessionKey string) string {
	if sk, err := session.ParseSessionKey(sessionKey); err == nil {
		return sk.AgentID
	}
	return ""
}
