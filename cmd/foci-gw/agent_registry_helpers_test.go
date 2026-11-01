package main

import (
	"sort"
	"testing"
)

// at returns the registered agent with this id, or nil (test shorthand).
func (r *agentRegistry) at(id string) *agentInstance {
	inst, _ := r.Get(id)
	return inst
}

// registryOf builds a registry holding insts, in order.
func registryOf(t testing.TB, insts ...*agentInstance) *agentRegistry {
	t.Helper()
	r := newAgentRegistry()
	for _, inst := range insts {
		if err := r.commit(inst); err != nil {
			t.Fatalf("registryOf: %v", err)
		}
	}
	return r
}

// registryFromMap builds a registry from an id → agent map, ordered by id.
// Each instance's id is set from its key.
func registryFromMap(m map[string]*agentInstance) *agentRegistry {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	r := newAgentRegistry()
	for _, id := range ids {
		inst := m[id]
		inst.id = id
		_ = r.commit(inst)
	}
	return r
}
