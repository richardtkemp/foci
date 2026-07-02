package app

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/log"
	"foci/internal/sqlite"
)

// frameStore is the durable, content-bearing backstop for the per-conversation
// in-memory replay buffer (convBinding.buffer). Every server→app frame is also
// persisted here verbatim as its encoded wire blob, keyed (conv_id, seq). This
// closes the eventual-consistency gap the in-memory buffer cannot: it survives a
// server restart (every update.sh deploy) and retains frames far longer than the
// in-memory depth/TTL bound, so a long-offline phone can backfill what it missed
// via reconnect replay (replayTo) and the GET /app/replay endpoint.
//
// Two properties fall out of persisting frames here:
//   - Persistent seqs. On binding (re)creation the hub seeds b.seq from MaxSeq,
//     so the per-conversation seq counter no longer resets to 0 on restart. That
//     removes the renumbering ambiguity that would otherwise make a reconnecting
//     client drop the fresh low-seq stream as stale.
//   - Byte-faithful replay. The stored value is the exact wire the live path
//     sent (fap.Encode output), so replay re-emits it unchanged.
//
// Writes are async (a single writer goroutine drains a buffered channel) to keep
// the send hot path latency-free; the in-memory buffer already guarantees
// immediate replay, so the durable write may lag a few ms. Close() drains the
// queue, so a graceful shutdown (SIGTERM — what update.sh does) loses nothing; a
// hard crash can lose only the last few in-flight frames (bounded; the offline
// push + history reconcile cover the worst case).
type frameStore struct {
	db             *sql.DB
	ttl            time.Duration
	writeCh        chan frameWrite
	done           chan struct{}
	wg             sync.WaitGroup
	lastInlineWarn atomic.Int64 // unix nanos of the last saturation warn (rate-limit)
}

// frameWrite is one pending durable write. Normally an append; when purge is
// set it deletes all frames for convID instead. Purges go through the same FIFO
// queue as appends so a delete can't race ahead of appends still queued for the
// conv (which would re-insert after the delete and resurrect an archived conv).
type frameWrite struct {
	convID  string
	agentID string
	seq     int64
	wire    string
	sentMs  int64
	visible bool
	preview string
	purge   bool
	done    chan int64 // purge only: receives rows deleted
}

// storedFrame is one frame read back for replay/backfill.
type storedFrame struct {
	seq  int64
	wire string
}

const frameWriteQueue = 1024 // async write backlog before Append falls back to sync

