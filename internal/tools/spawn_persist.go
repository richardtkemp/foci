package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"foci/internal/session"
)

// SpawnStore is where SpawnTracker keeps the spawns a foci restart must not
// lose (#2589): one agent_metadata row per spawn, under spawnMetaPrefix.
// *session.SessionIndex implements it.
type SpawnStore interface {
	SetAgentMetadata(agentID, key, value string) error
	DeleteAgentMetadata(agentID, key string) error
	AgentMetadataByPrefix(agentID, prefix string) (map[string]string, error)
}

// spawnMetaPrefix heads the agent_metadata key of each persisted spawn.
const spawnMetaPrefix = "spawn:"

// maxSpawnRestartResumes is how many restarts in a row may resume one spawn.
// A spawn interrupted more often than that is reported dead instead, so a spawn
// that crashes foci cannot crash it forever.
const maxSpawnRestartResumes = 3

// spawnRecord is the persisted form of one spawn: what a new process needs to
// resume it, or to hand its finished result to the parent.
type spawnRecord struct {
	ID        string `json:"id"`
	Parent    string `json:"parent"`
	BranchKey string `json:"branch_key"`
	// Name: see RunningSpawn (#2721); "" in a record written before names.
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description"`
	Model       string    `json:"model,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	Started     time.Time `json:"started"`
	Pending     []string  `json:"pending,omitempty"`
	Resumes     int       `json:"resumes,omitempty"`
	// Running is true while a run is under way: a new process resumes it.
	Running bool `json:"running"`
	// Result is a [SPAWN RESULT] the parent has not taken yet: a new process
	// delivers it.
	Result string `json:"result,omitempty"`
	// Character and SpawnModel: see RunningSpawn (#2643).
	Character  bool   `json:"character,omitempty"`
	SpawnModel string `json:"spawn_model,omitempty"`
	// Agent: see RunningSpawn (#2662).
	Agent string `json:"agent,omitempty"`
	// SpawnerUnattended: see RunningSpawn (#2684).
	SpawnerUnattended bool `json:"spawner_unattended,omitempty"`
	// Depth: see RunningSpawn (#3061); 0 in a record written before it.
	Depth int `json:"depth,omitempty"`
	// Kept: the messages to its spawner kept for the head of its result
	// (send_to_spawner, #2662), in the order sent. A resumed run's result
	// still carries them (#2677).
	Kept []keptRecord `json:"kept,omitempty"`
	// OneShot: an API agent's direct one-shot (#2979), resumed from its
	// saved conversation (oneShotRecord.Conversation, #2984), not by a turn
	// on a branch; it has no BranchKey. nil for every other spawn.
	OneShot *oneShotRecord `json:"one_shot,omitempty"`
	// Wake: what its own spawns sent it after its run ended, in arrival
	// order, for the run that resumes it (#3163); a new process resumes it
	// with them. Stopped: it was stopped, so they pass up instead.
	Wake    []childReport `json:"wake,omitempty"`
	Stopped bool          `json:"stopped,omitempty"`
}

// keptRecord is the persisted form of one kept message to the spawner.
type keptRecord struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// keptRecords is kept's persisted form.
func keptRecords(kept []SpawnLogEntry) []keptRecord {
	if len(kept) == 0 {
		return nil
	}
	out := make([]keptRecord, len(kept))
	for i, m := range kept {
		out[i] = keptRecord{At: m.At, Text: m.Text}
	}
	return out
}

// keptEntries is the in-memory form of a record's kept messages.
func keptEntries(recs []keptRecord) []SpawnLogEntry {
	if len(recs) == 0 {
		return nil
	}
	out := make([]SpawnLogEntry, len(recs))
	for i, m := range recs {
		out[i] = SpawnLogEntry{At: m.At, Kind: "to-spawner", Text: m.Text}
	}
	return out
}

// SetStore makes the tracker persist its spawns to store. Call it once at
// startup, before any spawn runs.
func (t *SpawnTracker) SetStore(store SpawnStore) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.store = store
	t.mu.Unlock()
}

// BeginShutdown tells the tracker foci is shutting down: from now on a run that
// ends cut short keeps its persisted record and is resumed by the next process,
// instead of being reported failed to a parent that can no longer take it.
func (t *SpawnTracker) BeginShutdown() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.shuttingDown = true
	t.mu.Unlock()
}

// BeginAgentStop is BeginShutdown for one agent's spawns: the live stop of
// that agent (#2787) has begun, so a run of one of its sessions cut short
// from now on keeps its persisted record, which the agent's next start (the
// renamed agent's, after a live rename) resumes.
func (t *SpawnTracker) BeginAgentStop(agentID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.stopping == nil {
		t.stopping = make(map[string]bool)
	}
	t.stopping[agentID] = true
	t.mu.Unlock()
}

// ForgetAgent drops the in-memory entries of agentID's spawns once its live
// stop is done, and ends BeginAgentStop for it. The persisted records are
// not touched: they are what the agent's next start resumes from.
func (t *SpawnTracker) ForgetAgent(agentID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for parent := range t.spawns {
		if session.AgentIDFromAnyKey(parent) == agentID {
			delete(t.spawns, parent)
			delete(t.routes, parent)
		}
	}
	delete(t.stopping, agentID)
}

// record is the persisted form of s, or nil when nothing about it must
// survive a restart (finished, result delivered, no reports waiting to wake
// it). live: its own spawns still run or hold a result for it, so it must
// stay known for them (#3163). Caller holds t.mu.
func (s *trackedSpawn) record(parent string, live bool) *spawnRecord {
	if !s.running && s.result == "" && len(s.wake) == 0 && !live {
		return nil
	}
	return &spawnRecord{
		ID: s.info.ID, Parent: parent, BranchKey: s.info.BranchKey,
		Name: s.info.Name, Description: s.info.Description, Model: s.info.Model, Prompt: s.prompt,
		Started: s.info.Started, Pending: slices.Clone(s.pending),
		Resumes: s.resumes, Running: s.running, Result: s.result,
		Character: s.info.Character, SpawnModel: s.info.SpawnModel,
		Agent: s.info.Agent, SpawnerUnattended: s.info.SpawnerUnattended, Depth: s.info.Depth,
		Kept: keptRecords(s.kept), OneShot: s.oneShot,
		Wake: slices.Clone(s.wake), Stopped: s.stopped && !s.running,
	}
}

// persist writes the spawn's current state to the store, or deletes its row
// when nothing about it must survive a restart. A no-op without a store.
// When parent is itself a background spawn's branch, that spawn's row is
// rewritten too: whether it must stay depends on its spawns (#3163).
func (t *SpawnTracker) persist(parent, id string) {
	if t == nil {
		return
	}
	upward := false
	for range maxPassUp {
		up, upID, ok := t.persistOne(parent, id, upward)
		if !ok {
			return
		}
		parent, id, upward = up, upID, true
	}
}

// persistOne is persist for one spawn. It returns the spawn whose branch is
// parent, when there is one and the store is set. upward: the call comes
// from one of the spawn's own spawns, and leaves alone a spawn whose run has
// ended and whose result is not recorded yet (reporting): its row stays
// "running" until then (finishOrNext).
func (t *SpawnTracker) persistOne(parent, id string, upward bool) (upParent, upID string, up bool) {
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	t.mu.Lock()
	store := t.store
	var rec *spawnRecord
	if s := t.spawns[parent][id]; s != nil {
		if s.info.Direct && s.oneShot == nil {
			// A direct spawn's caller waits for it in this process only;
			// an API one-shot is the exception (#2979).
			t.mu.Unlock()
			return "", "", false
		}
		if upward && s.reporting {
			t.mu.Unlock()
			return "", "", false
		}
		rec = s.record(parent, t.liveChildrenLocked(s.info.BranchKey))
	}
	if store != nil {
		upParent, upID, up = t.spawnOfBranchLocked(parent)
	}
	t.mu.Unlock()
	if store == nil {
		return "", "", false
	}
	t.writeRecord(store, parent, id, rec)
	return upParent, upID, up
}

// writeRecord stores rec as parent's spawn id's row, or deletes the row when
// rec is nil.
func (t *SpawnTracker) writeRecord(store SpawnStore, parent, id string, rec *spawnRecord) {
	agentID, key := session.AgentIDFromKey(parent), spawnMetaPrefix+id
	if rec == nil {
		if err := store.DeleteAgentMetadata(agentID, key); err != nil {
			spawnLog.Warnf("spawn=%s forget persisted record: %v", id, err)
		}
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		spawnLog.Errorf("spawn=%s encode record: %v", id, err)
		return
	}
	if err := store.SetAgentMetadata(agentID, key, string(b)); err != nil {
		spawnLog.Warnf("spawn=%s persist record: %v", id, err)
	}
}

// clearResult forgets msg once the parent has taken it. A newer result (a
// resumed run's) is kept. A spawn the tracker has already forgotten just has
// its row deleted.
func (t *SpawnTracker) clearResult(parent, id, msg string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if s := t.spawns[parent][id]; s != nil {
		if s.result != msg {
			t.mu.Unlock()
			return
		}
		s.result = ""
	}
	t.mu.Unlock()
	t.persist(parent, id)
}

// setCancel replaces a running spawn's cancel (a restored spawn gets its run's
// cancel once a slot frees up).
func (t *SpawnTracker) setCancel(parent, id string, cancel func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		s.cancel = cancel
	}
}

// loadRecords returns agentID's persisted spawns, oldest first. A row that
// does not decode is logged and deleted: it can be neither resumed nor
// reported.
func (t *SpawnTracker) loadRecords(agentID string) []spawnRecord {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	store := t.store
	t.mu.Unlock()
	if store == nil {
		return nil
	}
	rows, err := store.AgentMetadataByPrefix(agentID, spawnMetaPrefix)
	if err != nil {
		spawnLog.Errorf("agent=%s load persisted spawns: %v", agentID, err)
		return nil
	}
	var out []spawnRecord
	for key, val := range rows {
		var rec spawnRecord
		if err := json.Unmarshal([]byte(val), &rec); err != nil || rec.ID == "" || rec.Parent == "" || (rec.BranchKey == "" && rec.OneShot == nil) {
			spawnLog.Errorf("agent=%s persisted spawn %s unreadable (%v) — dropping it: %.200q", agentID, key, err, val)
			_ = store.DeleteAgentMetadata(agentID, key)
			continue
		}
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b spawnRecord) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// adopt registers a persisted spawn in a new process. running marks it as
// about to be resumed (its resume count goes up, and is persisted before the
// run starts, so a spawn that crashes foci is not resumed forever; its
// rec.Pending stays pending, and persisted, until the resumed run takes it,
// #3071); otherwise it is known as finished with outcome. cancel stops it.
func (t *SpawnTracker) adopt(rec spawnRecord, running bool, outcome string, cancel func()) {
	if t == nil {
		return
	}
	s := &trackedSpawn{
		info:   recInfo(rec),
		cancel: cancel, running: running,
		prompt: rec.Prompt, resumes: rec.Resumes, result: rec.Result,
		kept: keptEntries(rec.Kept), oneShot: rec.OneShot,
		wake: slices.Clone(rec.Wake), stopped: rec.Stopped && !running,
	}
	if running {
		s.resumes++
		s.pending = slices.Clone(rec.Pending)
	} else {
		s.ended, s.outcome = time.Now(), outcome
	}
	t.routeOf(rec.Parent) // its owner is known before anything reads it (#3206)
	t.mu.Lock()
	if t.spawns[rec.Parent] == nil {
		t.spawns[rec.Parent] = map[string]*trackedSpawn{}
	}
	t.spawns[rec.Parent][rec.ID] = s
	t.mu.Unlock()
	t.persist(rec.Parent, rec.ID)
}

// resumeCount is how many times a restart has resumed the spawn.
func (t *SpawnTracker) resumeCount(parent, id string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		return s.resumes
	}
	return 0
}
