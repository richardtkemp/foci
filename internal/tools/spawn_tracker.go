package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/turnevent"
)

// RunningSpawn is one clone-mode spawn running in the background (#2139). The
// app lists it beside the agent's subagents, opens its progress view, and stops
// it with them.
type RunningSpawn struct {
	// ID is the spawn's subagent group key: the app's chit for it is
	// "subagent:<ID>". It holds no colon, which the client's chit ids reserve.
	ID          string
	BranchKey   string
	Description string // the prompt's opening, as the agent's own subagent descriptions are
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
	// persistMu orders the writes of persist: each write snapshots the
	// spawn's state under it, so the last write always holds the latest state.
	persistMu sync.Mutex
}

type trackedSpawn struct {
	info    RunningSpawn
	cancel  context.CancelFunc
	stopped bool
	// stoppedBy says who stopped it, for its result: "the user" (/stop) or
	// its parent agent (spawn_stop).
	stoppedBy string
	running   bool
	timeout   time.Duration // of its first run: a resumed run gets it again
	ended     time.Time     // when its last run ended; zero while running
	outcome   string        // last run's end: "completed", "failed" or "stopped"
	pending   []string      // parent messages waiting for the spawn's next step
	log       []SpawnLogEntry
	prompt    string // the task it was started with, repeated when a restart resumes it
	resumes   int    // how many times a foci restart has resumed it
	// result is the [SPAWN RESULT] message on its way to the parent: kept,
	// and persisted, until the parent's turn has taken it.
	result string
	// held: result waits for a depth fan-in before it may go to the spawn's
	// owner (#2624, Spawner.ReleaseHeld). In memory only: a restart
	// re-routes the persisted result, which holds it again if it must.
	held bool
	// kept holds the spawn's messages to its spawner (send_to_spawner) that
	// could not go at once: its spawner's call is waiting on it, or a depth
	// fan-in holds its owner. They head its result. In memory only.
	kept []SpawnLogEntry
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
	Running bool
	Outcome string // "" while running
	Ended   time.Time
	Pending int // parent messages not yet delivered
	Timeout time.Duration
	Resumes int // times a foci restart has resumed it
	Log     []SpawnLogEntry
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
	t.add(parent, s, cancel, 0)
}

func (t *SpawnTracker) add(parent string, s RunningSpawn, cancel context.CancelFunc, timeout time.Duration) {
	t.addWithPrompt(parent, s, cancel, timeout, "")
}

// addWithPrompt registers a running spawn with the task it was given, and
// persists it so a restart can resume it.
func (t *SpawnTracker) addWithPrompt(parent string, s RunningSpawn, cancel context.CancelFunc, timeout time.Duration, prompt string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.spawns[parent] == nil {
		t.spawns[parent] = map[string]*trackedSpawn{}
	}
	t.spawns[parent][s.ID] = &trackedSpawn{info: s, cancel: cancel, running: true, timeout: timeout, prompt: prompt}
	t.mu.Unlock()
	t.persist(parent, s.ID)
}

// Running returns the running spawns of parentSession, oldest first (nil when
// there are none).
func (t *SpawnTracker) Running(parentSession string) []RunningSpawn {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []RunningSpawn
	for _, s := range t.spawns[parentSession] {
		if s.running {
			out = append(out, s.info)
		}
	}
	sortSpawns(out, func(r RunningSpawn) RunningSpawn { return r })
	return out
}

// List returns every known spawn of parentSession, running and finished,
// oldest first, without their logs.
func (t *SpawnTracker) List(parentSession string) []SpawnSnapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []SpawnSnapshot
	for _, s := range t.spawns[parentSession] {
		out = append(out, s.snapshot(0))
	}
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

