package platform

// notifyAgentDoc sends a document to ALL connections for an agent (test-only).
func (m *Messaging) notifyAgentDoc(agentID string, path string) {
	if m == nil {
		return
	}
	for _, conn := range m.connMgr.AllForAgent(agentID) {
		_ = conn.SendDocument(path)
	}
}

// wait waits for all platform connections to finish (test-only).
func (m *Messaging) wait() {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.ConnectionManager().Wait()
	}
}
