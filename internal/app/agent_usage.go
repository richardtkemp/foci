package app

import (
	"reflect"
	"sync"
	"time"

	"foci/internal/fap"
)

// Agent usage in the roster (#3151; Dick 2026-11-17: "New conversation dialog
// in the app: show a small indicator of the agent's usage allowance, if
// available").
//
// The gateway registers, per agent whose backend has a usage source, a
// function that returns the agent's allowance as last stored (the usage
// history, #3103). agentRoster calls it for each agent and puts the answer in
// AgentInfo.Usage. An agent with no source, or whose source returns nil, has
// no Usage: the app shows nothing for it.
//
// The registry is package-level, not on the Hub, because the gateway wires
// agents (tool_table.go) before and independently of the app hub's start.

var agentUsage struct {
	mu      sync.RWMutex
	sources map[string]func() *fap.AgentUsage
	// last is the usage per agent in the last roster sent to every socket
	// (pushHelloAll); AgentUsageChanged compares a fresh read with it so an
	// unchanged read sends nothing. A roster sent to one socket (its connect)
	// does not count: the others may still hold older figures.
	last map[string]*fap.AgentUsage
}

// SetAgentUsageSource registers fn as agentID's allowance source; nil fn
// removes it.
func SetAgentUsageSource(agentID string, fn func() *fap.AgentUsage) {
	agentUsage.mu.Lock()
	defer agentUsage.mu.Unlock()
	if fn == nil {
		delete(agentUsage.sources, agentID)
		return
	}
	if agentUsage.sources == nil {
		agentUsage.sources = map[string]func() *fap.AgentUsage{}
	}
	agentUsage.sources[agentID] = fn
}

// agentUsageFor returns each of ids' usage from its registered source
// (agents with none, or a nil answer, are absent). It must be called without
// h.mu held: a source reads the usage history database.
func agentUsageFor(ids []string) map[string]*fap.AgentUsage {
	agentUsage.mu.RLock()
	fns := make(map[string]func() *fap.AgentUsage, len(ids))
	for _, id := range ids {
		if fn := agentUsage.sources[id]; fn != nil {
			fns[id] = fn
		}
	}
	agentUsage.mu.RUnlock()
	out := make(map[string]*fap.AgentUsage, len(fns))
	for id, fn := range fns {
		if u := fn(); u != nil && (len(u.Windows) > 0 || len(u.Accounts) > 0) {
			out[id] = u
		}
	}
	return out
}

// noteRosterUsage records the usage a roster sent to every socket carried,
// for AgentUsageChanged.
func noteRosterUsage(u map[string]*fap.AgentUsage) {
	agentUsage.mu.Lock()
	agentUsage.last = u
	agentUsage.mu.Unlock()
}

// AgentUsageChanged tells the app that new usage figures were stored (the
// gateway calls it after each successful usage read). When the agents'
// usage differs from what the last roster carried, the roster is re-sent to
// every live socket, so an open app shows the new figures without a
// reconnect. A no-op when the app provider is not running.
func AgentUsageChanged() {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.agentUsageChanged()
}

// usageRecheckEvery is how often the hub compares the agents' usage with what
// the last roster carried when no read asks for it (#3267). The over-pace mark
// depends on the time as well as on the figures: without this it would stay
// set while reads fail or are off ([tools] spawn_reserve_percent = 0).
const usageRecheckEvery = 5 * time.Minute

// startUsageRecheck starts the periodic recheck, once per hub, on the gateway
// context (see startAutoCloseSweep); a hub with no context (every test-built
// hub) starts no loop.
func (h *Hub) startUsageRecheck() {
	ctx := h.deps.Ctx
	if ctx == nil {
		return
	}
	h.usageRecheckOnce.Do(func() {
		safeGo("usage-recheck", func() {
			ticker := time.NewTicker(usageRecheckEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if ctx.Err() != nil {
						return
					}
					h.agentUsageChanged()
				}
			}
		})
	})
}

func (h *Hub) agentUsageChanged() {
	h.mu.RLock()
	ids := append([]string(nil), h.agentOrder...)
	h.mu.RUnlock()
	fresh := agentUsageFor(ids)
	agentUsage.mu.RLock()
	same := sameShownUsage(fresh, agentUsage.last)
	agentUsage.mu.RUnlock()
	if same {
		return
	}
	h.pushHelloAll()
}

// sameShownUsage reports whether a and b show the same figures: AsOfMs is
// ignored, since every read moves it and a re-sent roster for it alone would
// change nothing the app shows. Each login's figures, name and pick mark
// (#3253) count, and so does the over-pace mark (#3267).
func sameShownUsage(a, b map[string]*fap.AgentUsage) bool {
	stripWindows := func(in []fap.UsageWindow) []fap.UsageWindow {
		ws := make([]fap.UsageWindow, len(in))
		for i, w := range in {
			w.AsOfMs = 0
			ws[i] = w
		}
		return ws
	}
	strip := func(m map[string]*fap.AgentUsage) map[string]fap.AgentUsage {
		out := make(map[string]fap.AgentUsage, len(m))
		for id, u := range m {
			s := fap.AgentUsage{Windows: stripWindows(u.Windows), OverPace: u.OverPace}
			for _, a := range u.Accounts {
				a.Windows = stripWindows(a.Windows)
				s.Accounts = append(s.Accounts, a)
			}
			out[id] = s
		}
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}
