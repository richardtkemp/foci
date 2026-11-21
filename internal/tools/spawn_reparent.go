package tools

// Spawn ownership across a depth tree (#2624). A depth leaf may report while
// its spawns still run. Its spawns then pass to its parent node, but only at
// the handover point: when the parent RECEIVES the leaf's report (its
// [DEPTH RESULTS] fan-in turn has run). Between the report and that fan-in,
// whatever the spawn sends its owner is held, neither delivered nor dropped;
// after it, it goes to the new owner. This repeats node by node up to the
// root chat.
//
// The owner is one piece of state, read by every surface (#3206). The spawn's
// record keeps the session that started it (its identity: the tracker's key,
// the persisted Parent). The tracker keeps, per starting session, its route
// (SpawnRoute: owner, hold, via), computed by SpawnDeps.Route when the first
// spawn of that session is tracked and re-read at every point the depth
// manager says an owner may have changed (Spawner.Reown, from
// DepthDeps.OnHandover). Every reader goes through that route: the delivery
// of results, the session-keyed queries (spawn_status, spawn_send, spawn_stop,
// the [running] line, /stop and the app's per-item stop), prompt routing
// (BranchOwner) and the app's view of the spawn (ownedObserver). When an
// owner changes, Reown moves each running spawn's entry in the app from the
// old owner's chat to the new one's. The route is in memory only: a restart
// computes it again from the persisted depth records, so a held result is
// simply its persisted Result, which a restart routes, and holds, again.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

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

// route is the owner of parent's spawns, as the tracker holds it.
func (s *Spawner) route(parent string) SpawnRoute {
	if s.deps.Tracker == nil {
		return s.liveRoute(parent)
	}
	return s.deps.Tracker.routeOf(parent)
}

// deliveryRoute is route for a delivery to the owner: the route the tracker
// holds, checked against liveRoute. Where they differ (an owner change no
// handover point has announced yet, such as depth mode switched off just
// before its OnDisable runs), the owners are re-read first (Reown), so a
// delivery is never held, or sent, by a stale route, and every other reader
// agrees with it from then on.
func (s *Spawner) deliveryRoute(parent string) SpawnRoute {
	r := s.route(parent)
	if s.deps.Tracker == nil {
		return r
	}
	if live := s.liveRoute(parent); live != r {
		spawnLog.Infof("spawn owner of %s changed unannounced (%+v -> %+v); re-reading the owners", parent, r, live)
		s.Reown()
		return s.route(parent)
	}
	return r
}

// liveRoute resolves the owner of parent's spawns now, through
// SpawnDeps.Route (read at call time: a test sets it after NewSpawner). The
// tracker calls it (setRouteResolver) to fill and refresh the routes every
// other reader uses.
func (s *Spawner) liveRoute(parent string) SpawnRoute {
	if s.deps.Route == nil {
		return SpawnRoute{Owner: parent}
	}
	return normRoute(parent, s.deps.Route(parent))
}

// normRoute fills an empty Owner with parent.
func normRoute(parent string, r SpawnRoute) SpawnRoute {
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
	text := msg
	if r.Owner != parent && r.Via != "" {
		text = reparentedNote(r.Via) + "\n\n" + msg
		spawnLog.Infof("spawn=%s result for %s delivered to its new owner %s", id, parent, r.Owner)
	}
	// An owner that is itself a background spawn takes the result as its
	// own input, and is resumed by it when its run has ended; a stopped one
	// passes it up to its own owner (#3163).
	rep := s.resultReport(parent, id, msg)
	rep.Text = text
	s.deliverChild(r.Owner, rep)
}

