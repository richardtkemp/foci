package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"foci/internal/session"
	"foci/internal/turnevent"
)

// RunningSpawn is one clone-mode spawn running in the background (#2139). The
// app lists it beside the agent's subagents, opens its progress view, and stops
// it with them.
type RunningSpawn struct {
	// ID is the spawn's subagent group key: the app's chit for it is
	// "subagent:<ID>". It holds no colon, which the client's chit ids reserve.
	ID        string
	BranchKey string
	// Name is the spawn's name, given by the spawn call (#2721): required,
	// trimmed, and unique among the spawns spawn_status lists in its
	// session. It is the label the user sees (Label) and a second handle for
	// spawn_send, spawn_stop and spawn_status. "" only for a spawn persisted
	// before names existed.
	Name        string
	Description string // the prompt's opening: spawn_status's Task line
	Model       string // the branch session's model; empty = unknown
	Started     time.Time
	// Character marks a background character spawn (#2643): a fresh branch
	// with the agent's own prompt, not a fork of the parent's conversation.
	// Its branch is re-prepared (SpawnDeps.PrepareCharacterBranch) before a
	// run after a restart.
	Character bool
	// SpawnModel is the model the caller asked for ("" = the default), kept
	// so a restart can prepare a character branch on it again.
	SpawnModel string
	// Direct marks a spawn that returns its result straight to the tool call
	// (raw, explore, character without background): tracked only while that
	// call waits, so spawn_status lists it and spawn_stop or /stop can end it
	// (#2656). It has no branch the caller can reach, takes no spawn_send,
	// and is never persisted: after a restart nobody waits for its result.
	Direct bool
	// Mode is the spawn's context ("character", "clone", ...), for listing.
	Mode string
	// Agent is the agent the spawn runs as when that is not the caller's own
	// (#2662, a cross-agent character spawn); "" = the caller's agent. Its
	// branch is that agent's session, run by that agent's Spawner.
	Agent string
	// CallerShell is, for a direct spawn started over the exec bridge, the
	// command lines of the shell processes its call came from (callerShell):
	// the app hides the background-command entry of the shell call that
	// matches it (ShellCallMatch) while the spawn runs, so the spawn shows
	// once (#2685). "" = not known.
	CallerShell string
	// SpawnerUnattended records that the turn which started the spawn had
	// nobody to answer a permission prompt (its TurnEvents were unattended:
	// keepalive, reflection, a webhook, ...; SpawnDeps.TurnUnattended). Such
	// a spawn runs unattended too, as before #2684; any other spawn's asks go
	// up to its spawner's chat. Captured at launch and kept across a restart.
	SpawnerUnattended bool
	// Depth is the spawn's depth (#3061, spawn_depth.go): 1 for a spawn of a
	// session no spawn started, N+1 for a spawn of a depth-N spawn. The spawn
	// may start a spawn only while Depth+1 <= the spawn depth limit. Kept
	// across a restart.
	Depth int
}

// SpawnObserver is told about each spawn of a parent session, background or
// direct (#2683), so a platform can show it beside the agent's subagents
// (#2139). Every method may
// be called from the spawn's goroutine.
type SpawnObserver interface {
	// SpawnStarted reports a spawn started for parentSession with its prompt.
	SpawnStarted(parentSession string, s RunningSpawn, prompt string)
	// SpawnText reports one block of the spawn's assistant text.
	SpawnText(parentSession, id, text string)
	// SpawnTool reports one tool call of the spawn, as one line (the tool name
	// and its input), for the app's Stop-view summary (#2960).
	SpawnTool(parentSession, id, line string)
	// SpawnPrompt reports a parent's spawn_send message to a spawn that is
	// running, folded into its turn or queued for its next step (#2822). A
	// message that resumes a finished spawn comes as SpawnStarted instead.
	SpawnPrompt(parentSession, id, message string)
	// SpawnModel reports the model a running spawn runs on, when it became known
	// or changed after SpawnStarted (#2879).
	SpawnModel(parentSession, id, model string)
	// SpawnRetry reports the spawn's backend is waiting to retry a failed API
	// call (a rate limit, an overload), with the time the next try is due
	// (unix ms) and its HTTP status (0 = none), or that the wait is over
	// (untilMs 0) (#3146).
	SpawnRetry(parentSession, id string, untilMs int64, status int)
	// SpawnEnded reports the spawn finished, failed or was stopped.
	SpawnEnded(parentSession, id string)
}

