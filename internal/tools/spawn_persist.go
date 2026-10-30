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
	ID          string        `json:"id"`
	Parent      string        `json:"parent"`
	BranchKey   string        `json:"branch_key"`
	Description string        `json:"description"`
	Model       string        `json:"model,omitempty"`
	Prompt      string        `json:"prompt,omitempty"`
	Started     time.Time     `json:"started"`
	Timeout     time.Duration `json:"timeout"`
	Pending     []string      `json:"pending,omitempty"`
	Resumes     int           `json:"resumes,omitempty"`
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

// record is the persisted form of s, or nil when nothing about it must
// survive a restart (finished, result delivered). Caller holds t.mu.
func (s *trackedSpawn) record(parent string) *spawnRecord {
	if !s.running && s.result == "" {
		return nil
	}
	return &spawnRecord{
		ID: s.info.ID, Parent: parent, BranchKey: s.info.BranchKey,
		Description: s.info.Description, Model: s.info.Model, Prompt: s.prompt,
		Started: s.info.Started, Timeout: s.timeout, Pending: slices.Clone(s.pending),
		Resumes: s.resumes, Running: s.running, Result: s.result,
		Character: s.info.Character, SpawnModel: s.info.SpawnModel,
		Agent: s.info.Agent,
	}
}

// persist writes the spawn's current state to the store, or deletes its row
// when nothing about it must survive a restart. A no-op without a store.
func (t *SpawnTracker) persist(parent, id string) {
	if t == nil {
		return
	}
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	t.mu.Lock()
	store := t.store
	var rec *spawnRecord
	if s := t.spawns[parent][id]; s != nil {
		if s.info.Direct {
			// A direct spawn's caller waits for it in this process only.
			t.mu.Unlock()
			return
		}
		rec = s.record(parent)
	}
	t.mu.Unlock()
	if store == nil {
		return
	}
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
		if err := json.Unmarshal([]byte(val), &rec); err != nil || rec.ID == "" || rec.Parent == "" || rec.BranchKey == "" {
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
// run starts, so a spawn that crashes foci is not resumed forever); otherwise
// it is known as finished with outcome. cancel stops it.
func (t *SpawnTracker) adopt(rec spawnRecord, running bool, outcome string, cancel func()) {
	if t == nil {
		return
	}
	s := &trackedSpawn{
		info:   recInfo(rec),
		cancel: cancel, running: running, timeout: rec.Timeout,
		prompt: rec.Prompt, resumes: rec.Resumes, result: rec.Result,
	}
	if running {
		s.resumes++
	} else {
		s.ended, s.outcome = time.Now(), outcome
	}
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
