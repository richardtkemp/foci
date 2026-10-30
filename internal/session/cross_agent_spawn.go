package session

// A cross-agent spawn (#2662) runs as another agent: its sessions are that
// agent's, but they belong to no chat of its. They all hang off one
// independent root per target agent, <target>/ixspawn, so every place that
// would reach a chat for a session (connection lookups, send_to_chat, typing,
// autonomous-run delivery) can tell them apart with one check and keep them
// out of the target's chats. The result goes back to the caller's session,
// which is another agent's key.

// crossAgentSpawnID is the id of the root a cross-agent spawn's sessions hang off.
const crossAgentSpawnID = "xspawn"

// CrossAgentSpawnRoot is the root key a cross-agent spawn running as
// agentID hangs its sessions off.
func CrossAgentSpawnRoot(agentID string) string {
	return SessionKey{AgentID: agentID, Type: 'i', ID: crossAgentSpawnID}.String()
}

// IsCrossAgentSpawnKey reports whether key is a cross-agent spawn's session
// (the root or a child of it).
func IsCrossAgentSpawnKey(key string) bool {
	sk, err := ParseSessionKey(key)
	return err == nil && sk.Type == 'i' && sk.ID == crossAgentSpawnID
}
