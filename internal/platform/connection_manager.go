package platform

import (
	"context"
	"strings"

	"foci/internal/log"
	"foci/internal/session"
)

// --- Aggregating ConnectionManager ---

// aggregatingConnMgr delegates ConnectionManager calls across multiple platform
// managers. For ForSession/ForSessionOrPrimary, it uses a platform lookup
// function to dispatch directly to the correct platform, avoiding false-positive
// matches when chat IDs collide across platforms.
type aggregatingConnMgr struct {
	named           map[string]ConnectionManager // platform name → manager
	order           []string                     // iteration order
	chatPlatformFn  func(agentID string, chatID int64) string
}

func newAggregatingConnMgr(providers []MessagingProvider, chatPlatformFn func(agentID string, chatID int64) string) *aggregatingConnMgr {
	named := make(map[string]ConnectionManager, len(providers))
	order := make([]string, len(providers))
	for i, p := range providers {
		name := p.Name()
		named[name] = p.ConnectionManager()
		order[i] = name
	}
	return &aggregatingConnMgr{
		named:          named,
		order:          order,
		chatPlatformFn: chatPlatformFn,
	}
}

func (a *aggregatingConnMgr) Primary(agentID string) Connection {
	for _, name := range a.order {
		if c := a.named[name].Primary(agentID); c != nil {
			return c
		}
	}
	return nil
}

func (a *aggregatingConnMgr) AllForAgent(agentID string) []Connection {
	var conns []Connection
	for _, name := range a.order {
		conns = append(conns, a.named[name].AllForAgent(agentID)...)
	}
	return conns
}

func (a *aggregatingConnMgr) ForSession(sessionKey string) Connection {
	// Try platform-aware routing: extract agentID and chatID from the session key,
	// look up which platform owns it, and dispatch directly.
	if a.chatPlatformFn != nil {
		agentID, chatID := extractSessionInfo(sessionKey)
		if agentID != "" && chatID != 0 {
			if platformName := a.chatPlatformFn(agentID, chatID); platformName != "" {
				if mgr, ok := a.named[platformName]; ok {
					if c := mgr.ForSession(sessionKey); c != nil {
						return c
					}
					log.Debugf("platform", "platform %q claimed chat %d but ForSession returned nil for %s", platformName, chatID, sessionKey)
				}
			}
		}
	}

	// Fallback: iterate all platforms (first-message-ever case, facet bots, etc.)
	for _, name := range a.order {
		if c := a.named[name].ForSession(sessionKey); c != nil {
			return c
		}
	}
	return nil
}

func (a *aggregatingConnMgr) ForSessionOrPrimary(sessionKey, agentID string) Connection {
	if c := a.ForSession(sessionKey); c != nil {
		return c
	}
	// Platform-aware primary: if we know which platform owns this chat,
	// prefer that platform's primary bot over the first-match fallback.
	if a.chatPlatformFn != nil {
		_, chatID := extractSessionInfo(sessionKey)
		if chatID != 0 {
			if platName := a.chatPlatformFn(agentID, chatID); platName != "" {
				if mgr, ok := a.named[platName]; ok {
					if c := mgr.Primary(agentID); c != nil {
						return c
					}
				}
			}
		}
	}
	return a.Primary(agentID)
}

func (a *aggregatingConnMgr) AcquireFacet(agentID string) (Connection, bool) {
	for _, name := range a.order {
		if c, ok := a.named[name].AcquireFacet(agentID); ok {
			return c, true
		}
	}
	return nil, false
}

func (a *aggregatingConnMgr) HasFacet(agentID string) bool {
	for _, name := range a.order {
		if a.named[name].HasFacet(agentID) {
			return true
		}
	}
	return false
}

func (a *aggregatingConnMgr) StartAll(ctx context.Context) {
	for _, name := range a.order {
		a.named[name].StartAll(ctx)
	}
}

func (a *aggregatingConnMgr) Wait() {
	for _, name := range a.order {
		a.named[name].Wait()
	}
}

// extractSessionInfo extracts the agentID and chatID from a session key string.
// Returns ("", 0) if the key cannot be parsed.
func extractSessionInfo(sessionKey string) (string, int64) {
	parts := strings.SplitN(sessionKey, "/", 3)
	if len(parts) < 2 {
		return "", 0
	}
	chatID := session.ChatIDFromKey(sessionKey)
	return parts[0], chatID
}
