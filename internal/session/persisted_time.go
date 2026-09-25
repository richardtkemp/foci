package session

import "time"

// PersistedTime is one restart-surviving scheduler timestamp: the
// agent_metadata row (agentID, key). It is the single mechanism every
// scheduler/gate uses for "when did X last happen" (or "until when is X
// closed") state that must not reset on a restart (#2026) — the periodic
// runner's reflection/consolidation/reset/background/cleanup timers, the
// warning dispatchers' cadence, and the endpoint rate-limit gates. Keeping the
// encoding and the load/save pair in one place is what stops each caller from
// growing its own slightly different parse.
//
// The zero value and a PersistedTime over a nil index are valid no-ops: Load
// reports nothing stored and Save succeeds, so test/struct-literal callers
// without a state DB degrade to the old in-memory behaviour.
type PersistedTime struct {
	idx     *SessionIndex
	agentID string
	key     string
}

// PersistedTime returns the handle for the timestamp stored under
// (agentID, key). Safe on a nil index.
func (idx *SessionIndex) PersistedTime(agentID, key string) PersistedTime {
	return PersistedTime{idx: idx, agentID: agentID, key: key}
}

// Load returns the stored timestamp. ok is false when there is no index,
// nothing is stored, or the stored value doesn't parse (callers then keep
// their own boot default). Values written as RFC3339 (seconds — the format
// used before this helper existed) parse too.
func (p PersistedTime) Load() (t time.Time, ok bool) {
	if p.idx == nil {
		return time.Time{}, false
	}
	raw, err := p.idx.GetAgentMetadata(p.agentID, p.key)
	if err != nil || raw == "" {
		return time.Time{}, false
	}
	t, err = time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Save stores t, or deletes the row when t is zero (so "never" round-trips as
// "nothing stored" rather than as year 1).
func (p PersistedTime) Save(t time.Time) error {
	if p.idx == nil {
		return nil
	}
	if t.IsZero() {
		return p.idx.DeleteAgentMetadata(p.agentID, p.key)
	}
	return p.idx.SetAgentMetadata(p.agentID, p.key, t.Format(time.RFC3339Nano))
}