// SpawnTracker holds every background spawn, keyed by parent session, so /stop
// can stop a session's spawns on their own (#2139), the statusline can list
// them (#2127), and the parent can steer a spawn and check on it (#2523). A
// finished spawn stays known (its last maxFinishedSpawns per parent) so a
// follow-up message can resume its branch.
//
// With a store (SetStore), every spawn that is running or whose result has not
// reached its parent yet is also kept in state.db, so a foci restart resumes it
// (#2589, Spawner.RestoreSpawns).
type SpawnTracker struct {
	mu     sync.Mutex
	spawns map[string]map[string]*trackedSpawn // parent session → spawn id → spawn

	// store keeps the persisted records; nil = in memory only.
	store SpawnStore
	// shuttingDown is set by BeginShutdown: a run cut short from then on was
	// cut by the shutdown, and is resumed by the next process instead of
	// being reported failed.
	shuttingDown bool
	// stopping holds the agents whose live stop has begun (BeginAgentStop,
	// #2787): a run of theirs cut short is treated like one cut by a
	// shutdown, for that agent's next start to resume.
	stopping map[string]bool
	// starting holds, per caller session, the names its spawn calls have
	// claimed (claimName) and not yet registered a spawn under, so two calls
	// at once cannot take one name (#2721).
	starting map[string]map[string]bool
	// persistMu orders the writes of persist: each write snapshots the
	// spawn's state under it, so the last write always holds the latest state.
	persistMu sync.Mutex

	// routes holds, per session that started spawns, who owns them now
	// (#3206, spawn_reparent.go): every session-keyed query, delivery and
	// app event reads it. A session with no entry owns its own spawns.
	routes map[string]SpawnRoute
	// resolvers computes an agent's routes (setRouteResolver), by agent id.
	resolvers map[string]func(parent string) SpawnRoute
	// obsMu orders the app events of spawns (ownedObserver) with the moves
	// of Spawner.Reown, so none is shown in a chat the spawn has left.
	obsMu sync.Mutex
}

type trackedSpawn struct {
	info    RunningSpawn
	cancel  context.CancelFunc
	stopped bool
	// stoppedBy says who stopped it, for its result: "the user" (/stop) or
	// its parent agent (spawn_stop).
	stoppedBy string
	running   bool
	// queued: a background spawn started while every spawn slot was taken
	// ([tools] max_concurrent_spawns): it is running to its parent, but its
	// run waits for a slot (#3235): its first run, or a run spawn_send
	// resumes it for (#3244). queuedAt: when it began to wait. In memory only.
	queued   bool
	queuedAt time.Time
	ended    time.Time // when its last run ended; zero while running
	outcome  string    // last run's end: "completed", "failed" or "stopped"
	pending  []string  // parent messages waiting for the spawn's next step
	log      []SpawnLogEntry
	prompt   string // the task it was started with, repeated when a restart resumes it
	resumes  int    // how many times a foci restart has resumed it
	// result is the [SPAWN RESULT] message on its way to the parent: kept,
	// and persisted, until the parent's turn has taken it.
	result string
	// held: result waits for a depth fan-in before it may go to the spawn's
	// owner (#2624, Spawner.Reown). In memory only: a restart
	// re-routes the persisted result, which holds it again if it must.
	held bool
	// kept holds the spawn's messages to its spawner (send_to_spawner) that
	// could not go at once: its spawner's call is waiting on it, or a depth
	// fan-in holds its owner. They head its result. Persisted with the
	// spawn (#2677). A direct spawn is never persisted, nor are its kept
	// messages, except an API one-shot (oneShot, #2979).
	kept []SpawnLogEntry
	// oneShot is set on an API agent's direct one-shot (#2979): a direct
	// spawn that is persisted, so a restart resumes it.
	oneShot *oneShotRecord
	// adrift are the runs on the spawn's branch that began while the spawn
	// was already running (spawnAutonomousSink, not owned): its backend woke
	// by itself, or an injection ran there. When the spawn's run ends while
	// one is still going, the oldest takes the spawn over and reports its
	// result (#3143). In memory only.
	adrift []*spawnAutonomousSink
	// adriftAnswer is the answer of the latest adrift run that ended while
	// the spawn ran, for a result that would otherwise not carry it (a paused
	// spawn gone cold, #3143). Cleared when the spawn's run ends.
	adriftAnswer string
	// wake holds what its own spawns sent it after its run ended, for the
	// run that resumes it (#3163, wakeSpawn), in arrival order. Persisted
	// (spawnRecord.Wake, Dick 2026-11-18: "Persist the queue"). waking says
	// a resume is under way (in memory only).
	wake   []childReport
	waking bool
	// reporting: its run has ended and its result is not recorded yet
	// (finishOrNext to pushResult); a persist from one of its own spawns
	// then leaves its row as it is (#3163).
	reporting bool
	// awaiting is set while its run has ended but work it started in the
	// background still runs (#2728): it stays running, and its result
	// waits for that work to end (spawnAwait). In memory only: a restart
	// resumes the running record.
	awaiting *spawnAwait
}

// SpawnLogEntry is one step of a spawn's activity, kept for spawn_status
// (#2523): the input it was given, its assistant text, its tool calls, and the
// end of each run.
type SpawnLogEntry struct {
	At   time.Time
	Kind string // "prompt", "message", "text", "tool", "result", "error"
	Text string
}

// SpawnSnapshot is a copy of one tracked spawn's state.
type SpawnSnapshot struct {
	RunningSpawn
	// Parent is the session that started the spawn: the key it is tracked
	// and acted on under, whoever owns it now (#3206).
	Parent string
	// Via names Parent, when the spawn was handed over to the session it
	// was listed for (SpawnRoute.Via); "" otherwise.
	Via     string
	Running bool
	// Queued: running, but its run waits for a free spawn slot: its first
	// (#3235), or one spawn_send resumes it for (#3244). QueuedSince: when
	// it began to wait.
	Queued      bool
	QueuedSince time.Time
	Outcome     string // "" while running
	Ended       time.Time
	Pending     int // parent messages not yet delivered
	Resumes     int // times a foci restart has resumed it
	// Awaiting names the background work a running spawn waits on: its
	// run has ended, and its result waits for that work (#2728). "" when
	// its run is going (or it is finished).
	Awaiting string
	Log      []SpawnLogEntry
}

