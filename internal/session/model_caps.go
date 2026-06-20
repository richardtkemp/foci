package session

import (
	"fmt"
	"time"

	"foci/internal/timeutil"
)

// ModelCapsRow is one persisted model-capability record in primitive form, so
// the session package needn't import modelcaps (avoids a layering dependency).
// effort/thinking arrive pre-encoded as JSON arrays — the modelcaps adapter in
// cmd/foci-gw does the Caps<->row translation. (#840)
type ModelCapsRow struct {
	Model         string
	ContextWindow int
	MaxOutput     int
	EffortJSON    string // JSON-encoded []string, e.g. ["low","high"]; "" = none
	ThinkingJSON  string // JSON-encoded []string; "" = none
}

// SaveModelCaps replaces all persisted capability rows for a backend with the
// given set, stamped with fetchedAt. Run in one transaction so a reader never
// observes a half-written catalogue. Called after a successful catalogue fetch
// so the record survives a restart. An empty rows slice clears the backend's
// rows (kept transactional for consistency).
func (idx *SessionIndex) SaveModelCaps(backend string, rows []ModelCapsRow, fetchedAt time.Time) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("model_caps: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.Exec(`DELETE FROM model_caps WHERE backend = ?`, backend); err != nil {
		return fmt.Errorf("model_caps: clear backend %q: %w", backend, err)
	}

	stamp := timeutil.Format(fetchedAt)
	for _, r := range rows {
		if _, err := tx.Exec(
			`INSERT INTO model_caps
			 (backend, model, context_window, max_output, effort_json, thinking_json, fetched_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			backend, r.Model, r.ContextWindow, r.MaxOutput, r.EffortJSON, r.ThinkingJSON, stamp,
		); err != nil {
			return fmt.Errorf("model_caps: insert %q/%q: %w", backend, r.Model, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("model_caps: commit: %w", err)
	}
	return nil
}

// LoadModelCaps returns the persisted rows for a backend and the fetch time
// they were stamped with. A backend with no rows returns (nil, zero, nil) — not
// an error — so the caller treats it as a cold cache and falls back to the
// static registry until the first live fetch lands. All rows of a backend share
// one fetched_at (written together in SaveModelCaps), so the first row's stamp
// is authoritative.
func (idx *SessionIndex) LoadModelCaps(backend string) ([]ModelCapsRow, time.Time, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	dbRows, err := idx.db.Query(
		`SELECT model, context_window, max_output, effort_json, thinking_json, fetched_at
		 FROM model_caps WHERE backend = ?`,
		backend,
	)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("model_caps: query backend %q: %w", backend, err)
	}
	defer func() { _ = dbRows.Close() }()

	var (
		rows      []ModelCapsRow
		fetchedAt time.Time
	)
	for dbRows.Next() {
		var (
			r        ModelCapsRow
			stampStr string
		)
		if err := dbRows.Scan(&r.Model, &r.ContextWindow, &r.MaxOutput, &r.EffortJSON, &r.ThinkingJSON, &stampStr); err != nil {
			return nil, time.Time{}, fmt.Errorf("model_caps: scan: %w", err)
		}
		if fetchedAt.IsZero() {
			fetchedAt, _ = time.Parse(time.RFC3339, stampStr)
		}
		rows = append(rows, r)
	}
	if err := dbRows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("model_caps: iterate: %w", err)
	}
	return rows, fetchedAt, nil
}
