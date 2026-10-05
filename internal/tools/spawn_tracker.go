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

// SpawnTracker holds every running background spawn, keyed by parent session,
// so /stop can stop a session's spawns on their own (#2139) and the statusline
// can list them (#2127).
type SpawnTracker struct {
	mu      sync.Mutex
	running map[string]map[string]*trackedSpawn // parent session → spawn id → spawn
}

type trackedSpawn struct {
	info    RunningSpawn
	cancel  context.CancelFunc
	stopped bool
}

// Spawns is the process-wide tracker the spawn tool registers with and /stop
// stops through. Session keys carry the agent id, so one tracker serves every
// agent.
var Spawns = NewSpawnTracker()

// NewSpawnTracker returns an empty tracker.
func NewSpawnTracker() *SpawnTracker {
	return &SpawnTracker{running: map[string]map[string]*trackedSpawn{}}
}

// Add registers a running spawn of parent; its goroutine removes it when it ends.
func (t *SpawnTracker) Add(parent string, s RunningSpawn, cancel context.CancelFunc) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running[parent] == nil {
		t.running[parent] = map[string]*trackedSpawn{}
	}
	t.running[parent][s.ID] = &trackedSpawn{info: s, cancel: cancel}
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
	for _, s := range t.running[parentSession] {
		out = append(out, s.info)
	}
	slices.SortFunc(out, func(a, b RunningSpawn) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// remove forgets a finished spawn and reports whether Stop ended it.
func (t *SpawnTracker) remove(parent, id string) (stopped bool) {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.running[parent][id]
	if s == nil {
		return false
	}
	delete(t.running[parent], id)
	if len(t.running[parent]) == 0 {
		delete(t.running, parent)
	}
	return s.stopped
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
	for _, s := range t.running[parentSession] {
		if s.stopped {
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

// spawnTextSink forwards a spawn's assistant text to the observer while the
// inner BufferSink collects its final text. The API turn emits only the text
// between tool calls as TextBlocks; the answer itself arrives on TurnComplete.
type spawnTextSink struct {
	*turnevent.BufferSink
	obs        SpawnObserver
	parent, id string
}

func (s *spawnTextSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TextBlock:
		if e.Text != "" {
			s.obs.SpawnText(s.parent, s.id, e.Text)
		}
	case turnevent.TurnComplete:
		if e.FinalText != "" {
			s.obs.SpawnText(s.parent, s.id, e.FinalText)
		}
	}
	s.BufferSink.Emit(ctx, ev)
}