// Reown re-reads the owner of every spawn of this agent's sessions (#3206)
// and delivers each held result whose owner may now take it (#2624). The
// depth manager calls it at each point an owner may change
// (DepthDeps.OnHandover): a node reported or dismissed, a fan-in taken, a
// reset, its restore, and depth mode turned off (#2628) or on.
func (s *Spawner) Reown() {
	if tr := s.deps.Tracker; tr != nil {
		// Under obsMu, so no event of a moved spawn reaches the app
		// between the owner change and the move (ownedObserver).
		tr.obsMu.Lock()
		for _, m := range tr.reroute(s.deps.AgentID) {
			spawnLog.Infof("spawn=%s (started by %s) passes from %s to %s", m.info.ID, m.parent, m.from.Owner, m.to.Owner)
			s.showMoved(m)
		}
		tr.obsMu.Unlock()
	}
	s.releaseHeld()
}

// spawnHandedOverNote ends a moved spawn's entry in the chat it leaves.
const spawnHandedOverNote = "↪ Handed over: this conversation has reported, so this spawn now belongs to the one it reported to, and carries on there."

// showMoved moves a running spawn's entry in the app (#3206) from the chat
// of its old owner, where its view is closed with a note, to its new owner's,
// where it is listed as running and opens with its task, headed by where it
// came from. Caller holds the tracker's obsMu.
func (s *Spawner) showMoved(m movedSpawn) {
	obs := s.deps.Observer
	if o, ok := obs.(ownedObserver); ok {
		obs = o.inner
	}
	if obs == nil {
		return
	}
	obs.SpawnText(m.from.Owner, m.info.ID, spawnHandedOverNote)
	obs.SpawnEnded(m.from.Owner, m.info.ID)
	prompt := m.prompt
	if m.to.Via != "" {
		prompt = "↪ Handed over from " + m.to.Via + ".\n\n" + prompt
	}
	obs.SpawnStarted(m.to.Owner, m.info, prompt)
}

// ownedObserver is the SpawnObserver a Spawner reports through (NewSpawner,
// #3206): it shows each event in the chat of the spawn's current owner,
// whichever session started it, so a spawn handed over up a depth tree is
// shown where its results go. Each event is told under the tracker's obsMu,
// which Reown holds while it moves a spawn, so no event falls between an
// owner change and the move.
type ownedObserver struct {
	inner SpawnObserver
	t     *SpawnTracker
}

var _ SpawnObserver = ownedObserver{}

// owned runs fn with the owner of parent's spawns, under obsMu.
func (o ownedObserver) owned(parent string, fn func(owner string)) {
	o.t.obsMu.Lock()
	defer o.t.obsMu.Unlock()
	fn(o.t.ownerOf(parent))
}

func (o ownedObserver) SpawnStarted(parent string, s RunningSpawn, prompt string) {
	o.owned(parent, func(owner string) { o.inner.SpawnStarted(owner, s, prompt) })
}

func (o ownedObserver) SpawnText(parent, id, text string) {
	o.owned(parent, func(owner string) { o.inner.SpawnText(owner, id, text) })
}

func (o ownedObserver) SpawnTool(parent, id, line string) {
	o.owned(parent, func(owner string) { o.inner.SpawnTool(owner, id, line) })
}

func (o ownedObserver) SpawnPrompt(parent, id, message string) {
	o.owned(parent, func(owner string) { o.inner.SpawnPrompt(owner, id, message) })
}

func (o ownedObserver) SpawnModel(parent, id, model string) {
	o.owned(parent, func(owner string) { o.inner.SpawnModel(owner, id, model) })
}

func (o ownedObserver) SpawnRetry(parent, id string, untilMs int64, status int) {
	o.owned(parent, func(owner string) { o.inner.SpawnRetry(owner, id, untilMs, status) })
}

func (o ownedObserver) SpawnEnded(parent, id string) {
	o.owned(parent, func(owner string) { o.inner.SpawnEnded(owner, id) })
}