const (
	// maxFinishedSpawns is how many finished spawns per parent stay known
	// for spawn_status and follow-ups.
	maxFinishedSpawns = 20
	// spawnLogCap bounds each spawn's activity log; the oldest go first.
	spawnLogCap = 200
	// spawnLogEntryMax bounds one log entry's text, in runes.
	spawnLogEntryMax = 4000
)

// Spawns is the process-wide tracker the spawn tool registers with and /stop
// stops through. Session keys carry the agent id, so one tracker serves every
// agent.
var Spawns = NewSpawnTracker()

// NewSpawnTracker returns an empty tracker.
func NewSpawnTracker() *SpawnTracker {
	return &SpawnTracker{spawns: map[string]map[string]*trackedSpawn{}}
}

// Add registers a running spawn of parent; its goroutine removes it when it ends.
func (t *SpawnTracker) Add(parent string, s RunningSpawn, cancel context.CancelFunc) {
	t.add(parent, s, cancel)
}

func (t *SpawnTracker) add(parent string, s RunningSpawn, cancel context.CancelFunc) {
	t.addWithPrompt(parent, s, cancel, "")
}

// addWithPrompt registers a running spawn with the task it was given, and
// persists it so a restart can resume it.
func (t *SpawnTracker) addWithPrompt(parent string, s RunningSpawn, cancel context.CancelFunc, prompt string) {
	if t == nil {
		return
	}
	t.routeOf(parent) // its owner is known before anything reads it (#3206)
	t.mu.Lock()
	if t.spawns[parent] == nil {
		t.spawns[parent] = map[string]*trackedSpawn{}
	}
	t.spawns[parent][s.ID] = &trackedSpawn{info: s, cancel: cancel, running: true, prompt: prompt}
	t.mu.Unlock()
	t.persist(parent, s.ID)
}

// ownedLocked calls fn with each spawn owner owns now (#3206): those it
// started and kept, and those handed over to it, with the session that
// started each. Caller holds t.mu.
func (t *SpawnTracker) ownedLocked(owner string, fn func(parent string, s *trackedSpawn)) {
	for p, m := range t.spawns {
		if len(m) == 0 || t.ownerLocked(p) != owner {
			continue
		}
		for _, s := range m {
			fn(p, s)
		}
	}
}

// ownedSnapshot is s's snapshot as listed for its owner: with the session
// that started it, and that session's name when it is another one.
func (t *SpawnTracker) ownedSnapshot(owner, parent string, s *trackedSpawn, n int) SpawnSnapshot {
	snap := s.snapshot(n)
	snap.Parent = parent
	if parent != owner {
		snap.Via = t.routes[parent].Via
	}
	return snap
}

// Running returns the running spawns session owns now (#3206), oldest first
// (nil when there are none): the ones it started, unless they have passed
// to another depth node, and the ones handed over to it.
func (t *SpawnTracker) Running(session string) []RunningSpawn {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []RunningSpawn
	t.ownedLocked(session, func(_ string, s *trackedSpawn) {
		if s.running {
			out = append(out, s.info)
		}
	})
	sortSpawns(out, func(r RunningSpawn) RunningSpawn { return r })
	return out
}

// List returns every known spawn session owns now (#3206), running and
// finished, oldest first, without their logs.
func (t *SpawnTracker) List(session string) []SpawnSnapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []SpawnSnapshot
	t.ownedLocked(session, func(p string, s *trackedSpawn) {
		out = append(out, t.ownedSnapshot(session, p, s, 0))
	})
	sortSpawns(out, func(s SpawnSnapshot) RunningSpawn { return s.RunningSpawn })
	return out
}