// newFrameStore opens (or creates) the durable frame DB and starts its writer.
func newFrameStore(path string, ttl time.Duration) (*frameStore, error) {
	db, err := sqlite.OpenInit(path,
		`CREATE TABLE IF NOT EXISTS app_frames (
			conv_id  TEXT    NOT NULL,
			seq      INTEGER NOT NULL,
			wire     TEXT    NOT NULL,
			sent_ms  INTEGER NOT NULL,
			visible  INTEGER NOT NULL DEFAULT 1,
			agent_id TEXT    NOT NULL DEFAULT '',
			preview  TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_app_frames_sent ON app_frames(sent_ms)`,
		// Durable promptID→conversation index so a resolution can always emit a
		// resolve frame (even after a restart wipes the in-memory prompt registry),
		// making ask resolution survive replay. Row removed on resolve.
		`CREATE TABLE IF NOT EXISTS app_prompts (
			prompt_id  TEXT    NOT NULL PRIMARY KEY,
			conv_id    TEXT    NOT NULL,
			agent_id   TEXT    NOT NULL,
			created_ms INTEGER NOT NULL
		)`,
		`PRAGMA auto_vacuum = INCREMENTAL`,
	)
	if err != nil {
		return nil, err
	}
	// Migrate an existing DB to carry agent_id (which conv belongs to which agent —
	// needed to rebuild bindings at startup) and preview (the visible frame's roster
	// preview, so a restart can seed the roster snapshot). Errors ignored: a
	// "duplicate column" means it is already present (the ALTER-ADD-COLUMN idiom).
	_, _ = db.Exec(`ALTER TABLE app_frames ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE app_frames ADD COLUMN preview TEXT NOT NULL DEFAULT ''`)
	s := &frameStore{
		db:      db,
		ttl:     ttl,
		writeCh: make(chan frameWrite, frameWriteQueue),
		done:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// Append durably persists one sent frame. Non-blocking in the common case; if the
// writer is saturated it writes synchronously rather than drop (durability beats
// latency here). nil receiver is a no-op so a store-less hub (no data_dir) is safe.
func (s *frameStore) Append(convID, agentID string, seq int64, wire string, sentMs int64, visible bool, preview string) {
	if s == nil {
		return
	}
	w := frameWrite{convID: convID, agentID: agentID, seq: seq, wire: wire, sentMs: sentMs, visible: visible, preview: preview}
	select {
	case s.writeCh <- w:
	default:
		// Queue full: write inline so nothing is lost. This means a second
		// goroutine now writes concurrently with the writer — safe (pool-wide
		// busy_timeout), but a sign frames are outpacing the DB. Warn, rate-
		// limited to once/5s so a burst doesn't flood the log.
		now := time.Now().UnixNano()
		if last := s.lastInlineWarn.Load(); now-last > int64(5*time.Second) && s.lastInlineWarn.CompareAndSwap(last, now) {
			log.Warnf("app", "frame store write queue saturated (cap=%d) — writing inline; frames outpacing DB drain", cap(s.writeCh))
		}
		s.insert(w)
	}
}

// writer drains the queue until Close, then flushes whatever remains.
func (s *frameStore) writer() {
	defer s.wg.Done()
	for {
		select {
		case w := <-s.writeCh:
			s.apply(w)
		case <-s.done:
			for {
				select {
				case w := <-s.writeCh:
					s.apply(w)
				default:
					return
				}
			}
		}
	}
}

// apply dispatches a queued write to insert or purge.
func (s *frameStore) apply(w frameWrite) {
	if w.purge {
		res, err := s.db.Exec(`DELETE FROM app_frames WHERE conv_id = ?`, w.convID)
		if err != nil {
			log.Errorf("app", "frame store PurgeConv (conv=%s): %v", w.convID, err)
			w.done <- 0
			return
		}
		n, _ := res.RowsAffected()
		w.done <- n
		return
	}
	s.insert(w)
}

// insert writes one frame. INSERT OR REPLACE keeps the latest wire for a
// (conv_id, seq) — defensive against a seq reused across an edge-case rehydrate.
func (s *frameStore) insert(w frameWrite) {
	v := 0
	if w.visible {
		v = 1
	}
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO app_frames (conv_id, seq, wire, sent_ms, visible, agent_id, preview) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.convID, w.seq, w.wire, w.sentMs, v, w.agentID, w.preview,
	); err != nil {
		log.Errorf("app", "frame store insert (conv=%s seq=%d): %v", w.convID, w.seq, err)
	}
}

// restorableConv identifies a conversation to rebuild a binding for at startup.
type restorableConv struct {
	convID  string
	agentID string
}

// RestorableConvs returns the conversations that should have their bindings
// rebuilt at startup: those with at least one VISIBLE frame (a real message, not
// transient typing) and a known agent_id. Archived conversations have had their
// frames purged (PurgeConv), so presence here IS the "non-archived & active"
// signal — no archived flag needed (#app-binding-restore). Legacy rows with an
// empty agent_id (pre-migration) are skipped: their binding can't be rebuilt
// without the agent, and they recover lazily on the app's next open.
func (s *frameStore) RestorableConvs() []restorableConv {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT conv_id, agent_id FROM app_frames WHERE visible = 1 AND agent_id <> ''`,
	)
	if err != nil {
		log.Errorf("app", "frame store RestorableConvs: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []restorableConv
	for rows.Next() {
		var c restorableConv
		if err := rows.Scan(&c.convID, &c.agentID); err != nil {
			log.Errorf("app", "frame store RestorableConvs scan: %v", err)
			return out
		}
		out = append(out, c)
	}
	return out
}

// PurgeConv deletes every frame for a conversation. Called when a session is
// archived: removing its durable frames is what excludes it from the startup
// binding restore (delete-on-archive — presence of frames is the restore signal,
// so there is no archived flag to store). Returns rows removed.
func (s *frameStore) PurgeConv(convID string) int64 {
	if s == nil {
		return 0
	}
	done := make(chan int64, 1)
	s.writeCh <- frameWrite{convID: convID, purge: true, done: done}
	return <-done
}

// MaxSeq returns the highest persisted seq for a conversation (0 if none). Used
// to rehydrate b.seq on binding creation so seqs survive a restart. Safe to call
// at binding creation: the prior process drained its writes on shutdown, and a
// new process issues no sends for the conversation before the binding exists.
func (s *frameStore) MaxSeq(convID string) int64 {
	if s == nil {
		return 0
	}
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM app_frames WHERE conv_id = ?`, convID).Scan(&seq); err != nil {
		log.Errorf("app", "frame store MaxSeq (conv=%s): %v", convID, err)
		return 0
	}
	if seq.Valid {
		return seq.Int64
	}
	return 0
}

