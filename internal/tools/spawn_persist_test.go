package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
)

// memSpawnStore is an in-memory SpawnStore (agent_metadata rows).
type memSpawnStore struct {
	mu   sync.Mutex
	rows map[string]map[string]string // agent → key → value
}

func newMemSpawnStore() *memSpawnStore {
	return &memSpawnStore{rows: map[string]map[string]string{}}
}

func (m *memSpawnStore) SetAgentMetadata(agentID, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows[agentID] == nil {
		m.rows[agentID] = map[string]string{}
	}
	m.rows[agentID][key] = value
	return nil
}

func (m *memSpawnStore) DeleteAgentMetadata(agentID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows[agentID], key)
	return nil
}

func (m *memSpawnStore) AgentMetadataByPrefix(agentID, prefix string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.rows[agentID] {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

// record returns the persisted record of spawn id, if any.
func (m *memSpawnStore) record(t *testing.T, id string) (spawnRecord, bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows["test"][spawnMetaPrefix+id]
	if !ok {
		return spawnRecord{}, false
	}
	var rec spawnRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	return rec, true
}

func (m *memSpawnStore) put(t *testing.T, rec spawnRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SetAgentMetadata("test", spawnMetaPrefix+rec.ID, string(b))
}

// delivery is one InjectToAgentThen call: the test decides whether the
// parent's turn took it.
type delivery struct {
	session, msg string
	done         func(bool)
}

type persistHarness struct {
	spawner    *Spawner
	tracker    *SpawnTracker
	store      *memSpawnStore
	deliveries chan delivery
	notices    chan string
	ctx        context.Context
}

func newPersistHarness(agent SpawnAgent, deps func(*SpawnDeps)) *persistHarness {
	h := &persistHarness{
		tracker: NewSpawnTracker(), store: newMemSpawnStore(),
		deliveries: make(chan delivery, 8), notices: make(chan string, 8),
	}
	h.tracker.SetStore(h.store)
	notifier := NewAsyncNotifier(func(sk, msg, _, _ string, _ *AgentOrigin) {
		h.deliveries <- delivery{session: sk, msg: msg, done: func(bool) {}}
	}).WithDeliverThen(func(sk, msg, _ string, done func(bool)) {
		h.deliveries <- delivery{session: sk, msg: msg, done: done}
	})
	d := SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    "test",
		MaxInherit: 2,
		Notifier:   notifier,
		Tracker:    h.tracker,
		Notice:     func(sk, text string) { h.notices <- sk + "\n" + text },
	}
	if deps != nil {
		deps(&d)
	}
	h.spawner = NewSpawner(d, func() SpawnAgent { return agent })
	h.ctx = WithSessionKey(context.Background(), "test/imain")
	return h
}

func (h *persistHarness) spawn(t *testing.T, prompt string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"prompt": prompt, "context": "clone", "timeout": 30})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := spawnIDRe.FindStringSubmatch(res.Text)
	if m == nil {
		t.Fatalf("no spawn id in %q", res.Text)
	}
	return m[1]
}

func (h *persistHarness) nextDelivery(t *testing.T) delivery {
	t.Helper()
	select {
	case d := <-h.deliveries:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a delivery to the parent")
		return delivery{}
	}
}