func sortSpawns[T any](s []T, info func(T) RunningSpawn) {
	slices.SortFunc(s, func(x, y T) int {
		a, b := info(x), info(y)
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Lookup finds a spawn session owns now (#3206) by its id or its branch key,
// with its last n log entries (all when n <= 0). The snapshot's Parent is
// the session it is keyed under.
func (t *SpawnTracker) Lookup(session, idOrBranch string, n int) (SpawnSnapshot, bool) {
	if t == nil || idOrBranch == "" {
		return SpawnSnapshot{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var byID, byBranch *trackedSpawn
	var idParent, branchParent string
	t.ownedLocked(session, func(p string, s *trackedSpawn) {
		switch {
		case s.info.ID == idOrBranch:
			byID, idParent = s, p
		case s.info.BranchKey == idOrBranch && byBranch == nil:
			byBranch, branchParent = s, p
		}
	})
	s, p := byID, idParent
	if s == nil {
		s, p = byBranch, branchParent
	}
	if s == nil {
		return SpawnSnapshot{}, false
	}
	snap := t.ownedSnapshot(session, p, s, n)
	if n <= 0 {
		snap.Log = slices.Clone(s.log)
	}
	return snap, true
}

func (t *SpawnTracker) find(parent, idOrBranch string) *trackedSpawn {
	if idOrBranch == "" {
		return nil
	}
	if s := t.spawns[parent][idOrBranch]; s != nil {
		return s
	}
	for _, s := range t.spawns[parent] {
		if s.info.BranchKey == idOrBranch {
			return s
		}
	}
	return nil
}

// lookupName finds the spawns session owns now named name (#2721, #3206),
// with their last n log entries (all when n <= 0), by id. A spawn with no
// name never matches. More than one is possible only across spawns handed
// over from different depth leaves.
func (t *SpawnTracker) lookupName(session, name string, n int) []SpawnSnapshot {
	if t == nil || name == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []SpawnSnapshot
	t.ownedLocked(session, func(p string, s *trackedSpawn) {
		if s.info.Name != name {
			return
		}
		snap := t.ownedSnapshot(session, p, s, n)
		if n <= 0 {
			snap.Log = slices.Clone(s.log)
		}
		out = append(out, snap)
	})
	slices.SortFunc(out, func(a, b SpawnSnapshot) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// claimName reserves name for a spawn caller is starting (#2721). It fails
// when a spawn caller owns now (the ones its spawn_status lists, #3206) is
// named name, or another of caller's spawn calls is starting under it.
// release frees the reservation; call it once the spawn is registered, or
// did not start.
func (t *SpawnTracker) claimName(caller, name string) (release func(), err error) {
	if t == nil {
		return func() {}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var taken *trackedSpawn
	t.ownedLocked(caller, func(_ string, s *trackedSpawn) {
		if s.info.Name == name {
			taken = s
		}
	})
	if taken != nil {
		return nil, fmt.Errorf("this session already has a spawn named %q (%s); give this one another name (a finished spawn keeps its name until it is no longer listed by spawn_status)", name, taken.info.ID)
	}
	if t.starting[caller][name] {
		return nil, fmt.Errorf("another spawn named %q is starting in this session; give this one another name", name)
	}
	if t.starting == nil {
		t.starting = map[string]map[string]bool{}
	}
	if t.starting[caller] == nil {
		t.starting[caller] = map[string]bool{}
	}
	t.starting[caller][name] = true
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.starting[caller], name)
		if len(t.starting[caller]) == 0 {
			delete(t.starting, caller)
		}
	}, nil
}

// spawnInfo is parent's spawn id as it was started, whether or not it runs.
func (t *SpawnTracker) spawnInfo(parent, id string) (RunningSpawn, bool) {
	if t == nil {
		return RunningSpawn{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		return s.info, true
	}
	return RunningSpawn{}, false
}

func (s *trackedSpawn) snapshot(n int) SpawnSnapshot {
	snap := SpawnSnapshot{
		RunningSpawn: s.info, Running: s.running, Queued: s.queued, QueuedSince: s.queuedAt, Ended: s.ended,
		Pending: len(s.pending), Resumes: s.resumes,
	}
	if !s.running {
		snap.Outcome = s.outcome
	}
	if s.awaiting != nil {
		snap.Awaiting = s.awaiting.work
	}
	if n > 0 {
		snap.Log = slices.Clone(s.log[max(0, len(s.log)-n):])
	}
	return snap
}

// ForBranch reports the spawn whose branch session is branchKey, and its
// parent: a spawn's own tool calls and permission prompts carry the branch
// key, not the spawn id.
func (t *SpawnTracker) ForBranch(branchKey string) (parent string, s RunningSpawn, ok bool) {
	if t == nil || branchKey == "" {
		return "", RunningSpawn{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for p, m := range t.spawns {
		for _, ts := range m {
			if ts.info.BranchKey == branchKey {
				return p, ts.info, true
			}
		}
	}
	return "", RunningSpawn{}, false
}

// queue holds text for a running spawn's next step and reports whether it
// did. A spawn that has ended (or is being stopped) takes no more input: the
// caller resumes it instead.
func (t *SpawnTracker) queue(parent, id, text string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	// A spawn waiting on its background work has no step to take the text
	// (#2728): the caller resumes it instead, which ends the wait.
	if s == nil || !s.running || s.stopped || s.awaiting != nil {
		t.mu.Unlock()
		return false
	}
	s.pending = append(s.pending, text)
	s.appendLog("message", "(queued) "+text)
	t.mu.Unlock()
	t.persist(parent, id)
	return true
}

// pushPending adds text to the messages waiting for a running spawn, and
// persists it, so a restart before its run takes the text keeps it. No log
// entry: the caller logs what the parent wrote. False when the spawn is gone.
func (t *SpawnTracker) pushPending(parent, id, text string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return false
	}
	s.pending = append(s.pending, text)
	t.mu.Unlock()
	t.persist(parent, id)
	return true
}

// drainPending takes the messages waiting for a spawn. The API turn calls it
// at each tool boundary (the spawn's turnevent.Steerer).
func (t *SpawnTracker) drainPending(parent, id string) []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || len(s.pending) == 0 {
		t.mu.Unlock()
		return nil
	}
	out := s.pending
	s.pending = nil
	t.mu.Unlock()
	t.persist(parent, id)
	return out
}

// runEnd is how finishOrNext ended a spawn's run.
type runEnd struct {
	// next is the input of the spawn's next turn when done is false.
	next []string
	// done: the run is over; stopped says /stop or spawn_stop ended it.
	done, stopped bool
	// interrupted: the shutdown cut it; the record stays for resume.
	interrupted bool
	// dropped is how many queued messages were never delivered.
	dropped int
	// handedOff: a run adrift on the spawn's branch took the spawn over
	// (#3143). The spawn stays running, and that run reports the result.
	handedOff *spawnAutonomousSink
	// awaiting: the run ended while work it started in the background
	// still runs (#2728). The spawn stays running and delivers no result
	// now; the caller starts the wait (Spawner.startAwait).
	awaiting *spawnAwait
}

// finishOrNext ends a spawn's run. When parent messages arrived that its turn
// did not take, and the run may go on (not stopped, not failed: force false),
// it returns them as the next turn's input instead, done false. Otherwise it
// marks the spawn finished and returns done true, whether /stop ended it, and
// how many queued messages were never delivered. One lock covers both, so a
// message queued as the run ends is never lost: queue fails once done.
//
// A run cut short (force) after BeginShutdown was cut by the shutdown: it
// returns done and interrupted true and leaves the spawn, and its persisted
// record, as they are, so the next process resumes it (#2589).
//
// A run that ends while a run adrift on the branch still goes (and the spawn
// was not stopped) hands the spawn over to that run (#3143): done and
// handedOff, the spawn still running, its queued messages kept for the
// follow-up of the run that took it over.
func (t *SpawnTracker) finishOrNext(parent, id string, force bool) runEnd {
	return t.finishRun(parent, id, force, "", "")
}

// finishRun is finishOrNext for a run that ended while work its backend
// started in the background may still run: work names it ("" none), and
// answer is the text the run ended with. A run that is not cut short (force
// false), not stopped, has no message waiting and no run adrift to hand
// over to, and leaves work running, does not finish the spawn (#2728, Dick
// 2026-11-21: "Keep it running"): it stays running with answer kept in its
// wait, done and awaiting set, and its result waits for that work to end.
func (t *SpawnTracker) finishRun(parent, id string, force bool, work, answer string) runEnd {
	if t == nil {
		return runEnd{done: true}
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return runEnd{done: true}
	}
	if force && (t.shuttingDown || t.stopping[session.AgentIDFromAnyKey(parent)]) && !s.stopped {
		t.mu.Unlock()
		return runEnd{done: true, interrupted: true}
	}
	if !force && !s.stopped && len(s.pending) > 0 {
		next := s.pending
		s.pending = nil
		t.mu.Unlock()
		t.persist(parent, id)
		return runEnd{next: next}
	}
	if r := s.handOffLocked(); r != nil {
		t.mu.Unlock()
		t.persist(parent, id)
		return runEnd{done: true, handedOff: r}
	}
	if work != "" && !force && !s.stopped {
		w := newSpawnAwait(answer, work)
		s.awaiting = w
		s.cancel = w.stop
		s.adriftAnswer = ""
		t.mu.Unlock()
		return runEnd{done: true, awaiting: w}
	}
	defer t.mu.Unlock()
	end := runEnd{done: true, stopped: s.stopped, dropped: len(s.pending)}
	s.pending = nil
	s.running = false
	s.reporting = true
	s.ended = time.Now()
	s.adriftAnswer = ""
	t.pruneFinished(parent)
	// Not persisted here: the record stays "running" until the result is
	// recorded (pushResult), so a crash in between resumes the spawn rather
	// than losing its result.
	return end
}

// setQueued marks parent's spawn id as waiting for a spawn slot, from now
// (#3235, #3244).
func (t *SpawnTracker) setQueued(parent, id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		s.queued, s.queuedAt = true, time.Now()
	}
}

// startQueued records that a queued spawn got its slot and starts its run
// now. For its first run (firstRun) its start time becomes now, so
// spawn_status's age is its run's; a resumed spawn keeps its start time, as
// one resumed at once does (#3244).
func (t *SpawnTracker) startQueued(parent, id string, firstRun bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return
	}
	s.queued, s.queuedAt = false, time.Time{}
	if firstRun {
		s.info.Started = time.Now()
	}
	t.mu.Unlock()
	t.persist(parent, id)
}

// setModel records the model a spawn runs on, reporting whether that changed it.
func (t *SpawnTracker) setModel(parent, id, model string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || s.info.Model == model {
		t.mu.Unlock()
		return false
	}
	s.info.Model = model
	t.mu.Unlock()
	t.persist(parent, id)
	return true
}

// setOutcome records how a spawn's last run ended, for spawn_status.
func (t *SpawnTracker) setOutcome(parent, id, outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		s.outcome = outcome
	}
}

// pruneFinished forgets the oldest finished spawns of parent beyond
// maxFinishedSpawns. Caller holds t.mu.
func (t *SpawnTracker) pruneFinished(parent string) {
	var done []*trackedSpawn
	for _, s := range t.spawns[parent] {
		// A finished spawn stays known while its own spawns run or hold a
		// result for it, or while reports wait to wake it (#3163).
		if !s.running && !s.waking && len(s.wake) == 0 && !t.liveChildrenLocked(s.info.BranchKey) {
			done = append(done, s)
		}
	}
	if len(done) <= maxFinishedSpawns {
		return
	}
	slices.SortFunc(done, func(a, b *trackedSpawn) int { return a.ended.Compare(b.ended) })
	for _, s := range done[:len(done)-maxFinishedSpawns] {
		delete(t.spawns[parent], s.info.ID)
	}
}

// restart marks a finished spawn running again for a follow-up run, with that
// run's cancel. False when it is unknown or already running. A spawn waiting
// on its background work (#2728, not being stopped) is taken over by the
// run instead: its wait ends, and fromWait is true (it is still shown
// running).
func (t *SpawnTracker) restart(parent, id string, cancel context.CancelFunc) (ok, fromWait bool) {
	if t == nil {
		return false, false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || (s.running && (s.awaiting == nil || s.stopped)) {
		t.mu.Unlock()
		return false, false
	}
	fromWait = s.takeAwaitLocked()
	s.running, s.stopped, s.cancel, s.reporting = true, false, cancel, false
	s.ended, s.outcome = time.Time{}, ""
	t.mu.Unlock()
	t.persist(parent, id)
	return true, fromWait
}

// appendLog adds one entry to a spawn's activity log.
func (t *SpawnTracker) appendLog(parent, id, kind, text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		s.appendLog(kind, text)
	}
}

// lastLogText is the text of the spawn's latest entry of kind, or "".
func (t *SpawnTracker) lastLogText(parent, id, kind string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return ""
	}
	for i := len(s.log) - 1; i >= 0; i-- {
		if s.log[i].Kind == kind {
			return s.log[i].Text
		}
	}
	return ""
}

func (s *trackedSpawn) appendLog(kind, text string) {
	if r := []rune(text); len(r) > spawnLogEntryMax {
		text = string(r[:spawnLogEntryMax]) + "…"
	}
	s.log = append(s.log, SpawnLogEntry{At: time.Now(), Kind: kind, Text: text})
	if len(s.log) > spawnLogCap {
		s.log = slices.Delete(s.log, 0, len(s.log)-spawnLogCap)
	}
}

// addDirect registers a direct spawn while its tool call waits for it
// (#2656). Not persisted, unless setOneShot marks it an API one-shot (#2979).
func (t *SpawnTracker) addDirect(parent string, s RunningSpawn, cancel context.CancelFunc, prompt string) {
	if t == nil {
		return
	}
	s.Direct = true
	t.routeOf(parent) // its owner is known before anything reads it (#3206)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spawns[parent] == nil {
		t.spawns[parent] = map[string]*trackedSpawn{}
	}
	ts := &trackedSpawn{info: s, cancel: cancel, running: true, prompt: prompt}
	ts.appendLog("prompt", prompt)
	t.spawns[parent][s.ID] = ts
}

// endDirect marks a direct spawn finished with outcome ("completed",
// "failed"; "stopped" when Stop or stopOne ended it, whatever outcome says)
// and returns who stopped it ("" = nobody) and the messages it kept for its
// caller (send_to_spawner), taken in the same step that ends it so none can
// arrive in between and be lost.
//
// An API one-shot's persisted record (#2979) is forgotten here, unless foci
// is shutting down and the run did not complete: then it stays, for the next
// process to resume it, and resumes is true: the resumed run delivers its
// result and kept messages (#3168: so a late report must not). Its
// conversation file stays either way (#2984).
func (t *SpawnTracker) endDirect(parent, id, outcome, detail string) (stoppedBy string, kept []SpawnLogEntry, resumes bool) {
	if t == nil {
		return "", nil, false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return "", nil, false
	}
	if s.oneShot != nil {
		resumes = outcome != "completed" && !s.stopped &&
			(t.shuttingDown || t.stopping[session.AgentIDFromAnyKey(parent)])
		if !resumes {
			defer t.persist(parent, id)
		}
	}
	defer t.mu.Unlock()
	s.running = false
	kept, s.kept = s.kept, nil
	s.ended = time.Now()
	if s.stopped {
		outcome = "stopped"
	}
	s.outcome = outcome
	if detail != "" {
		kind := "result"
		if outcome != "completed" {
			kind = "error"
		}
		s.appendLog(kind, detail)
	}
	t.pruneFinished(parent)
	if !s.stopped {
		return "", kept, resumes
	}
	if s.stoppedBy == "" {
		return "the user", kept, resumes
	}
	return s.stoppedBy, kept, resumes
}

// stoppedBySpawnStop is stoppedBy for spawn_stop.
const stoppedBySpawnStop = "its parent agent (spawn_stop)"

// stopOne cancels parent's running spawn idOrBranch for spawn_stop. It
// reports whether the spawn is known, and whether it was running (and is now
// being stopped).
func (t *SpawnTracker) stopOne(parent, idOrBranch string) (known, stopped bool) {
	if t == nil {
		return false, false
	}
	t.mu.Lock()
	s := t.find(parent, idOrBranch)
	if s == nil {
		t.mu.Unlock()
		return false, false
	}
	if !s.running || s.stopped {
		t.mu.Unlock()
		return true, false
	}
	s.stopped, s.stoppedBy = true, stoppedBySpawnStop
	s.cancel()
	id := s.info.ID
	t.mu.Unlock()
	t.persistStopped([]spawnRef{{parent, id}})
	return true, true
}

// spawnRef names one tracked spawn: its parent session and its id.
type spawnRef struct{ parent, id string }

// persistStopped persists spawns just marked stopped, so a restart before
// their runs end reports them stopped instead of resuming them (#3226).
// Caller does not hold t.mu.
//
// It skips a spawn whose run has ended meanwhile and whose result is not
// recorded yet (persistOne's upward rule): its row stays "running" until
// pushResult writes it, so a crash in between loses nothing (finishOrNext).
// Nothing above it needs rewriting: a stop does not change whether the spawn
// is live for its parent spawn's row (#3163); its run's end does, and
// persists then.
func (t *SpawnTracker) persistStopped(refs []spawnRef) {
	for _, r := range refs {
		_, _, _ = t.persistOne(r.parent, r.id, true)
	}
}

// StopOne cancels the one running spawn with this id that session owns now
// (#3206), for the user's per-item stop (#2901), and reports whether it was
// running.
func (t *SpawnTracker) StopOne(session, id string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	var found *trackedSpawn
	var parent string
	t.ownedLocked(session, func(p string, s *trackedSpawn) {
		if s.info.ID == id {
			found, parent = s, p
		}
	})
	if found == nil || !found.running || found.stopped {
		t.mu.Unlock()
		return false
	}
	found.stopped, found.stoppedBy = true, "the user"
	found.cancel()
	t.mu.Unlock()
	t.persistStopped([]spawnRef{{parent, id}})
	return true
}

// Stop cancels every running spawn session owns now (#3206) and returns how
// many it stopped. Each spawn's own goroutine then ends it, reporting it
// stopped.
func (t *SpawnTracker) Stop(session string) int {
	return t.stopAll(session, "the user")
}

// stopAll is Stop, recording who stopped the spawns.
func (t *SpawnTracker) stopAll(session, by string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	var refs []spawnRef
	t.ownedLocked(session, func(p string, s *trackedSpawn) {
		if !s.running || s.stopped {
			return
		}
		s.stopped, s.stoppedBy = true, by
		s.cancel()
		refs = append(refs, spawnRef{p, s.info.ID})
	})
	t.mu.Unlock()
	t.persistStopped(refs)
	return len(refs)
}

// stopper says who stopped a spawn ("the user" when unknown).
func (t *SpawnTracker) stopper(parent, id string) string {
	if t == nil {
		return "the user"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil && s.stoppedBy != "" {
		return s.stoppedBy
	}
	return "the user"
}

// UnnamedSpawnLabel stands in for the name of a spawn that has none (one
// persisted before #2721 and restored after it), wherever a name is shown.
const UnnamedSpawnLabel = "(unnamed spawn)"

// Label is the spawn's name on one line (whitespace collapsed), or
// UnnamedSpawnLabel when it has none: what the user and the agent see it as.
func (r RunningSpawn) Label() string {
	if l := oneLine(r.Name); l != "" {
		return l
	}
	return UnnamedSpawnLabel
}

// oneLine collapses s's whitespace runs, line breaks included, to one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// spawnDescription is a spawn's prompt opening on one line, whitespace
// collapsed: spawn_status's Task line for it.
func spawnDescription(prompt string) string {
	r := []rune(oneLine(prompt))
	if len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return string(r)
}

// newSpawnID returns a fresh colon-free spawn group key.
func newSpawnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "spawn-" + hex.EncodeToString(b[:])
}

// syntheticModel is Claude Code's name for a result served without an API call
// (agent.SyntheticModel; this package cannot import agent).
const syntheticModel = "<synthetic>"

// spawnTextSink forwards a spawn's assistant text to the observer, and its
// text and tool calls to the tracker's activity log, while the inner
// BufferSink collects its final text. The API turn emits only the text between
// tool calls as TextBlocks; the answer itself arrives on TurnComplete. Either
// of obs and tracker may be nil.
type spawnTextSink struct {
	*turnevent.BufferSink
	obs        SpawnObserver
	tracker    *SpawnTracker
	parent, id string
	// sawText is set once any assistant text has passed through.
	sawText atomic.Bool
	// mu guards blocks: the text blocks forwarded so far, in order.
	mu     sync.Mutex
	blocks []string
	// retrying is set while the observer shows a retry wait (#3146).
	retrying atomic.Bool
	// detached is set when the spawn was marked ended while its run went on
	// (awaitDirectRun, #3166): the run's later events reach only its buffer.
	detached atomic.Bool
}

// noteRetry tells the observer the backend waits to retry until now + delay
// (#3146): the app shows the spawn as waiting, not as thinking. The time is
// rounded to the second: Claude Code repeats a long wait's api_retry every
// 30 s with the delay shortened by about 30 s, and an unrounded time would
// differ by a few ms each time and re-send the app's activity frame.
func (s *spawnTextSink) noteRetry(e turnevent.BackendRetry) {
	s.retrying.Store(true)
	if s.obs != nil {
		s.obs.SpawnRetry(s.parent, s.id, time.Now().Add(e.Delay).Round(time.Second).UnixMilli(), e.Status)
	}
}

// clearRetry ends a shown retry wait: the backend produced output again, or
// the turn ended.
func (s *spawnTextSink) clearRetry() {
	if s.retrying.CompareAndSwap(true, false) && s.obs != nil {
		s.obs.SpawnRetry(s.parent, s.id, 0, 0)
	}
}

// noteModel records the model the spawn's backend reports and, on a change,
// updates the spawn's entry in the app. A spawn run as an agent with no configured
// model has none when it starts (#2879). A result served with no API call names a
// sentinel, not a model.
func (s *spawnTextSink) noteModel(model string) {
	if model == "" || model == syntheticModel || !s.tracker.setModel(s.parent, s.id, model) {
		return
	}
	if s.obs != nil {
		s.obs.SpawnModel(s.parent, s.id, model)
	}
}

func (s *spawnTextSink) Emit(ctx context.Context, ev turnevent.Event) {
	if s.detached.Load() {
		s.BufferSink.Emit(ctx, ev)
		return
	}
	switch ev.(type) {
	case turnevent.TextBlock, turnevent.TextDelta, turnevent.ThinkingDelta, turnevent.ToolCall, turnevent.TurnComplete:
		s.clearRetry()
	}
	switch e := ev.(type) {
	case turnevent.BackendRetry:
		s.noteRetry(e)
	case turnevent.ModelKnown:
		s.noteModel(e.Model)
	case turnevent.TextBlock:
		if e.Text != "" {
			s.sawText.Store(true)
			s.mu.Lock()
			s.blocks = append(s.blocks, e.Text)
			s.mu.Unlock()
			if s.obs != nil {
				s.obs.SpawnText(s.parent, s.id, e.Text)
			}
			s.tracker.appendLog(s.parent, s.id, "text", e.Text)
		}
	case turnevent.ToolCall:
		line := toolCallSummary(e)
		if s.obs != nil {
			s.obs.SpawnTool(s.parent, s.id, line)
		}
		s.tracker.appendLog(s.parent, s.id, "tool", line)
	case turnevent.TurnComplete:
		s.noteModel(e.Model)
		s.mu.Lock()
		rest := unstreamedText(e.FinalText, s.blocks)
		s.mu.Unlock()
		if rest != "" {
			s.sawText.Store(true)
			if s.obs != nil {
				s.obs.SpawnText(s.parent, s.id, rest)
			}
			// A backend that already sent the answer as a text block
			// (Claude Code) is not logged twice.
			if s.tracker.lastLogText(s.parent, s.id, "text") != rest {
				s.tracker.appendLog(s.parent, s.id, "text", rest)
			}
		}
	}
	s.BufferSink.Emit(ctx, ev)
}

// unstreamedText is the part of a turn's FinalText that its text blocks did
// not already show. A delegated backend (Claude Code, codex) streams every
// assistant message as a block and then reports FinalText as all of them
// joined, with a blank line between messages and nothing between the blocks of
// one message; codex can append an error after them. So when FinalText starts
// with the blocks in order, separated only by whitespace, only what follows
// them is new (#2598). A FinalText that is just the last block repeats it.
// Otherwise (the API path, whose answer arrives only here) FinalText is all
// new.
func unstreamedText(final string, blocks []string) string {
	if len(blocks) == 0 {
		return final
	}
	if strings.TrimSpace(final) == strings.TrimSpace(blocks[len(blocks)-1]) {
		return ""
	}
	rest := final
	for _, b := range blocks {
		b = strings.TrimSpace(b)
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		if !strings.HasPrefix(rest, b) {
			return final
		}
		rest = rest[len(b):]
	}
	return strings.TrimSpace(rest)
}

// toolCallSummary is a tool call as one log line: its name and its arguments,
// cut short.
func toolCallSummary(e turnevent.ToolCall) string {
	args := strings.Join(strings.Fields(string(e.Args)), " ")
	if r := []rune(args); len(r) > 300 {
		args = string(r[:300]) + "…"
	}
	if args == "" {
		return e.Name
	}
	return e.Name + " " + args
}

// runningSpawn reports whether parent's spawn id is running, and its info.
func (t *SpawnTracker) runningSpawn(parent, id string) (RunningSpawn, bool) {
	if t == nil {
		return RunningSpawn{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || !s.running {
		return RunningSpawn{}, false
	}
	return s.info, true
}

// keepMessage keeps a message the running spawn sent its spawner for the
// head of its result, and reports whether it did (false: the spawn is not
// running).
func (t *SpawnTracker) keepMessage(parent, id, text string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || !s.running {
		t.mu.Unlock()
		return false
	}
	s.kept = append(s.kept, SpawnLogEntry{At: time.Now(), Kind: "to-spawner", Text: text})
	s.appendLog("to-spawner", "(held for the result) "+text)
	t.mu.Unlock()
	// Saved with the spawn, so a restart resumes it with them (#2677).
	t.persist(parent, id)
	return true
}

// takeKept returns, and forgets, the messages kept for the head of the
// spawn's result. It does not persist: the result they head is persisted
// next (pushResult), and that write drops them from the record, so a crash in
// between still has them in the record that resumes the spawn.
func (t *SpawnTracker) takeKept(parent, id string) []SpawnLogEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return nil
	}
	out := s.kept
	s.kept = nil
	return out
}