// releaseHeld delivers every held spawn result of this agent whose owner may
// now take it.
func (s *Spawner) releaseHeld() {
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

// BranchOwner resolves the session that owns the spawn running on branchKey
// now (#2630): the session that started it, or, once that depth leaf has
// handed over, the node it passed to, by the same walk as the spawn's results
// (route). ok is false when branchKey is not one of this agent's spawns.
// While the route holds, the owner is still the reported leaf.
//
// A spawn run as this agent for another one (#2662) is owned by its caller:
// the walk is the caller's Spawner's, so its prompts go to the caller (Dick
// 2026-10-30), never to this agent's chats.
func (s *Spawner) BranchOwner(branchKey string) (owner string, ok bool) {
	if o, ok := s.directBatchOwner(branchKey); ok {
		return o, true
	}
	parent, info, found := s.deps.Tracker.ForBranch(branchKey)
	if !found {
		return "", false
	}
	if callerAgent := session.AgentIDFromKey(parent); callerAgent != s.deps.AgentID {
		if info.Agent != s.deps.AgentID {
			return "", false
		}
		if c := spawnerFor(callerAgent); c != nil {
			return c.route(parent).Owner, true
		}
		return parent, true
	}
	return s.route(parent).Owner, true
}

// spawners maps an agent id to its Spawner (RegisterSpawner), for callers
// that resolve a spawn's owner without the tool deps: the backend prompt
// routing of a delegated agent (#2632), built apart from its tools.
var (
	spawnersMu sync.RWMutex
	spawners   = map[string]*Spawner{}
)

// RegisterSpawner records agentID's Spawner for SpawnBranchOwner.
func RegisterSpawner(agentID string, s *Spawner) {
	spawnersMu.Lock()
	spawners[agentID] = s
	spawnersMu.Unlock()
}

// UnregisterSpawner forgets agentID's Spawner (the live stop of one agent,
// #2787).
func UnregisterSpawner(agentID string) {
	spawnersMu.Lock()
	delete(spawners, agentID)
	spawnersMu.Unlock()
}

// SpawnBranchOwner is BranchOwner on the registered Spawner of branchKey's
// agent; ok is false when the agent has none or branchKey is not a spawn.
func SpawnBranchOwner(branchKey string) (owner string, ok bool) {
	spawnersMu.RLock()
	s := spawners[session.AgentIDFromAnyKey(branchKey)]
	spawnersMu.RUnlock()
	if s == nil {
		return "", false
	}
	return s.BranchOwner(branchKey)
}

// errNoSpawn is lookupForCaller's error for a reference that matches no spawn
// of the caller's.
var errNoSpawn = errors.New("no such spawn")

// lookupForCaller finds a spawn caller owns now (#3206: its own, unless it
// has passed up a depth tree, and those handed over to it), and the session
// it is keyed under. ref is its id or branch key, which win, or its name
// (#2721). A name that more than one of them carries (only possible across
// spawns handed over from different depth leaves) is refused with their ids;
// no match is errNoSpawn.
func (s *Spawner) lookupForCaller(caller, ref string, n int) (string, SpawnSnapshot, error) {
	if snap, ok := s.deps.Tracker.Lookup(caller, ref, n); ok {
		return snap.Parent, snap, nil
	}
	found := s.deps.Tracker.lookupName(caller, strings.TrimSpace(ref), n)
	switch len(found) {
	case 0:
		return "", SpawnSnapshot{}, errNoSpawn
	case 1:
		return found[0].Parent, found[0], nil
	}
	ids := make([]string, len(found))
	for i, f := range found {
		ids[i] = f.ID
	}
	return "", SpawnSnapshot{}, fmt.Errorf("more than one of your spawns is named %q (%s): give its id instead", strings.TrimSpace(ref), strings.Join(ids, ", "))
}

// claimSpawnName reserves name for a spawn caller is starting (#2721): it
// must not be the name of any spawn caller's spawn_status lists.
func (s *Spawner) claimSpawnName(caller, name string) (release func(), err error) {
	return s.deps.Tracker.claimName(caller, name)
}

// listForCaller lists the spawns caller owns now (#3206); a handed-over
// one's description says where it came from.
func (s *Spawner) listForCaller(caller string) []SpawnSnapshot {
	out := s.deps.Tracker.List(caller)
	for i, sp := range out {
		if sp.Parent != caller {
			out[i].Description = fmt.Sprintf("(from %s) %s", sp.Via, sp.Description)
		}
	}
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
		s.result, s.held, s.reporting = msg, hold, false
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

// --- the routes (#3206) ---

// setRouteResolver makes fn the resolver of agentID's routes: what
// routeOf and reroute ask for the owner of a session's spawns. The agent's
// Spawner registers its liveRoute (NewSpawner).
func (t *SpawnTracker) setRouteResolver(agentID string, fn func(parent string) SpawnRoute) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolvers == nil {
		t.resolvers = map[string]func(string) SpawnRoute{}
	}
	t.resolvers[agentID] = fn
}

// routeOf is the route of parent's spawns: the one the tracker holds, or,
// for a session it holds none for yet, the resolver's, which it then keeps.
// A session of an agent with no resolver owns its spawns.
func (t *SpawnTracker) routeOf(parent string) SpawnRoute {
	if t == nil {
		return SpawnRoute{Owner: parent}
	}
	t.mu.Lock()
	r, ok := t.routes[parent]
	fn := t.resolvers[session.AgentIDFromKey(parent)]
	t.mu.Unlock()
	if ok {
		return r
	}
	if fn == nil {
		return SpawnRoute{Owner: parent}
	}
	// Resolved without t.mu: the resolver takes the depth manager's lock.
	r = normRoute(parent, fn(parent))
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.routes[parent]; ok {
		return cur // a reroute in between has the fresher one
	}
	if t.routes == nil {
		t.routes = map[string]SpawnRoute{}
	}
	t.routes[parent] = r
	return r
}

// ownerOf is the session that owns parent's spawns now, as the tracker holds
// it.
func (t *SpawnTracker) ownerOf(parent string) string {
	if t == nil {
		return parent
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ownerLocked(parent)
}

// ownerLocked is the session that owns parent's spawns now. Caller holds t.mu.
func (t *SpawnTracker) ownerLocked(parent string) string {
	if r, ok := t.routes[parent]; ok {
		return r.Owner
	}
	return parent
}

// movedSpawn is a running spawn whose owner a reroute changed.
type movedSpawn struct {
	parent   string
	from, to SpawnRoute
	info     RunningSpawn
	prompt   string
}

// reroute asks agentID's resolver again for the route of every session whose
// spawns the tracker holds, keeps the answers, forgets the routes of
// sessions with no spawn left, and returns the running spawns whose owner
// changed, oldest first.
func (t *SpawnTracker) reroute(agentID string) []movedSpawn {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	fn := t.resolvers[agentID]
	var parents []string
	for p, m := range t.spawns {
		if len(m) > 0 && session.AgentIDFromKey(p) == agentID {
			parents = append(parents, p)
		}
	}
	for p := range t.routes {
		if len(t.spawns[p]) == 0 && session.AgentIDFromKey(p) == agentID {
			delete(t.routes, p)
		}
	}
	t.mu.Unlock()
	if fn == nil {
		return nil
	}
	fresh := make(map[string]SpawnRoute, len(parents))
	for _, p := range parents {
		fresh[p] = normRoute(p, fn(p))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.routes == nil {
		t.routes = map[string]SpawnRoute{}
	}
	var moved []movedSpawn
	for p, r := range fresh {
		old, ok := t.routes[p]
		if !ok {
			old = SpawnRoute{Owner: p}
		}
		t.routes[p] = r
		if old.Owner == r.Owner {
			continue
		}
		for _, s := range t.spawns[p] {
			if s.running {
				moved = append(moved, movedSpawn{parent: p, from: old, to: r, info: s.info, prompt: s.prompt})
			}
		}
	}
	sortSpawns(moved, func(m movedSpawn) RunningSpawn { return m.info })
	return moved
}
