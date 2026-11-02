package main

import (
	"fmt"
	"slices"
	"sync"

	"foci/internal/command"
	"foci/internal/config"
)

// agentRegistry is the running process's set of agents: one locked map plus
// the registration order. Boot fills it in config order; `/agents new`
// appends a hot-added agent at the END (#963), so the first agent — the
// default target for HTTP, askgw and the shared facet pool — never changes
// while the process runs.
//
// Every reader that runs after boot (the HTTP handlers, askgw, the live
// appliers, the warning hook, the memory guard, the tmux monitor, the facet
// restore resolver, the agent resolver and list) reads through it, so a
// hot-added agent is visible to all of them at once, and adding one while
// they run is race-free.
//
// An agent is only ever added FULLY BUILT (instance built, inbox started):
// a hot-add first reserves its id (reserve), builds the agent off-registry,
// then publishes it (commit). A reserved id is not visible to any reader, but
// it blocks a second reserve of the same id, so two concurrent hot-adds of one
// id cannot both build.
type agentRegistry struct {
	mu       sync.RWMutex
	byID     map[string]*agentInstance
	order    []string
	reserved map[string]struct{}
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{
		byID:     make(map[string]*agentInstance),
		reserved: make(map[string]struct{}),
	}
}

// Get returns the agent with this id.
func (r *agentRegistry) Get(id string) (*agentInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.byID[id]
	return inst, ok
}

// Default returns the first-registered agent (the default target when a
// caller names none), or false when there is none.
func (r *agentRegistry) Default() (*agentInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil, false
	}
	return r.byID[r.order[0]], true
}

// Order returns a copy of the agent ids in registration order.
func (r *agentRegistry) Order() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// All returns the agents in registration order (a snapshot slice).
func (r *agentRegistry) All() []*agentInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*agentInstance, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Len returns the number of registered agents.
func (r *agentRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Snapshot returns a copy of the id → agent map, for boot- and shutdown-time
// passes that take a map.
func (r *agentRegistry) Snapshot() map[string]*agentInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*agentInstance, len(r.byID))
	for id, inst := range r.byID {
		out[id] = inst
	}
	return out
}

// Configs returns the config of every registered agent, in order. The bot
// token conflict check for a hot-add runs over these plus the new agent's.
func (r *agentRegistry) Configs() []config.AgentConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.AgentConfig, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id].agentCfg)
	}
	return out
}

// errAgentExists is returned by reserve for an id that is running or being
// started.
type errAgentExists struct{ id string }

func (e errAgentExists) Error() string {
	return fmt.Sprintf("agent %q is already running", e.id)
}

// reserve claims id for a hot-add. It fails when the id is registered or
// already reserved. The caller must end the claim with commit or release.
func (r *agentRegistry) reserve(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; ok {
		return errAgentExists{id}
	}
	if _, ok := r.reserved[id]; ok {
		return errAgentExists{id}
	}
	r.reserved[id] = struct{}{}
	return nil
}

// release drops a reservation whose build failed. Nothing is registered.
func (r *agentRegistry) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, id)
}

// commit publishes a fully built agent at the end of the order, ending any
// reservation of its id. It fails, registering nothing, when the id is
// already registered.
func (r *agentRegistry) commit(inst *agentInstance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, inst.id)
	if _, ok := r.byID[inst.id]; ok {
		return errAgentExists{inst.id}
	}
	r.byID[inst.id] = inst
	r.order = append(r.order, inst.id)
	return nil
}

// remove unregisters a running agent (its live stop, #2787) and returns its
// place in the order, so a live rename can put the new id there (the first
// agent is the default one). ok is false when id is not registered.
func (r *agentRegistry) remove(id string) (inst *agentInstance, slot int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok = r.byID[id]
	if !ok {
		return nil, -1, false
	}
	delete(r.byID, id)
	slot = slices.Index(r.order, id)
	r.order = slices.Delete(r.order, slot, slot+1)
	return inst, slot, true
}

// commitAt is commit with the agent placed at slot in the order (clamped to
// the end), for the new id of a live rename.
func (r *agentRegistry) commitAt(inst *agentInstance, slot int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, inst.id)
	if _, ok := r.byID[inst.id]; ok {
		return errAgentExists{inst.id}
	}
	r.byID[inst.id] = inst
	if slot < 0 || slot > len(r.order) {
		slot = len(r.order)
	}
	r.order = slices.Insert(r.order, slot, inst.id)
	return nil
}

// newAgentResolver returns the gateway's agent resolver (session_notify, the
// ask and async-notify paths): the registered agent, or nil.
func newAgentResolver(agents *agentRegistry) func(agentID string) *agentInstance {
	return func(agentID string) *agentInstance {
		inst, ok := agents.Get(agentID)
		if !ok {
			return nil
		}
		// Test-only: when a test stops an agent via the harness
		// control socket, present the agent as unreachable to the
		// session_notify resolver so the "unknown target agent"
		// drop-and-log path fires. Production code never sets this
		// flag; the resolver returns the live instance.
		if inst.stopped.Load() {
			return nil
		}
		return inst
	}
}

// newAgentLister returns the /agents list source (also the /agents new
// wizard's uniqueness check), in registration order.
func newAgentLister(agents *agentRegistry) func() []command.AgentInfo {
	return func() []command.AgentInfo {
		var infos []command.AgentInfo
		for _, inst := range agents.All() {
			id := inst.id
			sk := defaultSessionKeyFor(inst.ag, id)
			var mc int
			var lastAct string
			model := inst.ag.Model
			if sk != "" {
				mc, _ = inst.ag.Sessions.MessageCount(sk)
				lastAct = inst.ag.Sessions.LastActivity(sk)
				model = inst.ag.SessionModel(sk)
			}
			infos = append(infos, command.AgentInfo{
				ID:           id,
				SessionKey:   sk,
				Model:        model,
				MessageCount: mc,
				LastActivity: lastAct,
			})
		}
		return infos
	}
}