// Lookup finds a spawn of parentSession by its id or its branch key, with its
// last n log entries (all when n <= 0).
func (t *SpawnTracker) Lookup(parentSession, idOrBranch string, n int) (SpawnSnapshot, bool) {
	if t == nil {
		return SpawnSnapshot{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.find(parentSession, idOrBranch); s != nil {
		snap := s.snapshot(n)
		if n <= 0 {
			snap.Log = slices.Clone(s.log)
		}
		return snap, true
	}
	return SpawnSnapshot{}, false
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

func (s *trackedSpawn) snapshot(n int) SpawnSnapshot {
	snap := SpawnSnapshot{
		RunningSpawn: s.info, Running: s.running, Ended: s.ended,
		Pending: len(s.pending), Timeout: s.timeout, Resumes: s.resumes,
	}
	if !s.running {
		snap.Outcome = s.outcome
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
	if s == nil || !s.running || s.stopped {
		t.mu.Unlock()
		return false
	}
	s.pending = append(s.pending, text)
	s.appendLog("message", "(queued) "+text)
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
func (t *SpawnTracker) finishOrNext(parent, id string, force bool) (next []string, done, stopped, interrupted bool, dropped int) {
	if t == nil {
		return nil, true, false, false, 0
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return nil, true, false, false, 0
	}
	if force && t.shuttingDown && !s.stopped {
		t.mu.Unlock()
		return nil, true, false, true, 0
	}
	if !force && !s.stopped && len(s.pending) > 0 {
		next = s.pending
		s.pending = nil
		t.mu.Unlock()
		t.persist(parent, id)
		return next, false, false, false, 0
	}
	defer t.mu.Unlock()
	dropped = len(s.pending)
	s.pending = nil
	s.running = false
	s.ended = time.Now()
	stopped = s.stopped
	t.pruneFinished(parent)
	// Not persisted here: the record stays "running" until the result is
	// recorded (pushResult), so a crash in between resumes the spawn rather
	// than losing its result.
	return nil, true, stopped, false, dropped
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
		if !s.running {
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
// run's cancel. False when it is unknown or already running.
func (t *SpawnTracker) restart(parent, id string, cancel context.CancelFunc) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil || s.running {
		t.mu.Unlock()
		return false
	}
	s.running, s.stopped, s.cancel = true, false, cancel
	s.ended, s.outcome = time.Time{}, ""
	t.mu.Unlock()
	t.persist(parent, id)
	return true
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
// (#2656). Not persisted.
func (t *SpawnTracker) addDirect(parent string, s RunningSpawn, cancel context.CancelFunc, timeout time.Duration, prompt string) {
	if t == nil {
		return
	}
	s.Direct = true
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spawns[parent] == nil {
		t.spawns[parent] = map[string]*trackedSpawn{}
	}
	ts := &trackedSpawn{info: s, cancel: cancel, running: true, timeout: timeout, prompt: prompt}
	ts.appendLog("prompt", prompt)
	t.spawns[parent][s.ID] = ts
}

// endDirect marks a direct spawn finished with outcome ("completed",
// "failed"; "stopped" when Stop or stopOne ended it, whatever outcome says)
// and returns who stopped it ("" = nobody) and the messages it kept for its
// caller (send_to_spawner), taken in the same step that ends it so none can
// arrive in between and be lost.
func (t *SpawnTracker) endDirect(parent, id, outcome, detail string) (stoppedBy string, kept []SpawnLogEntry) {
	if t == nil {
		return "", nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return "", nil
	}
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
		return "", kept
	}
	if s.stoppedBy == "" {
		return "the user", kept
	}
	return s.stoppedBy, kept
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
	defer t.mu.Unlock()
	s := t.find(parent, idOrBranch)
	if s == nil {
		return false, false
	}
	if !s.running || s.stopped {
		return true, false
	}
	s.stopped, s.stoppedBy = true, stoppedBySpawnStop
	s.cancel()
	return true, true
}

// Stop cancels every running spawn of parentSession and returns how many it
// stopped. Each spawn's own goroutine then ends it, reporting it stopped.
func (t *SpawnTracker) Stop(parentSession string) int {
	return t.stopAll(parentSession, "the user")
}

// stopAll is Stop, recording who stopped the spawns.
func (t *SpawnTracker) stopAll(parentSession, by string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, s := range t.spawns[parentSession] {
		if !s.running || s.stopped {
			continue
		}
		s.stopped, s.stoppedBy = true, by
		s.cancel()
		n++
	}
	return n
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

// spawnDescription is the one-line label a spawn is listed under: its prompt's
// opening, whitespace collapsed so a multi-line prompt reads as one line.
func spawnDescription(prompt string) string {
	r := []rune(strings.Join(strings.Fields(prompt), " "))
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
}

func (s *spawnTextSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TextBlock:
		if e.Text != "" {
			s.sawText.Store(true)
			if s.obs != nil {
				s.obs.SpawnText(s.parent, s.id, e.Text)
			}
			s.tracker.appendLog(s.parent, s.id, "text", e.Text)
		}
	case turnevent.ToolCall:
		s.tracker.appendLog(s.parent, s.id, "tool", toolCallSummary(e))
	case turnevent.TurnComplete:
		if e.FinalText != "" {
			s.sawText.Store(true)
			if s.obs != nil {
				s.obs.SpawnText(s.parent, s.id, e.FinalText)
			}
			// A backend that already sent the answer as a text block
			// (Claude Code) is not logged twice.
			if s.tracker.lastLogText(s.parent, s.id, "text") != e.FinalText {
				s.tracker.appendLog(s.parent, s.id, "text", e.FinalText)
			}
		}
	}
	s.BufferSink.Emit(ctx, ev)
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
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || !s.running {
		return false
	}
	s.kept = append(s.kept, SpawnLogEntry{At: time.Now(), Kind: "to-spawner", Text: text})
	s.appendLog("to-spawner", "(held for the result) "+text)
	return true
}

// takeKept returns, and forgets, the messages kept for the head of the
// spawn's result.
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