// LastVisible returns the newest user-visible frame's roster preview and send
// time (ms) for convID — used to seed the roster's last-activity snapshot after a
// restart. ok is false when the conversation has no stored visible frame.
func (s *frameStore) LastVisible(convID string) (preview string, sentMs int64, ok bool) {
	if s == nil {
		return "", 0, false
	}
	err := s.db.QueryRow(
		`SELECT preview, sent_ms FROM app_frames WHERE conv_id = ? AND visible = 1 ORDER BY seq DESC LIMIT 1`,
		convID,
	).Scan(&preview, &sentMs)
	if err != nil {
		return "", 0, false
	}
	return preview, sentMs, true
}

// Range returns up to limit frames with seq > fromSeq, in ascending seq order —
// the backfill source for replayTo and GET /app/replay.
func (s *frameStore) Range(convID string, fromSeq int64, limit int) []storedFrame {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT seq, wire FROM app_frames WHERE conv_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		convID, fromSeq, limit,
	)
	if err != nil {
		log.Errorf("app", "frame store Range (conv=%s from=%d): %v", convID, fromSeq, err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []storedFrame
	for rows.Next() {
		var f storedFrame
		if err := rows.Scan(&f.seq, &f.wire); err != nil {
			log.Errorf("app", "frame store Range scan: %v", err)
			return out
		}
		out = append(out, f)
	}
	return out
}

// PutPrompt records a live app prompt's conversation so a resolution can find it
// even after a restart wipes the in-memory registry. createdMs bounds cleanup.
func (s *frameStore) PutPrompt(promptID, convID, agentID string, createdMs int64) {
	if s == nil {
		return
	}
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO app_prompts (prompt_id, conv_id, agent_id, created_ms) VALUES (?, ?, ?, ?)`,
		promptID, convID, agentID, createdMs,
	); err != nil {
		log.Errorf("app", "frame store PutPrompt (prompt=%s): %v", promptID, err)
	}
}

// PromptConv returns the conversation + agent a still-open prompt belongs to.
func (s *frameStore) PromptConv(promptID string) (convID, agentID string, ok bool) {
	if s == nil {
		return "", "", false
	}
	if err := s.db.QueryRow(
		`SELECT conv_id, agent_id FROM app_prompts WHERE prompt_id = ?`, promptID,
	).Scan(&convID, &agentID); err != nil {
		return "", "", false
	}
	return convID, agentID, true
}

// DeletePrompt drops a resolved prompt from the index.
func (s *frameStore) DeletePrompt(promptID string) {
	if s == nil {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM app_prompts WHERE prompt_id = ?`, promptID); err != nil {
		log.Errorf("app", "frame store DeletePrompt (prompt=%s): %v", promptID, err)
	}
}

// TrimOlderThan deletes frames older than cutoffMs and returns the rows removed.
func (s *frameStore) TrimOlderThan(cutoffMs int64) int64 {
	if s == nil {
		return 0
	}
	res, err := s.db.Exec(`DELETE FROM app_frames WHERE sent_ms < ?`, cutoffMs)
	if err != nil {
		log.Errorf("app", "frame store trim: %v", err)
		return 0
	}
	_, _ = s.db.Exec(`DELETE FROM app_prompts WHERE created_ms < ?`, cutoffMs)
	n, _ := res.RowsAffected()
	if n > 0 {
		// Reclaim freed pages incrementally (auto_vacuum=INCREMENTAL).
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum`)
	}
	return n
}

// janitor periodically trims frames past the TTL until ctx is cancelled.
func (s *frameStore) janitor(done <-chan struct{}) {
	if s == nil || s.ttl <= 0 {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-s.ttl).UnixMilli()
			if n := s.TrimOlderThan(cutoff); n > 0 {
				log.Infof("app", "frame store: trimmed %d frame(s) older than %s", n, s.ttl)
			}
		}
	}
}

// Close stops the writer, flushes pending writes, and closes the DB.
func (s *frameStore) Close() {
	if s == nil {
		return
	}
	close(s.done)
	s.wg.Wait()
	if err := s.db.Close(); err != nil {
		log.Errorf("app", "frame store close: %v", err)
	}
}
