package app

import (
	"database/sql"
	"errors"
	"sync"
	"time"

	"foci/internal/sqlite"
)

// Checkbox-mirror deployment defaults (#1360) — what the [platforms.app]
// checkbox_* knobs resolve to when unset (provider.go registers them as the
// platform defaults; newHub resolves any override). Bounds are needed so a
// hello replay, one put and the store cannot grow without limit.
const (
	defaultCheckboxStoreFile  = "app-checkboxes.db"  // durable checkbox-state DB, relative to data_dir (the app-frames.db naming)
	defaultCheckboxDocCap     = 10000                // max stored documents; an existing one can always be updated
	defaultCheckboxReplayDocs = 200                  // max documents one post-hello replay sends
	defaultCheckboxReplayTTL  = 90 * 24 * time.Hour  // max age of a document a post-hello replay sends
	defaultCheckboxRetention  = 365 * 24 * time.Hour // hub-start sweep removes documents not updated for this long
)

// errCheckboxCap is returned by put when a NEW document would exceed the
// store's document cap. Deliberately not sent to the client: a full store is
// not something the sender can fix by retrying, so the put is logged,
// dropped, and stays un-mirrored until the sweep frees room.
var errCheckboxCap = errors.New("checkbox document cap exceeded")

// checkboxStore is the durable half of the checkbox-state cross-device mirror
// (#1360): one row per document, holding that document's whole tick state and
// the time it was last put. Its own SQLite file, NOT chat_metadata: document
// keys are attachment blob ids — global, not conversation-scoped — so there is
// no agent+platform+chatID triple to key, and updated_ms (replay ordering and
// the age sweeps) has no place in a metadata value. See docs/WIRING.md →
// cross-device mirrors.
//
// The hub process is the file's only writer; the mutex serialises each put's
// cap check with its write so two concurrent puts of NEW keys cannot both pass
// a count taken before the other's insert. All methods are nil-receiver-safe
// (the frameStore idiom): a hub with no store (no data_dir, or the open
// failed) degrades to a live-only mirror.
type checkboxStore struct {
	db *sql.DB
	mu sync.Mutex

	// maxDocs is the resolved document cap: defaultCheckboxDocCap from the
	// constructor, overridden by newHub from [platforms.app].checkbox_max_docs
	// (the blobStore maxBytes idiom).
	maxDocs int
}

// newCheckboxStore opens (or creates) the checkbox-state DB.
func newCheckboxStore(path string) (*checkboxStore, error) {
	db, err := sqlite.OpenInit(path,
		`CREATE TABLE IF NOT EXISTS app_checkboxes (
			doc_key    TEXT    NOT NULL PRIMARY KEY,
			state      TEXT    NOT NULL,
			updated_ms INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_app_checkboxes_updated ON app_checkboxes(updated_ms)`,
	)
	if err != nil {
		return nil, err
	}
	return &checkboxStore{db: db, maxDocs: defaultCheckboxDocCap}, nil
}

// put stores one document's whole state, replacing any previous one
// (last-write-wins by arrival). An empty state deletes the stored document
// instead of storing an empty row — a clear is never cap-blocked and leaves
// nothing for a later replay to send. nowMs is a parameter (the frameStore
// Append idiom) so tests control the ordering the replay and sweeps read.
//
// Whether a document exists is decided by ROW PRESENCE, never by decoding its
// stored state: a corrupt row still counts as existing, so its update REPLACES
// it rather than being refused at the cap (#1360 review finding B).
func (s *checkboxStore) put(key, state string, nowMs int64) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == "" {
		_, err := s.db.Exec(`DELETE FROM app_checkboxes WHERE doc_key = ?`, key)
		return err
	}
	err := s.db.QueryRow(`SELECT 1 FROM app_checkboxes WHERE doc_key = ?`, key).Scan(new(int))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A document the store has never seen: subject to the cap.
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM app_checkboxes`).Scan(&n); err != nil {
			return err
		}
		if n >= s.maxDocs {
			return errCheckboxCap
		}
	case err != nil:
		return err
	}
	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO app_checkboxes (doc_key, state, updated_ms) VALUES (?, ?, ?)`,
		key, state, nowMs,
	)
	return err
}

// checkboxDoc is one stored document read back for the hello replay.
type checkboxDoc struct {
	key       string
	state     string
	updatedMs int64
}

// replay returns up to limit documents updated at or after minUpdatedMs, most
// recently updated first. A corrupt row (a state that does not decode or
// breaks the put limits) is the caller's to skip (finding B), and it still
// spends one of the SQL limit's slots — accepted: corrupt rows are rare (no
// writer of ours can produce one), the sweep eventually removes them by age,
// and filtering validity in SQL would duplicate the state encoding here. No
// retry logic for the same reason.
func (s *checkboxStore) replay(limit int, minUpdatedMs int64) []checkboxDoc {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT doc_key, state, updated_ms FROM app_checkboxes WHERE updated_ms >= ? ORDER BY updated_ms DESC LIMIT ?`,
		minUpdatedMs, limit,
	)
	if err != nil {
		appLog.Errorf("checkbox store replay: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []checkboxDoc
	for rows.Next() {
		var d checkboxDoc
		if err := rows.Scan(&d.key, &d.state, &d.updatedMs); err != nil {
			appLog.Errorf("checkbox store replay scan: %v", err)
			return out
		}
		out = append(out, d)
	}
	return out
}

// removeOlderThan deletes documents not updated since cutoffMs and returns how
// many went. Run once per hub start (StartAll); nil-safe.
func (s *checkboxStore) removeOlderThan(cutoffMs int64) int64 {
	if s == nil {
		return 0
	}
	res, err := s.db.Exec(`DELETE FROM app_checkboxes WHERE updated_ms < ?`, cutoffMs)
	if err != nil {
		appLog.Errorf("checkbox store sweep: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// Close closes the DB. nil-safe.
func (s *checkboxStore) Close() {
	if s == nil {
		return
	}
	if err := s.db.Close(); err != nil {
		appLog.Errorf("checkbox store close: %v", err)
	}
}
