package main

import (
	"encoding/json"
	"time"

	"foci/internal/modelcaps"
	"foci/internal/session"
)

// modelCapsPersister adapts SessionIndex (state.db) to modelcaps.Persister,
// translating modelcaps.Caps to/from the primitive session.ModelCapsRow the
// session package stores (effort/thinking JSON-encoded). This is the only place
// that knows both types, keeping modelcaps a DB-free leaf and the session
// package modelcaps-free. (#840)
type modelCapsPersister struct {
	idx *session.SessionIndex
}

// compile-time check: modelCapsPersister satisfies the injected interface.
var _ modelcaps.Persister = modelCapsPersister{}

func (p modelCapsPersister) Save(backend string, entries map[string]modelcaps.Caps, fetchedAt time.Time) error {
	rows := make([]session.ModelCapsRow, 0, len(entries))
	for model, c := range entries {
		rows = append(rows, session.ModelCapsRow{
			Model:         model,
			ContextWindow: c.ContextWindow,
			MaxOutput:     c.MaxOutput,
			EffortJSON:    marshalLevels(c.Effort),
			ThinkingJSON:  marshalLevels(c.Thinking),
		})
	}
	return p.idx.SaveModelCaps(backend, rows, fetchedAt)
}

func (p modelCapsPersister) Load(backend string) (map[string]modelcaps.Caps, time.Time, error) {
	rows, fetchedAt, err := p.idx.LoadModelCaps(backend)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(rows) == 0 {
		return nil, time.Time{}, nil
	}
	entries := make(map[string]modelcaps.Caps, len(rows))
	for _, r := range rows {
		entries[r.Model] = modelcaps.Caps{
			ContextWindow: r.ContextWindow,
			MaxOutput:     r.MaxOutput,
			Effort:        unmarshalLevels(r.EffortJSON),
			Thinking:      unmarshalLevels(r.ThinkingJSON),
		}
	}
	return entries, fetchedAt, nil
}

// marshalLevels encodes a level slice as a JSON array; empty/nil → "".
func marshalLevels(levels []string) string {
	if len(levels) == 0 {
		return ""
	}
	b, err := json.Marshal(levels)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalLevels decodes marshalLevels output; "" or malformed → nil.
func unmarshalLevels(s string) []string {
	if s == "" {
		return nil
	}
	var levels []string
	if err := json.Unmarshal([]byte(s), &levels); err != nil {
		return nil
	}
	return levels
}