func (h *persistHarness) noDelivery(t *testing.T) {
	t.Helper()
	select {
	case d := <-h.deliveries:
		t.Fatalf("unexpected delivery to %s: %q", d.session, d.msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// A running spawn is persisted with its task; its result stays persisted
// until the parent's turn has taken it, so a refused or lost delivery is not
// the end of it (#2589).
func TestSpawnPersistedUntilParentTakesResult(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newPersistHarness(agent, nil)
	id := h.spawn(t, "count the stars")
	<-agent.started

	rec, ok := h.store.record(t, id)
	if !ok || !rec.Running || rec.Prompt != "count the stars" || rec.Parent != "test/imain" || rec.BranchKey == "" {
		t.Fatalf("running spawn record = %+v (ok %v), want running with its prompt", rec, ok)
	}

	close(agent.gates[0])
	d := h.nextDelivery(t)
	if !strings.Contains(d.msg, "[SPAWN RESULT]") || !strings.Contains(d.msg, "completed") {
		t.Fatalf("delivered %q, want a completed [SPAWN RESULT]", d.msg)
	}
	rec, ok = h.store.record(t, id)
	if !ok || rec.Running || rec.Result != d.msg {
		t.Fatalf("record while the parent has not taken the result = %+v (ok %v), want the result kept", rec, ok)
	}

	d.done(false) // refused: still kept
	if _, ok := h.store.record(t, id); !ok {
		t.Fatal("a refused result was forgotten")
	}
	d.done(true)
	if rec, ok := h.store.record(t, id); ok {
		t.Fatalf("record after the parent took the result = %+v, want none", rec)
	}
}

// errAgent fails its turn once its context ends or release is closed, as a
// delegated turn does when the shutdown closes its backend.
type errAgent struct {
	started chan struct{}
	release chan struct{}
}

func (a *errAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	a.started <- struct{}{}
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	return errors.New("backend closed")
}

// A run cut short once foci is shutting down is neither reported failed nor
// forgotten: its record stays running for the next process to resume.
func TestSpawnCutByShutdownLeftForNextProcess(t *testing.T) {
	t.Parallel()
	agent := &errAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	h := newPersistHarness(agent, nil)
	id := h.spawn(t, "long task")
	<-agent.started

	h.tracker.BeginShutdown()
	close(agent.release)

	h.noDelivery(t)
	rec, ok := h.store.record(t, id)
	if !ok || !rec.Running || rec.Result != "" {
		t.Fatalf("record after a shutdown cut = %+v (ok %v), want still running", rec, ok)
	}
}

// Without a shutdown the same failure is reported to the parent as before.
func TestSpawnFailureWithoutShutdownStillReported(t *testing.T) {
	t.Parallel()
	agent := &errAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	h := newPersistHarness(agent, nil)
	h.spawn(t, "long task")
	<-agent.started
	close(agent.release)
	if d := h.nextDelivery(t); !strings.Contains(d.msg, "failed") {
		t.Fatalf("delivered %q, want a failed [SPAWN RESULT]", d.msg)
	}
}

func runningRecord(id string) spawnRecord {
	return spawnRecord{
		ID: id, Parent: "test/imain", BranchKey: "test/imain/b" + id,
		Description: "count the stars", Prompt: "count the stars",
		Started: time.Now().Add(-time.Minute), Timeout: 30 * time.Second,
		Running: true, Pending: []string{spawnMessagePrefix + "also the moons"},
	}
}

// A spawn the last process left running is resumed on its branch: a restart
// notice restating its task, its backend's unconsumed inputs, then the parent
// messages that were waiting. The parent still gets its result, and the user
// is told it was resumed.
func TestRestoreSpawnsResumesRunningSpawn(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	var took []string
	h := newPersistHarness(agent, func(d *SpawnDeps) {
		d.TakeUndelivered = func(bk string) []string {
			took = append(took, bk)
			return []string{"unconsumed input"}
		}
	})
	rec := runningRecord("spawn-aa")
	h.store.put(t, rec)

	h.spawner.RestoreSpawns()
	if len(took) != 1 || took[0] != rec.BranchKey {
		t.Fatalf("TakeUndelivered calls = %v, want the branch once, before RestoreSpawns returns", took)
	}
	select {
	case n := <-h.notices:
		if !strings.HasPrefix(n, "test/imain\n") || !strings.Contains(n, "Resumed 1") || !strings.Contains(n, "spawn-aa") {
			t.Fatalf("notice = %q, want the parent's user told spawn-aa was resumed", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no restart notice for the user")
	}

	<-agent.started
	if got, ok := h.store.record(t, rec.ID); !ok || got.Resumes != 1 || !got.Running {
		t.Fatalf("record during the resumed run = %+v (ok %v), want running with Resumes 1", got, ok)
	}
	calls, keys := agent.turns()
	if keys[0] != rec.BranchKey {
		t.Fatalf("resumed on %q, want branch %q", keys[0], rec.BranchKey)
	}
	texts := calls[0]
	if len(texts) != 3 || !strings.Contains(texts[0], "FOCI RESTARTED") || !strings.Contains(texts[0], "count the stars") ||
		texts[1] != "unconsumed input" || texts[2] != rec.Pending[0] {
		t.Fatalf("resume turn inputs = %q, want notice+task, unconsumed input, pending message", texts)
	}
	if snap, ok := h.tracker.Lookup("test/imain", rec.ID, 0); !ok || !snap.Running || snap.Resumes != 1 {
		t.Fatalf("tracker after restore = %+v (ok %v), want it running and resumed once", snap, ok)
	}

	close(agent.gates[0])
	d := h.nextDelivery(t)
	if d.session != "test/imain" || !strings.Contains(d.msg, "completed") || !strings.Contains(d.msg, "resumed 1 time(s)") {
		t.Fatalf("delivered %q to %s, want the parent's completed result naming the resume", d.msg, d.session)
	}
	d.done(true)
	if _, ok := h.store.record(t, rec.ID); ok {
		t.Fatal("record kept after the parent took the result")
	}
}

// A spawn that cannot be resumed is reported to its parent and its user as
// lost, never dropped silently.
func TestRestoreSpawnsReportsUnresumableAsLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		resumes   int
		canResume bool
		reason    string
	}{
		{"no saved conversation", 0, false, "no saved conversation"},
		{"too many restarts", maxSpawnRestartResumes, true, "interrupted it 3 times"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := newScriptedSpawnAgent(0)
			h := newPersistHarness(agent, func(d *SpawnDeps) {
				d.CanResume = func(string) bool { return tc.canResume }
			})
			rec := runningRecord("spawn-bb")
			rec.Resumes = tc.resumes
			h.store.put(t, rec)

			h.spawner.RestoreSpawns()
			d := h.nextDelivery(t)
			if !strings.Contains(d.msg, "[SPAWN RESULT]") || !strings.Contains(d.msg, "lost in a foci restart") ||
				!strings.Contains(d.msg, tc.reason) || !strings.Contains(d.msg, "count the stars") {
				t.Fatalf("parent got %q, want a lost [SPAWN RESULT] with the reason and the task", d.msg)
			}
			if n := <-h.notices; !strings.Contains(n, "Could not resume 1") || !strings.Contains(n, "spawn-bb") {
				t.Fatalf("notice = %q, want the user told spawn-bb was lost", n)
			}
			select {
			case <-agent.started:
				t.Fatal("an unresumable spawn was run")
			case <-time.After(50 * time.Millisecond):
			}
			if snap, ok := h.tracker.Lookup("test/imain", rec.ID, 0); !ok || snap.Running || snap.Outcome != "lost" {
				t.Fatalf("tracker = %+v (ok %v), want it known as lost", snap, ok)
			}
			d.done(true)
			if _, ok := h.store.record(t, rec.ID); ok {
				t.Fatal("record kept after the parent took the lost report")
			}
		})
	}
}

// A result the last process could not hand over is delivered by the next one.
func TestRestoreSpawnsDeliversUntakenResult(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h := newPersistHarness(agent, nil)
	rec := runningRecord("spawn-cc")
	rec.Running, rec.Result = false, "[SPAWN RESULT] Branch x (spawn spawn-cc) completed:\n\n42"
	h.store.put(t, rec)

	h.spawner.RestoreSpawns()
	d := h.nextDelivery(t)
	if d.msg != rec.Result {
		t.Fatalf("delivered %q, want the persisted result", d.msg)
	}
	select {
	case n := <-h.notices:
		t.Fatalf("notice %q for a restart that resumed nothing", n)
	default:
	}
	d.done(true)
	if _, ok := h.store.record(t, rec.ID); ok {
		t.Fatal("record kept after delivery")
	}
}

// /stop reaches a restored spawn while it still waits for a slot.
func TestRestoredSpawnStoppableBeforeItRuns(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h := newPersistHarness(agent, func(d *SpawnDeps) { d.MaxInherit = 1 })
	h.spawner.sem <- struct{}{} // the only slot is taken
	rec := runningRecord("spawn-dd")
	h.store.put(t, rec)

	h.spawner.RestoreSpawns()
	<-h.notices
	if n := h.tracker.Stop("test/imain"); n != 1 {
		t.Fatalf("Stop stopped %d, want the waiting restored spawn", n)
	}
	<-h.spawner.sem
	d := h.nextDelivery(t)
	if !strings.Contains(d.msg, "stopped by the user") {
		t.Fatalf("delivered %q, want stopped", d.msg)
	}
}
