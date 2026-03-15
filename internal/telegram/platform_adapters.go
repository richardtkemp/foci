package telegram

import (
	"context"

	"foci/internal/platform"
)

// Compile-time checks.
var (
	_ platform.Connection        = (*Bot)(nil)
	_ platform.ConnectionManager = (*ConnectionManagerAdapter)(nil)
)

// ConnectionManagerAdapter wraps BotManager to satisfy platform.ConnectionManager.
// It converts concrete *Bot returns to the platform.Connection interface.
type ConnectionManagerAdapter struct {
	*BotManager
}

func (a *ConnectionManagerAdapter) Primary(agentID string) platform.Connection {
	b := a.BotManager.PrimaryBot(agentID)
	if b == nil {
		return nil
	}
	return b
}

func (a *ConnectionManagerAdapter) AllForAgent(agentID string) []platform.Connection {
	b := a.BotManager.PrimaryBot(agentID)
	if b == nil {
		return nil
	}
	return []platform.Connection{b}
}

func (a *ConnectionManagerAdapter) ForSession(sessionKey string) platform.Connection {
	b := a.BotManager.BotForSession(sessionKey)
	if b == nil {
		return nil
	}
	return b
}

func (a *ConnectionManagerAdapter) ForSessionOrPrimary(sessionKey, agentID string) platform.Connection {
	b := a.BotManager.BotForSessionOrPrimary(sessionKey, agentID)
	if b == nil {
		return nil
	}
	return b
}

func (a *ConnectionManagerAdapter) AcquireFacet(agentID string) (platform.Connection, bool) {
	b, ok := a.BotManager.AcquireFacet(agentID)
	if b == nil {
		return nil, ok
	}
	return b, ok
}

func (a *ConnectionManagerAdapter) StartAll(ctx context.Context) {
	a.BotManager.StartAll(ctx)
}

func (a *ConnectionManagerAdapter) Wait() {
	a.BotManager.Wait()
}

func (a *ConnectionManagerAdapter) HasFacet(agentID string) bool {
	return a.BotManager.HasFacet(agentID)
}
