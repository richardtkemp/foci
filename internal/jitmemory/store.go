package jitmemory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/sqlite"
)

// Store is the JIT memory database (jit_memory.db): the evaluation log of
// every run (Dick's requirement 11: how often it fires and injects, and
// whether the agent used or already knew the injected notes), and each
// session's dedupe set, so a restart does not offer the same notes again.
type Store struct {
	db *sql.DB
}

// storeSchema creates the tables. runs.hits is a JSON array of Hit (path,
// section, the query that found it, its rank, the file's mtime, the verdict);
// runs.calls a JSON array of the model calls (kind, model, tokens, ms, cost).
var storeSchema = []string{
	`CREATE TABLE IF NOT EXISTS runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		session_key TEXT NOT NULL,
		role TEXT NOT NULL,
		text_chars INTEGER NOT NULL,
		text_head TEXT NOT NULL,
		context_messages INTEGER NOT NULL DEFAULT 0,
		strategy TEXT NOT NULL DEFAULT '',
		queries TEXT NOT NULL DEFAULT '[]',
		hits TEXT NOT NULL DEFAULT '[]',
		judge_answer TEXT NOT NULL DEFAULT '',
		injection TEXT NOT NULL DEFAULT '',
		injected_chars INTEGER NOT NULL DEFAULT 0,
		delivery TEXT NOT NULL DEFAULT '',
		skipped TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		calls TEXT NOT NULL DEFAULT '[]',
		cost_usd REAL NOT NULL DEFAULT 0,
		search_ms INTEGER NOT NULL DEFAULT 0,
		total_ms INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS runs_session ON runs(session_key, ts)`,
	`CREATE TABLE IF NOT EXISTS seen (
		session_key TEXT NOT NULL,
		hit_key TEXT NOT NULL,
		generation INTEGER NOT NULL,
		PRIMARY KEY (session_key, hit_key)
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		session_key TEXT PRIMARY KEY,
		generation INTEGER NOT NULL
	)`,
}

// OpenStore opens (creating) the database at path.
func OpenStore(path string) (*Store, error) {
	db, err := sqlite.OpenInit(path, storeSchema...)
	if err != nil {
		return nil, fmt.Errorf("open jit memory store %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Call is one model call of a run.
type Call struct {
	Kind         string  `json:"kind"` // query | judge
	Model        string  `json:"model,omitempty"`
	InputTokens  int     `json:"in,omitempty"`
	OutputTokens int     `json:"out,omitempty"`
	MS           int64   `json:"ms"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Error        string  `json:"error,omitempty"`
}

// RunRecord is one run as logged.
type RunRecord struct {
	Time            time.Time
	AgentID         string
	SessionKey      string
	Role            string
	TextChars       int
	TextHead        string
	ContextMessages int
	Strategy        string
	Queries         []string
	Hits            []Hit
	JudgeAnswer     string
	Injection       string
	Skipped         string
	Error           string
	Calls           []Call
	CostUSD         float64
	SearchMS        int64
	TotalMS         int64
}

func (r *RunRecord) addCall(kind string, rep CallReport, err error) {
	c := Call{Kind: kind, Model: rep.Model, InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens,
		MS: rep.Duration.Milliseconds(), CostUSD: rep.CostUSD}
	if err != nil {
		c.Error = err.Error()
	}
	r.Calls = append(r.Calls, c)
	r.CostUSD += rep.CostUSD
}

// LogRun records r and returns its row id.
func (s *Store) LogRun(r *RunRecord) (int64, error) {
	q, _ := json.Marshal(nonNil(r.Queries))
	h, _ := json.Marshal(r.Hits)
	if r.Hits == nil {
		h = []byte("[]")
	}
	c, _ := json.Marshal(r.Calls)
	if r.Calls == nil {
		c = []byte("[]")
	}
	res, err := s.db.Exec(`INSERT INTO runs (ts, agent_id, session_key, role, text_chars, text_head, context_messages,
		strategy, queries, hits, judge_answer, injection, injected_chars, skipped, error, calls, cost_usd, search_ms, total_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.UTC().Format(time.RFC3339Nano), r.AgentID, r.SessionKey, r.Role, r.TextChars, r.TextHead, r.ContextMessages,
		r.Strategy, string(q), string(h), r.JudgeAnswer, r.Injection, len([]rune(r.Injection)), r.Skipped, r.Error,
		string(c), r.CostUSD, r.SearchMS, r.TotalMS)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Delivery modes (runs.delivery).
const (
	DeliveryAttached     = "attached"      // attached to the user's message in time
	DeliveryInjected     = "injected"      // sent to the agent (an agent message, or a late user-message result)
	DeliveryLateDropped  = "late_dropped"  // late, on a backend that cannot take it mid-turn
	DeliveryLateInjected = "late_injected" // late user-message result, sent to the agent
)

// SetDelivery records how run id's injection reached the agent.
func (s *Store) SetDelivery(id int64, mode string) error {
	if id == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE runs SET delivery = ? WHERE id = ?`, mode, id)
	return err
}

// LoadSession returns sk's compaction count and dedupe set.
func (s *Store) LoadSession(sk string) (int, map[string]int, error) {
	gen := 0
	err := s.db.QueryRow(`SELECT generation FROM sessions WHERE session_key = ?`, sk).Scan(&gen)
	if err != nil && err != sql.ErrNoRows {
		return 0, nil, err
	}
	rows, err := s.db.Query(`SELECT hit_key, generation FROM seen WHERE session_key = ?`, sk)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]int{}
	for rows.Next() {
		var k string
		var g int
		if err := rows.Scan(&k, &g); err != nil {
			return 0, nil, err
		}
		seen[k] = g
	}
	return gen, seen, rows.Err()
}

// SetGeneration records sk's compaction count.
func (s *Store) SetGeneration(sk string, gen int) error {
	_, err := s.db.Exec(`INSERT INTO sessions (session_key, generation) VALUES (?, ?)
		ON CONFLICT(session_key) DO UPDATE SET generation = excluded.generation`, sk, gen)
	return err
}

// MarkSeen records that keys went to sk's judge at compaction count gen.
func (s *Store) MarkSeen(sk string, keys []string, gen int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT INTO seen (session_key, hit_key, generation) VALUES (?, ?, ?)
			ON CONFLICT(session_key, hit_key) DO UPDATE SET generation = excluded.generation`, sk, k, gen); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
