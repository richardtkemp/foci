package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
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
}

// SpawnObserver is told about each background spawn of a parent session, so a
// platform can show it beside the agent's subagents (#2139). Every method may
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
type SpawnTracker struct {
	mu     sync.Mutex
	spawns map[string]map[string]*trackedSpawn // parent session → spawn id → spawn
}

type trackedSpawn struct {
	info    RunningSpawn
	cancel  context.CancelFunc
	stopped bool
	running bool
	timeout time.Duration // of its first run: a resumed run gets it again
	ended   time.Time     // when its last run ended; zero while running
	outcome string        // last run's end: "completed", "failed" or "stopped"
	pending []string      // parent messages waiting for the spawn's next step
	log     []SpawnLogEntry
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
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spawns[parent] == nil {
		t.spawns[parent] = map[string]*trackedSpawn{}
	}
	t.spawns[parent][s.ID] = &trackedSpawn{info: s, cancel: cancel, running: true, timeout: timeout}
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
		Pending: len(s.pending), Timeout: s.timeout,
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
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || !s.running || s.stopped {
		return false
	}
	s.pending = append(s.pending, text)
	s.appendLog("message", "(queued) "+text)
	return true
}

// drainPending takes the messages waiting for a spawn. The API turn calls it
// at each tool boundary (the spawn's turnevent.Steerer).
func (t *SpawnTracker) drainPending(parent, id string) []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || len(s.pending) == 0 {
		return nil
	}
	out := s.pending
	s.pending = nil
	return out
}

// finishOrNext ends a spawn's run. When parent messages arrived that its turn
// did not take, and the run may go on (not stopped, not failed: force false),
// it returns them as the next turn's input instead, done false. Otherwise it
// marks the spawn finished and returns done true, whether /stop ended it, and
// how many queued messages were never delivered. One lock covers both, so a
// message queued as the run ends is never lost: queue fails once done.
func (t *SpawnTracker) finishOrNext(parent, id string, force bool) (next []string, done, stopped bool, dropped int) {
	if t == nil {
		return nil, true, false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return nil, true, false, 0
	}
	if !force && !s.stopped && len(s.pending) > 0 {
		next = s.pending
		s.pending = nil
		return next, false, false, 0
	}
	dropped = len(s.pending)
	s.pending = nil
	s.running = false
	s.ended = time.Now()
	stopped = s.stopped
	t.pruneFinished(parent)
	return nil, true, stopped, dropped
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
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || s.running {
		return false
	}
	s.running, s.stopped, s.cancel = true, false, cancel
	s.ended, s.outcome = time.Time{}, ""
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

// Stop cancels every running spawn of parentSession and returns how many it
// stopped. Each spawn's own goroutine then ends it, reporting it stopped.
func (t *SpawnTracker) Stop(parentSession string) int {
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
		s.stopped = true
		s.cancel()
		n++
	}
	return n
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
}

func (s *spawnTextSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TextBlock:
		if e.Text != "" {
			if s.obs != nil {
				s.obs.SpawnText(s.parent, s.id, e.Text)
			}
			s.tracker.appendLog(s.parent, s.id, "text", e.Text)
		}
	case turnevent.ToolCall:
		s.tracker.appendLog(s.parent, s.id, "tool", toolCallSummary(e))
	case turnevent.TurnComplete:
		if e.FinalText != "" {
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
