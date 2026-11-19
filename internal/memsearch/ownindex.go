package memsearch

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/sqlite"
)

var ownLog = log.NewComponentLogger("own-memory")

// Index is the own-material index (#3185): which lines each session family
// wrote or read through its tools, and when each family's root was
// compacted. A family is a root session and every session under it (its
// branches: reflection, consolidation, /branch; Dick's ruling Q118: "count for
// the root session").
//
// Dick's rulings (2026-11-19): exclude what "belongs to the same session
// within the last 3 compactions" (Q117, N = [jit_memory] dedupe_compactions);
// a memory-file hit is the family's own when 2 of its 3 probe lines are in
// the family's line set (12:40, "Sure, sounds good").
//
// It is fed from the tool events every backend emits (Observe): the
// delegated backends' OnToolStart/OnToolEnd (claude-code, codex, opencode)
// and the API transport's tool loop. Lines are recorded under the family's
// compaction count at the time, so the window is a count, and a line older
// than the window is pruned at the next compaction.
//
// Writes go through one goroutine in batches, so a tool event never waits on
// the database.
type Index struct {
	db *sql.DB

	mu   sync.Mutex
	gens map[string]int // family -> compaction count, loaded on first use

	// sendMu guards ch against Close: Observe sends under the read lock,
	// Close closes ch under the write lock, so a tool event that races the
	// gateway's shutdown is dropped instead of sending on a closed channel.
	sendMu  sync.RWMutex
	closed  bool
	ch      chan pendingLines
	done    chan struct{}
	flushCh chan chan struct{}
	dropped sync.Once
}

type pendingLines struct {
	family string
	gen    int
	hashes []int64
	at     int64
}

var ownSchema = []string{
	`CREATE TABLE IF NOT EXISTS families (
		family TEXT PRIMARY KEY,
		gen INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS compactions (
		family TEXT NOT NULL,
		gen INTEGER NOT NULL,
		at INTEGER NOT NULL,
		PRIMARY KEY (family, gen)
	)`,
	`CREATE TABLE IF NOT EXISTS lines (
		family TEXT NOT NULL,
		hash INTEGER NOT NULL,
		gen INTEGER NOT NULL,
		at INTEGER NOT NULL,
		PRIMARY KEY (family, hash, gen)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS lines_at ON lines(at)`,
}

// lineRetention bounds a line's life when its family is never compacted
// again (an idle chat, an independent session): transcripts and
// conversation rows are kept 30 days, so nothing older can be "recent".
const lineRetention = 30 * 24 * time.Hour

// OpenIndex opens (creating) the index database at path and starts its
// writer.
func OpenIndex(path string) (*Index, error) {
	db, err := sqlite.OpenInit(path, ownSchema...)
	if err != nil {
		return nil, fmt.Errorf("open own-memory index %s: %w", path, err)
	}
	x := &Index{db: db, gens: map[string]int{}, ch: make(chan pendingLines, 4096),
		done: make(chan struct{}), flushCh: make(chan chan struct{})}
	x.pruneOld()
	go x.writer()
	return x, nil
}

// Close stops the writer (after it writes what is queued) and closes the
// database.
func (x *Index) Close() error {
	if x == nil {
		return nil
	}
	x.sendMu.Lock()
	if x.closed {
		x.sendMu.Unlock()
		return nil
	}
	x.closed = true
	close(x.ch)
	x.sendMu.Unlock()
	<-x.done
	return x.db.Close()
}

// Family is the family a session key belongs to: its root session.
func Family(sessionKey string) string {
	k, err := session.ParseSessionKey(sessionKey)
	if err != nil {
		return sessionKey
	}
	return k.Root().String()
}

// genLocked returns family's compaction count, loading it once.
func (x *Index) genLocked(family string) int {
	if g, ok := x.gens[family]; ok {
		return g
	}
	g := 0
	if err := x.db.QueryRow(`SELECT gen FROM families WHERE family = ?`, family).Scan(&g); err != nil && err != sql.ErrNoRows {
		ownLog.Warnf("family=%s load compaction count: %v", family, err)
	}
	x.gens[family] = g
	return g
}

