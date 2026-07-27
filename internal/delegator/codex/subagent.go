package codex

import (
	"sync"
)

// subagentTracker maps Codex's stable child thread ID to the foci subagent
// group. Codex emits a new collab item ID for every spawn/input/resume call;
// that ID is not a UI identity. The child thread ID survives those calls.
//
// A subagent runs in its own codex thread, and the app-server streams that
// thread's entire lifecycle down the SAME connection as the parent's — turn
// start/completion, message deltas, completed items, token usage, all tagged
// with the child's threadId (verified live on 0.144.5 and 0.145.0, for both
// the subagents feature and collab mode). So this tracker exists to answer one
// question for the reader: "is this threadId a child, and which run is it?"
// Everything the UI needs then arrives as ordinary notifications.
//
// It used to poll each child with thread/read every 500ms instead. That was
// unnecessary — the data was already being delivered — and actively harmful: a
// full re-read needs a de-dup cursor (#1571), and a collab child's thread is a
// FORK of its parent, so a full re-read replays the parent's own transcript
// into the subagent panel (#1592). Consuming the stream has neither problem,
// because each item arrives exactly once and only the child's own items do.
type subagentTracker struct {
	mu         sync.Mutex
	active     map[string]*subagentRun // agentThreadId -> the run currently open
	identities map[string]*subagentIdentity
}

type subagentIdentity struct {
	groupKey string
	label    string
	runIndex int
}

// subagentRun is the run currently open for a child thread: the UI group it
// belongs to and which run within that group. A child may run many times —
// codex starts a fresh turn on the child's thread for every parent
// interaction — and each of those is a run.
type subagentRun struct {
	groupKey string
	runIndex int
}

func newSubagentTracker() *subagentTracker {
	return &subagentTracker{
		active:     make(map[string]*subagentRun),
		identities: make(map[string]*subagentIdentity),
	}
}

// start binds an agent thread to a stable foci group. It returns started=false
// when the child is already active, preventing follow-up items from opening a
// second UI subagent. An inactive identity is a reactivation and gets a new
// run index while retaining the original group key.
func (st *subagentTracker) start(agentThreadID, groupKey, label string) (*subagentRun, bool) {
	if agentThreadID == "" {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if run := st.active[agentThreadID]; run != nil {
		if label != "" {
			st.identities[agentThreadID].label = label
		}
		return run, false
	}
	identity := st.identities[agentThreadID]
	if identity == nil {
		if groupKey == "" {
			return nil, false
		}
		identity = &subagentIdentity{groupKey: groupKey, label: label, runIndex: 0}
		st.identities[agentThreadID] = identity
	} else if identity.groupKey == "" {
		identity.groupKey = groupKey
	}
	if label != "" {
		identity.label = label
	}
	identity.runIndex++
	run := &subagentRun{groupKey: identity.groupKey, runIndex: identity.runIndex}
	st.active[agentThreadID] = run
	return run, true
}

// isChild reports whether agentThreadID belongs to a subagent this backend has
// seen. The identity outlives any single run, so a notification arriving
// between runs is still recognised as the child's and kept away from the
// process owner.
func (st *subagentTracker) isChild(agentThreadID string) bool {
	if agentThreadID == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.identities[agentThreadID] != nil
}

// current returns the open run for a child thread, or nil when it is between
// runs.
func (st *subagentTracker) current(agentThreadID string) *subagentRun {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.active[agentThreadID]
}

// stop closes the child's open run and returns it for the matching
// OnSubagentEnd callback. The identity is deliberately retained so a later
// reactivation reuses the same UI group with a fresh run index.
func (st *subagentTracker) stop(agentThreadID string) *subagentRun {
	st.mu.Lock()
	defer st.mu.Unlock()
	run := st.active[agentThreadID]
	if run != nil {
		delete(st.active, agentThreadID)
	}
	return run
}

// stopAll drops every open run WITHOUT emitting ends. It is for teardown of
// the whole backend, where the UI state is going away regardless. It is
// deliberately not called at parent-turn boundaries: a child routinely
// outlives its parent's turn (proven live — a parent's turn/completed arrived
// with the child still working and no terminal signal for it, #1588), so
// ending runs there reported children as finished while they were still going.
func (st *subagentTracker) stopAll() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.active = make(map[string]*subagentRun)
}
