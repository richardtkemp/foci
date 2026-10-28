package tools

// Spawn ownership across a depth tree (#2624). A depth leaf may report while
// its spawns still run. Its spawns then pass to its parent node, but only at
// the handover point: when the parent RECEIVES the leaf's report (its
// [DEPTH RESULTS] fan-in turn has run). Between the report and that fan-in,
// whatever the spawn sends its owner is held, neither delivered nor dropped;
// after it, it goes to the new owner. This repeats node by node up to the
// root chat. The spawn's record keeps the session that started it; the owner
// is resolved at each delivery (SpawnDeps.Route), so nothing is rewritten when
// a leaf reports, and a held result is simply its persisted Result, which a
// restart routes, and holds, again.

import (
	"fmt"
	"slices"

	"foci/internal/session"
)

// SpawnRoute is where a spawn's messages to its parent go.
type SpawnRoute struct {
	// Owner is the session that owns the spawn now: the one that started it
	// until it hands over, then the depth node it was handed to.
	Owner string
	// Hold: the owner has reported and its report has not reached its
	// parent's fan-in yet, so nothing may be delivered for now.
	Hold bool
	// Via names the session that started the spawn when Owner is another
	// one, for the note on what Owner receives ("" when Owner started it).
	Via string
}

// reparentedSep joins results held together for one spawn.
const reparentedSep = "\n\n---\n\n"

// route resolves the owner of parent's spawns.
func (s *Spawner) route(parent string) SpawnRoute {
	if s.deps.Route == nil {
		return SpawnRoute{Owner: parent}
	}
	r := s.deps.Route(parent)
	if r.Owner == "" {
		r.Owner = parent
	}
	return r
}

// reparentedNote heads a result delivered to a session that did not start
// the spawn.
func reparentedNote(via string) string {
	return fmt.Sprintf("[REPARENTED SPAWN] This spawn was started by %s, which has since reported; its results now come to you. "+
		"You can check on it and message it with spawn_status / spawn_send, as with your own spawns.", via)
}

// inject hands msg (parent's spawn id's recorded result) to r's owner; the
// record keeps it until the owner's turn has taken it.
func (s *Spawner) inject(parent, id, msg string, r SpawnRoute) {
	tr := s.deps.Tracker
	text := msg
	if r.Owner != parent && r.Via != "" {
		text = reparentedNote(r.Via) + "\n\n" + msg
		spawnLog.Infof("spawn=%s result for %s delivered to its new owner %s", id, parent, r.Owner)
	}
	s.deps.Notifier.InjectToAgentThen(r.Owner, text, "async_notify", func(delivered bool) {
		if delivered {
			tr.clearResult(parent, id, msg)
			return
		}
		spawnLog.Infof("spawn=%s result not taken by %s (refused) — kept for the next process", id, r.Owner)
	})
}

// ReleaseHeld delivers every held spawn result of this agent whose owner may
// now take it. The depth manager calls it after each handover point (a fan-in
// taken, a reset) and after its restore.
func (s *Spawner) ReleaseHeld() {
	tr := s.deps.Tracker
	for _, h := range tr.heldResults(s.deps.AgentID) {
		r := s.route(h.parent)
		if r.Hold {
			continue
		}
		if msg, ok := tr.unhold(h.parent, h.id); ok {
			s.inject(h.parent, h.id, msg, r)
		}
	}
}

// ownedLookup finds a spawn the caller owns but did not start: one whose
// starting session handed it over to caller (#2624). It returns the starting
// session too, which the tracker keys the spawn under.
func (s *Spawner) ownedLookup(caller, idOrBranch string, n int) (string, SpawnSnapshot, bool) {
	if s.deps.Route == nil {
		return "", SpawnSnapshot{}, false
	}
	for _, p := range s.deps.Tracker.parents(s.deps.AgentID) {
		if p == caller || s.route(p).Owner != caller {
			continue
		}
		if snap, ok := s.deps.Tracker.Lookup(p, idOrBranch, n); ok {
			return p, snap, true
		}
	}
	return "", SpawnSnapshot{}, false
}

// lookupForCaller finds a spawn of caller's, its own or handed over to it,
// and the session it is keyed under.
func (s *Spawner) lookupForCaller(caller, idOrBranch string, n int) (string, SpawnSnapshot, bool) {
	if snap, ok := s.deps.Tracker.Lookup(caller, idOrBranch, n); ok {
		return caller, snap, true
	}
	return s.ownedLookup(caller, idOrBranch, n)
}

// listForCaller lists caller's spawns, its own and those handed over to it;
// a handed-over one's description says where it came from.
func (s *Spawner) listForCaller(caller string) []SpawnSnapshot {
	out := s.deps.Tracker.List(caller)
	if s.deps.Route == nil {
		return out
	}
	for _, p := range s.deps.Tracker.parents(s.deps.AgentID) {
		if p == caller {
			continue
		}
		r := s.route(p)
		if r.Owner != caller {
			continue
		}
		for _, sp := range s.deps.Tracker.List(p) {
			sp.Description = fmt.Sprintf("(from %s) %s", r.Via, sp.Description)
			out = append(out, sp)
		}
	}
	sortSpawns(out, func(s SpawnSnapshot) RunningSpawn { return s.RunningSpawn })
	return out
}

// --- tracker side ---

// pushResult records msg as the spawn's result on its way to its owner and
// returns what is to be delivered. hold keeps it back (#2624); a result
// already held is kept in front of msg, so nothing held is overwritten.
func (t *SpawnTracker) pushResult(parent, id, msg string, hold bool) string {
	if t == nil {
		return msg
	}
	t.mu.Lock()
	if s := t.spawns[parent][id]; s != nil {
		if s.held && s.result != "" && s.result != msg {
			msg = s.result + reparentedSep + msg
		}
		s.result, s.held = msg, hold
	}
	t.mu.Unlock()
	t.persist(parent, id)
	return msg
}

type heldResult struct{ parent, id string }

// heldResults lists agentID's spawns whose result is held.
func (t *SpawnTracker) heldResults(agentID string) []heldResult {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []heldResult
	for p, m := range t.spawns {
		if session.AgentIDFromKey(p) != agentID {
			continue
		}
		for id, s := range m {
			if s.held && s.result != "" {
				out = append(out, heldResult{p, id})
			}
		}
	}
	return out
}

// unhold releases a held result and returns it; false when it is no longer
// held (another release took it).
func (t *SpawnTracker) unhold(parent, id string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || !s.held || s.result == "" {
		return "", false
	}
	s.held = false
	return s.result, true
}

// parents lists the sessions agentID's tracked spawns were started by.
func (t *SpawnTracker) parents(agentID string) []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for p, m := range t.spawns {
		if len(m) > 0 && session.AgentIDFromKey(p) == agentID {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