// NoteToolIO logs, once at startup, that agentID's backend does not report
// every tool's full input and output (its tool_io capability is level,
// "partial" or "no", #3187 Q127): the index holds only part of that agent's
// own material, so a memory hit it wrote by an unreported tool still reads
// as not its own. Nil-safe.
func (x *Index) NoteToolIO(agentID, backend, level, reason string) {
	if x == nil {
		return
	}
	ownLog.Infof("agent=%s backend=%s tool_io=%s: the own-material index sees only part of this agent's tools (%s)", agentID, backend, level, reason)
}

// Observe records the lines of one tool input or result of sessionKey (see
// ExtractLines) under its family's current compaction count. Nil-safe; never
// blocks: when the writer is far behind the lines are dropped (one WARN per
// process).
func (x *Index) Observe(sessionKey, text string) {
	if x == nil || strings.TrimSpace(text) == "" {
		return
	}
	lines := ExtractLines(text)
	if len(lines) == 0 {
		return
	}
	hashes := make([]int64, len(lines))
	for i, l := range lines {
		hashes[i] = HashLine(l)
	}
	family := Family(sessionKey)
	x.mu.Lock()
	gen := x.genLocked(family)
	x.mu.Unlock()
	x.sendMu.RLock()
	defer x.sendMu.RUnlock()
	if x.closed {
		return
	}
	select {
	case x.ch <- pendingLines{family: family, gen: gen, hashes: hashes, at: time.Now().Unix()}:
	default:
		x.dropped.Do(func() {
			ownLog.Warnf("writer queue full: tool lines of %s (and later ones while full) not indexed", family)
		})
	}
}

// writer writes queued lines in batches, one transaction per batch.
func (x *Index) writer() {
	defer close(x.done)
	for {
		select {
		case p, ok := <-x.ch:
			if !ok {
				return
			}
			batch := []pendingLines{p}
		drain:
			for len(batch) < 256 {
				select {
				case q, ok := <-x.ch:
					if !ok {
						break drain
					}
					batch = append(batch, q)
				default:
					break drain
				}
			}
			x.write(batch)
		case ack := <-x.flushCh:
			// Everything sent before Flush was called is either written or
			// still in the channel: drain it, then ack.
			var batch []pendingLines
		rest:
			for {
				select {
				case q, ok := <-x.ch:
					if !ok {
						break rest
					}
					batch = append(batch, q)
				default:
					break rest
				}
			}
			if len(batch) > 0 {
				x.write(batch)
			}
			close(ack)
		}
	}
}

