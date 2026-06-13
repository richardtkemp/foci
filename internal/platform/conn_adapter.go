package platform

import "context"

// BotPtr constrains the concrete connection type an adapter wraps: a comparable
// (pointer) type implementing Connection. The comparable bound lets the adapter
// turn a nil concrete value into a true nil Connection interface.
type BotPtr interface {
	comparable
	Connection
}

// ConnectionSource is the per-platform bot manager an adapter delegates to.
// telegram.BotManager and discord.BotManager satisfy ConnectionSource[*Bot].
type ConnectionSource[B BotPtr] interface {
	PrimaryBot(agentID string) B
	BotForSession(sessionKey string) B
	BotForSessionOrPrimary(sessionKey, agentID string) B
	AcquireFacet(agentID string) (B, bool)
	StartAll(ctx context.Context)
	Wait()
	HasFacet(agentID string) bool
}

// ConnectionManagerAdapter adapts a ConnectionSource into a ConnectionManager,
// converting concrete typed-nil bot returns into true nil Connection interfaces.
type ConnectionManagerAdapter[B BotPtr] struct {
	source ConnectionSource[B]
}

// NewConnectionManagerAdapter wraps a per-platform bot manager as a
// platform.ConnectionManager.
func NewConnectionManagerAdapter[B BotPtr](src ConnectionSource[B]) *ConnectionManagerAdapter[B] {
	return &ConnectionManagerAdapter[B]{source: src}
}

// conn centralises the typed-nil → nil-interface conversion.
func (a *ConnectionManagerAdapter[B]) conn(b B) Connection {
	var zero B
	if b == zero {
		return nil
	}
	return b
}

func (a *ConnectionManagerAdapter[B]) Primary(agentID string) Connection {
	return a.conn(a.source.PrimaryBot(agentID))
}

func (a *ConnectionManagerAdapter[B]) AllForAgent(agentID string) []Connection {
	if c := a.conn(a.source.PrimaryBot(agentID)); c != nil {
		return []Connection{c}
	}
	return nil
}

func (a *ConnectionManagerAdapter[B]) ForSession(sessionKey string) Connection {
	return a.conn(a.source.BotForSession(sessionKey))
}

func (a *ConnectionManagerAdapter[B]) ForSessionOrPrimary(sessionKey, agentID string) Connection {
	return a.conn(a.source.BotForSessionOrPrimary(sessionKey, agentID))
}

func (a *ConnectionManagerAdapter[B]) AcquireFacet(agentID string) (Connection, bool) {
	b, ok := a.source.AcquireFacet(agentID)
	return a.conn(b), ok
}

func (a *ConnectionManagerAdapter[B]) StartAll(ctx context.Context) { a.source.StartAll(ctx) }
func (a *ConnectionManagerAdapter[B]) Wait()                        { a.source.Wait() }
func (a *ConnectionManagerAdapter[B]) HasFacet(agentID string) bool { return a.source.HasFacet(agentID) }
