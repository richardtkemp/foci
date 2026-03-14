package mcp

import "context"

// newManager creates an empty MCP manager with no connections (test-only).
func newManager() *Manager {
	return &Manager{}
}

// connect connects to all configured MCP servers (test-only wrapper).
func (m *Manager) connect(ctx context.Context, servers []ServerConfig) error {
	return m.connectWith(ctx, servers, nil)
}

// serverCount returns the number of connected servers (test-only).
func (m *Manager) serverCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}

// toolCount returns the total number of tools across all connected servers (test-only).
func (m *Manager) toolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.servers {
		n += len(s.tools)
	}
	return n
}