func (x *Index) write(batch []pendingLines) {
	tx, err := x.db.Begin()
	if err != nil {
		ownLog.Warnf("index tool lines: %v", err)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO lines (family, hash, gen, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(family, hash, gen) DO UPDATE SET at = excluded.at`)
	if err != nil {
		_ = tx.Rollback()
		ownLog.Warnf("index tool lines: %v", err)
		return
	}
	for _, p := range batch {
		for _, h := range p.hashes {
			if _, err := stmt.Exec(p.family, h, p.gen, p.at); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				ownLog.Warnf("index tool lines of %s: %v", p.family, err)
				return
			}
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		ownLog.Warnf("index tool lines: %v", err)
	}
}

// Flush waits until every line observed before the call is written. For
// tests and for a caller that must see its own writes.
func (x *Index) Flush() {
	if x == nil {
		return
	}
	x.sendMu.RLock()
	if x.closed {
		x.sendMu.RUnlock()
		return
	}
	ack := make(chan struct{})
	x.flushCh <- ack
	x.sendMu.RUnlock()
	<-ack
}

// NoteCompaction records that sessionKey's family root was compacted now:
// its count goes up by one, the time is kept (the window's start for
// conversation rows), and lines older than keep compactions are pruned
// (keep <= 0 prunes nothing). Nil-safe.
func (x *Index) NoteCompaction(sessionKey string, keep int) {
	if x == nil {
		return
	}
	family := Family(sessionKey)
	x.mu.Lock()
	gen := x.genLocked(family) + 1
	x.gens[family] = gen
	x.mu.Unlock()
	now := time.Now().Unix()
	if _, err := x.db.Exec(`INSERT INTO families (family, gen) VALUES (?, ?)
		ON CONFLICT(family) DO UPDATE SET gen = excluded.gen`, family, gen); err != nil {
		ownLog.Warnf("family=%s record compaction: %v", family, err)
	}
	if _, err := x.db.Exec(`INSERT OR REPLACE INTO compactions (family, gen, at) VALUES (?, ?, ?)`, family, gen, now); err != nil {
		ownLog.Warnf("family=%s record compaction time: %v", family, err)
	}
	if keep > 0 {
		if res, err := x.db.Exec(`DELETE FROM lines WHERE family = ? AND gen <= ?`, family, gen-keep); err != nil {
			ownLog.Warnf("family=%s prune lines: %v", family, err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			ownLog.Debugf("family=%s compaction %d: pruned %d lines older than %d compactions", family, gen, n, keep)
		}
	}
	x.pruneOld()
}

// pruneOld deletes lines past lineRetention.
func (x *Index) pruneOld() {
	if _, err := x.db.Exec(`DELETE FROM lines WHERE at < ?`, time.Now().Add(-lineRetention).Unix()); err != nil {
		ownLog.Warnf("prune old lines: %v", err)
	}
}

// Window is a family's own-material window: the last N compactions of its
// root.
type Window struct {
	Family string
	N      int // the window size asked for; 0 = no window (nothing is own)
	Gen    int // the root's compaction count now
	// MinGen is the oldest compaction count whose lines are own: Gen-N+1.
	MinGen int
	// Since is when compaction MinGen happened: conversation rows at or
	// after it are own. Zero = every row of the family is own (fewer than N
	// compactions recorded).
	Since time.Time
}

// Active reports whether the window excludes anything.
func (w Window) Active() bool { return w.N > 0 && w.Family != "" }

// Own reports whether a conversation row of session at t is own material.
// A row with no time is treated as own (it cannot be placed outside).
func (w Window) Own(session string, t time.Time) bool {
	if !w.Active() || Family(session) != w.Family {
		return false
	}
	return w.Since.IsZero() || t.IsZero() || !t.Before(w.Since)
}

// Window returns sessionKey's family window of n compactions. Nil-safe (a
// nil Index has no window).
func (x *Index) Window(sessionKey string, n int) Window {
	if x == nil || sessionKey == "" || n <= 0 {
		return Window{}
	}
	family := Family(sessionKey)
	x.mu.Lock()
	gen := x.genLocked(family)
	x.mu.Unlock()
	w := Window{Family: family, N: n, Gen: gen, MinGen: gen - n + 1}
	if w.MinGen > 0 {
		var at int64
		err := x.db.QueryRow(`SELECT at FROM compactions WHERE family = ? AND gen = ?`, family, w.MinGen).Scan(&at)
		if err == nil {
			w.Since = time.Unix(at, 0)
		} else if err != sql.ErrNoRows {
			ownLog.Warnf("family=%s read compaction %d: %v", family, w.MinGen, err)
		}
	}
	return w
}

// Matches returns how many of hashes the window's family recorded at a
// compaction count of at least w.MinGen.
func (x *Index) Matches(w Window, hashes []int64) (int, error) {
	if x == nil || !w.Active() || len(hashes) == 0 {
		return 0, nil
	}
	args := []any{w.Family, w.MinGen}
	ph := make([]string, len(hashes))
	for i, h := range hashes {
		ph[i] = "?"
		args = append(args, h)
	}
	var n int
	err := x.db.QueryRow(`SELECT COUNT(DISTINCT hash) FROM lines WHERE family = ? AND gen >= ? AND hash IN (`+
		strings.Join(ph, ",")+`)`, args...).Scan(&n)
	return n, err
}
