package log

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

func TestAPIDB(t *testing.T) {
	// Verifies the SQLite API DB stores entries with all fields including
	// call_type, session_file, and that session-based queries work correctly.
	dbPath := filepath.Join(t.TempDir(), "test_api.db")

	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// Insert entries of different call types
	entries := []APIEntry{
		{
			Timestamp:       time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			Session:         "main/c123",
			Model:           "claude-haiku-4-5",
			Input:           1000,
			Output:          200,
			CacheRead:       500,
			CacheWrite:      300,
			ProvidedCostUSD: f64p(0.005),
			DurationMS:      1200,
			StopReason:      "end_turn",
			CallType:        "conversation",
		},
		{
			Timestamp:       time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC),
			Session:         "main/c123",
			Model:           "claude-haiku-4-5",
			Input:           2000,
			Output:          400,
			ProvidedCostUSD: f64p(0.01),
			DurationMS:      2400,
			StopReason:      "end_turn",
			CallType:        "compaction",
		},
		{
			Timestamp:       time.Date(2026, 3, 1, 10, 2, 0, 0, time.UTC),
			Session:         "main/c123",
			Model:           "claude-haiku-4-5",
			Input:           500,
			Output:          100,
			ProvidedCostUSD: f64p(0.002),
			DurationMS:      800,
			StopReason:      "end_turn",
			CallType:        "summary",
		},
		{
			Timestamp:       time.Date(2026, 3, 1, 10, 3, 0, 0, time.UTC),
			Session:         "main/ispawn-456",
			Model:           "claude-sonnet-4-5",
			Input:           3000,
			Output:          600,
			ProvidedCostUSD: f64p(0.02),
			DurationMS:      3600,
			StopReason:      "end_turn",
			CallType:        "spawn",
			SessionFile:     "/data/sessions/agent/main/spawn/456.jsonl",
		},
	}

	for _, e := range entries {
		apiLog.insert(e)
	}

	// Query by call_type
	rows, err := apiLog.db.Query("SELECT call_type, count(*) FROM api_calls GROUP BY call_type ORDER BY call_type")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var ct string
		var n int
		if err := rows.Scan(&ct, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[ct] = n
	}

	if counts["conversation"] != 1 {
		t.Errorf("conversation count = %d, want 1", counts["conversation"])
	}
	if counts["compaction"] != 1 {
		t.Errorf("compaction count = %d, want 1", counts["compaction"])
	}
	if counts["summary"] != 1 {
		t.Errorf("summary count = %d, want 1", counts["summary"])
	}
	if counts["spawn"] != 1 {
		t.Errorf("spawn count = %d, want 1", counts["spawn"])
	}

	// Verify session_file was stored
	var sf sql.NullString
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'spawn'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if !sf.Valid || sf.String != "/data/sessions/agent/main/spawn/456.jsonl" {
		t.Errorf("session_file = %v, want /data/sessions/agent/main/spawn/456.jsonl", sf)
	}

	// Verify session_file is NULL for entries without it
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'conversation'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if sf.Valid {
		t.Errorf("session_file should be NULL for conversation, got %q", sf.String)
	}

	// Query by session index
	var total int
	err = apiLog.db.QueryRow("SELECT count(*) FROM api_calls WHERE session = 'main/c123'").Scan(&total)
	if err != nil {
		t.Fatalf("query by session: %v", err)
	}
	if total != 3 {
		t.Errorf("session count = %d, want 3", total)
	}
}

func TestReadAPIDBLog(t *testing.T) {
	// Verifies ReadAPIDBLog returns all rows in chronological order with fields
	// mapped, and that the timestamp round-trips through the RFC3339 storage.
	// This is the durable source the /cost command reads so it survives restarts
	// (api.jsonl is reset on each restart).
	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// Insert out of order to confirm ORDER BY ts ASC.
	t1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)
	apiLog.insert(APIEntry{Timestamp: t2, Session: "s/c/2", Model: "m", Input: 20, Output: 4, ProvidedCostUSD: f64p(0.02), CallType: "delegated_turn"})
	apiLog.insert(APIEntry{Timestamp: t1, Session: "s/c/1", Model: "m", Input: 10, Output: 2, CacheRead: 5, ProvidedCostUSD: f64p(0.01), CallType: "conversation"})

	got := ReadAPIDBLog()
	if len(got) != 2 {
		t.Fatalf("ReadAPIDBLog len = %d, want 2", len(got))
	}
	// Chronological order: t1 first.
	if !got[0].Timestamp.Equal(t1) {
		t.Errorf("entry[0].Timestamp = %v, want %v", got[0].Timestamp, t1)
	}
	if !got[1].Timestamp.Equal(t2) {
		t.Errorf("entry[1].Timestamp = %v, want %v", got[1].Timestamp, t2)
	}
	if got[0].Session != "s/c/1" || got[0].Input != 10 || got[0].CacheRead != 5 || got[0].ProvidedCostUSD == nil || *got[0].ProvidedCostUSD != 0.01 {
		t.Errorf("entry[0] fields mismatched: %+v", got[0])
	}
	if got[1].ProvidedCostUSD == nil || *got[1].ProvidedCostUSD != 0.02 || got[1].CallType != "delegated_turn" {
		t.Errorf("entry[1] fields mismatched: %+v", got[1])
	}
}

func TestReadAPIDBLogNilDB(t *testing.T) {
	// When the db is not initialised, ReadAPIDBLog returns nil so callers fall
	// back to the JSONL reader.
	old := apiLog
	apiLog = nil
	defer func() { apiLog = old }()

	if got := ReadAPIDBLog(); got != nil {
		t.Errorf("ReadAPIDBLog() with nil db = %v, want nil", got)
	}
}

func TestAPIDBDisabled(t *testing.T) {
	// Verifies that API() is a no-op (no panic) when no DB is initialized.
	old := apiLog
	apiLog = nil
	defer func() { apiLog = old }()

	API(APIEntry{Session: "test", CallType: "conversation"})
	// No panic = pass
}

func TestInitAPIDBError(t *testing.T) {
	// Verifies InitAPIDB returns an error for a path that can't be created.
	err := InitAPIDB("/nonexistent/deep/dir/api.db")
	if err == nil {
		CloseAPIDB()
		t.Fatal("expected error for bad DB path")
	}
}

func TestInsertError(t *testing.T) {
	// Verifies that insert logs an error (rather than panicking) when
	// the prepared statement has been closed.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}

	// Close the stmt to force an exec error
	apiLog.stmt.Close()

	apiLog.insert(APIEntry{
		Timestamp: time.Now(),
		Session:   "test",
		Model:     "test",
		CallType:  "conversation",
	})

	// Should have logged an error
	if !strings.Contains(buf.String(), "insert error") {
		t.Errorf("expected insert error log, got: %s", buf.String())
	}

	// Clean up — close DB (stmt already closed)
	apiLog.db.Close()
	apiLog = nil
}

func TestInsertSessionLineNullability(t *testing.T) {
	// Verifies that session_line is stored as NULL for 0
	// and as a non-NULL integer for positive values in the SQLite API log.
	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	apiLog.insert(APIEntry{
		Timestamp:   time.Now(),
		Session:     "test",
		Model:       "test",
		CallType:    "conversation",
		SessionLine: 42,
		SessionFile: "/test.jsonl",
	})

	var sl sql.NullInt64
	apiLog.db.QueryRow("SELECT session_line FROM api_calls WHERE session_line IS NOT NULL").Scan(&sl)
	if !sl.Valid || sl.Int64 != 42 {
		t.Errorf("session_line = %v, want 42", sl)
	}
}

// #1854: the turn-summed token columns must round-trip as a group, and stay
// NULL — not zero — when the writer measured no turn total. A zero would
// price as a free turn, which is a wrong answer; NULL is "not measured", which
// is the truth for pre-change rows and for backends that do not accumulate.
func TestAPIDB_TurnCountsRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	t1 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	turn := modelinfo.TokenCounts{Input: 10, Output: 815, CacheRead: 201000, CacheWrite: 41300}
	// Context fill deliberately differs from the turn total in every class, so
	// a scan that read the wrong four columns cannot pass by coincidence.
	apiLog.insert(APIEntry{Timestamp: t1, Session: "s/c/1", Model: "m",
		Input: 3, Output: 815, CacheRead: 121000, CacheWrite: 300,
		Turn: &turn, CallType: "delegated_turn"})
	apiLog.insert(APIEntry{Timestamp: t1.Add(time.Minute), Session: "s/c/1", Model: "m",
		Input: 3, Output: 5, CacheRead: 121000, CacheWrite: 300,
		CallType: "delegated_turn"}) // no Turn: backend measured none

	got := ReadAPIDBLog()
	if len(got) != 2 {
		t.Fatalf("ReadAPIDBLog len = %d, want 2", len(got))
	}
	if got[0].Turn == nil || *got[0].Turn != turn {
		t.Errorf("entry[0].Turn = %+v, want %+v", got[0].Turn, turn)
	}
	if got[0].Input != 3 || got[0].CacheRead != 121000 {
		t.Errorf("entry[0] context fill = in=%d cr=%d, want 3/121000 — the original columns must keep their meaning", got[0].Input, got[0].CacheRead)
	}
	if got[1].Turn != nil {
		t.Errorf("entry[1].Turn = %+v, want nil for a row written without a turn total", *got[1].Turn)
	}

	// The columns themselves: NULL, not 0, on the unmeasured row.
	var nulls int
	if err := apiLog.db.QueryRow(`SELECT COUNT(*) FROM api_calls WHERE turn_input_tokens IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Errorf("rows with NULL turn_input_tokens = %d, want 1", nulls)
	}

	// Output HAS a turn_ twin since #1891. It did not until #1866 P3 made
	// pricing cross-model: the three turn_ columns became all-model sums while
	// output_tokens stayed PARENT-ONLY, so on a turn with a subagent on another
	// model they stopped being the same number. Measured live: 27,305 output
	// tokens priced, 10,212 stored, and the #1854 re-price identity failed by
	// the difference.
	//
	// This block used to assert the OPPOSITE — that a disagreeing Turn.Output
	// is discarded and reads back as output_tokens, and that the column must
	// not exist at all. That was correct while the two figures were always
	// equal. It is kept as a rewrite rather than a deletion so the inversion is
	// visible in the history.
	apiLog.insert(APIEntry{Timestamp: t1.Add(2 * time.Minute), Session: "s/c/1", Model: "m",
		Input: 3, Output: 815, CacheRead: 121000, CacheWrite: 300,
		Turn:     &modelinfo.TokenCounts{Input: 10, Output: 999, CacheRead: 201000, CacheWrite: 41300},
		CallType: "delegated_turn"})
	got = ReadAPIDBLog()
	if len(got) != 3 || got[2].Turn == nil {
		t.Fatalf("entry[2].Turn missing: %+v", got)
	}
	if got[2].Turn.Output != 999 {
		t.Errorf("entry[2].Turn.Output = %d, want 999 — the CROSS-MODEL turn total, "+
			"not output_tokens' parent-only 815 (#1891)", got[2].Turn.Output)
	}
	if got[2].Output != 815 {
		t.Errorf("entry[2].Output = %d, want 815 — output_tokens keeps its own "+
			"parent-only meaning; the new column is additive", got[2].Output)
	}
	var hasOut int
	if err := apiLog.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('api_calls') WHERE name='turn_output_tokens'`).Scan(&hasOut); err != nil {
		t.Fatal(err)
	}
	if hasOut != 1 {
		t.Errorf("turn_output_tokens column missing — the DROP must be followed by the ADD (#1891)")
	}
}

// TestAPIDB_TurnOutputFallsBackForPre1891Rows: a row written before the column
// existed has NULL there, and must still re-price. Reading it as zero output
// would silently under-report every historical turn.
func TestAPIDB_TurnOutputFallsBackForPre1891Rows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fallback_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	t1 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	apiLog.insert(APIEntry{Timestamp: t1, Session: "s/c/1", Model: "m",
		Input: 3, Output: 815, CacheRead: 121000, CacheWrite: 300,
		Turn:     &modelinfo.TokenCounts{Input: 10, Output: 999, CacheRead: 201000, CacheWrite: 41300},
		CallType: "delegated_turn"})
	// Simulate a pre-#1891 row: the turn group is present, the output twin is not.
	if _, err := apiLog.db.Exec(`UPDATE api_calls SET turn_output_tokens = NULL`); err != nil {
		t.Fatal(err)
	}
	got := ReadAPIDBLog()
	if len(got) != 1 || got[0].Turn == nil {
		t.Fatalf("Turn missing: %+v", got)
	}
	if got[0].Turn.Output != 815 {
		t.Errorf("Turn.Output = %d, want 815 (output_tokens) — a NULL twin must fall "+
			"back, not read as zero", got[0].Turn.Output)
	}
}

// TestAPIDB_TurnIDAndAgentIDRoundTrip covers #1880 phase C's schema half: a
// turn is no longer always one row, so rows need a shared turn identity and a
// subagent row needs to name its subagent — plus #1946's split of that
// naming into two columns: agent_id (the OWNING agent, on every row) and
// subagent_id (the Agent tool_use id, subagent rows only).
//
// The NULL assertion is the load-bearing one. An empty turn_id stored as ""
// would split un-attributed rows into two populations — historical rows (NULL)
// and new ones ("") — so "WHERE turn_id IS NULL" would silently under-report
// and every caller would need to test for both.
func TestAPIDB_TurnIDAndAgentIDRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	t1 := time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)
	const turnID = "clutch/c123@1757585000000000000"

	// A parent row and its two subagent rows — one turn, three rows, all
	// owned by the same agent; the subagent rows additionally name their
	// subagent.
	apiLog.insert(APIEntry{Timestamp: t1, Session: "clutch/c123", Model: "m",
		Output: 100, CallType: "delegated_turn", TurnID: turnID, AgentID: "clutch"})
	apiLog.insert(APIEntry{Timestamp: t1.Add(time.Second), Session: "clutch/c123", Model: "m2",
		Output: 200, CallType: "subagent_turn", TurnID: turnID, AgentID: "clutch", SubagentID: "agent-aaa"})
	apiLog.insert(APIEntry{Timestamp: t1.Add(2 * time.Second), Session: "clutch/c123", Model: "m2",
		Output: 300, CallType: "subagent_turn", TurnID: turnID, AgentID: "clutch", SubagentID: "agent-bbb"})
	// A row from a writer with no turn identity at all, but still owned.
	apiLog.insert(APIEntry{Timestamp: t1.Add(3 * time.Second), Session: "clutch/c123", Model: "m",
		Output: 7, CallType: "summary", AgentID: "clutch"})

	got := ReadAPIDBLog()
	if len(got) != 4 {
		t.Fatalf("ReadAPIDBLog len = %d, want 4", len(got))
	}
	for i, want := range []struct{ turn, agent, subagent string }{
		{turnID, "clutch", ""}, {turnID, "clutch", "agent-aaa"}, {turnID, "clutch", "agent-bbb"}, {"", "clutch", ""},
	} {
		if got[i].TurnID != want.turn || got[i].AgentID != want.agent || got[i].SubagentID != want.subagent {
			t.Errorf("entry[%d] turn_id=%q agent_id=%q subagent_id=%q, want %q/%q/%q",
				i, got[i].TurnID, got[i].AgentID, got[i].SubagentID, want.turn, want.agent, want.subagent)
		}
	}

	// The whole turn reassembles from the id — the query shape the column exists
	// for, and the one that was impossible before it (#1695).
	var rows, out int
	if err := apiLog.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(output_tokens), 0) FROM api_calls WHERE turn_id = ?`,
		turnID).Scan(&rows, &out); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || out != 600 {
		t.Errorf("turn %s reassembled to %d rows / %d output, want 3/600", turnID, rows, out)
	}

	// Absent means NULL, not "".
	var nullTurn, emptyTurn int
	if err := apiLog.db.QueryRow(
		`SELECT COUNT(CASE WHEN turn_id IS NULL THEN 1 END), COUNT(CASE WHEN turn_id = '' THEN 1 END) FROM api_calls`,
	).Scan(&nullTurn, &emptyTurn); err != nil {
		t.Fatal(err)
	}
	if nullTurn != 1 || emptyTurn != 0 {
		t.Errorf("turn_id NULL=%d empty-string=%d, want 1/0", nullTurn, emptyTurn)
	}
}

// legacyAgentOf is a test stand-in for session.AgentIDFromAnyKey. Kept
// independent — this package cannot import internal/session (see
// BackfillAgentIDs's doc comment) — so these tests exercise BackfillAgentIDs'
// own SQL, not the parser it delegates to (covered separately by
// internal/session/key_test.go's TestAgentIDFromAnyKey).
func legacyAgentOf(session string) string {
	if strings.HasPrefix(session, "agent:") {
		parts := strings.SplitN(session, ":", 3)
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	if i := strings.Index(session, "/"); i > 0 {
		return session[:i]
	}
	return session
}

// TestBackfillAgentIDs_SplitsHistoricalRows is the #1946 migration: rows
// written before agent_id/subagent_id had their new meaning get an honest
// agent_id on every row, and a subagent_turn row's old value (the tool_use
// id, once agent_id's only occupant) moves to subagent_id.
func TestBackfillAgentIDs_SplitsHistoricalRows(t *testing.T) {
	withAPIDB(t)

	// Simulate pre-#1946 rows via raw INSERT: agent_id NULL everywhere except
	// a subagent_turn row, which carries the tool_use id — the exact shape
	// #1946 found on the live db (48,630 of 48,650 rows blank, 20 holding a
	// toolu_ id). One row uses the legacy "agent:<name>:kind:<id>" session
	// grammar to prove the injected parser, not a hardcoded slash-split, does
	// the deriving.
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := apiLog.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	mustExec(`INSERT INTO api_calls (ts, session, model, call_type, turn_id)
		VALUES ('2026-09-11T11:00:00Z', 'clutch/c123', 'm', 'delegated_turn', 'T1')`)
	mustExec(`INSERT INTO api_calls (ts, session, model, call_type, turn_id, agent_id)
		VALUES ('2026-09-11T11:00:01Z', 'clutch/c123', 'm2', 'subagent_turn', 'T1', 'toolu_01ABC')`)
	mustExec(`INSERT INTO api_calls (ts, session, model, call_type)
		VALUES ('2026-09-11T11:00:02Z', 'agent:helen:kind:1', 'm', 'summary')`)

	if err := BackfillAgentIDs(legacyAgentOf); err != nil {
		t.Fatalf("BackfillAgentIDs: %v", err)
	}

	type row struct{ callType, agentID, subagentID string }
	rows, err := apiLog.db.Query(`SELECT call_type, COALESCE(agent_id, ''), COALESCE(subagent_id, '') FROM api_calls ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.callType, &r.agentID, &r.subagentID); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{
		{"delegated_turn", "clutch", ""},
		{"subagent_turn", "clutch", "toolu_01ABC"},
		{"summary", "helen", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBackfillAgentIDs_IdempotentOnSecondRun proves the guard clauses find
// nothing left to fix once a row has been backfilled — a real concern since
// this runs on every foci-gw startup, not once.
func TestBackfillAgentIDs_IdempotentOnSecondRun(t *testing.T) {
	withAPIDB(t)

	if _, err := apiLog.db.Exec(`INSERT INTO api_calls (ts, session, model, call_type, turn_id, agent_id)
		VALUES ('2026-09-11T11:00:00Z', 'clutch/c123', 'm2', 'subagent_turn', 'T1', 'toolu_01ABC')`); err != nil {
		t.Fatal(err)
	}

	calls := 0
	counting := func(session string) string {
		calls++
		return legacyAgentOf(session)
	}
	if err := BackfillAgentIDs(counting); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first run called agentOf %d times, want 1", calls)
	}

	var agentID, subagentID string
	if err := apiLog.db.QueryRow(`SELECT agent_id, subagent_id FROM api_calls`).Scan(&agentID, &subagentID); err != nil {
		t.Fatal(err)
	}
	if agentID != "clutch" || subagentID != "toolu_01ABC" {
		t.Fatalf("after first run agent_id=%q subagent_id=%q, want clutch/toolu_01ABC", agentID, subagentID)
	}

	// Second run: a subagent_turn row is always back in scope (recomputed
	// every time by design — its old agent_id value is a tool_use id, not
	// this row's own, until the FIRST run overwrites it; after that it is
	// its real, stable agent_id, so a THIRD run would be the true steady
	// state). What must not happen is the VALUES changing.
	if err := BackfillAgentIDs(counting); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := apiLog.db.QueryRow(`SELECT agent_id, subagent_id FROM api_calls`).Scan(&agentID, &subagentID); err != nil {
		t.Fatal(err)
	}
	if agentID != "clutch" || subagentID != "toolu_01ABC" {
		t.Fatalf("after second run agent_id=%q subagent_id=%q, want unchanged clutch/toolu_01ABC", agentID, subagentID)
	}
}

// TestBackfillAgentIDs_NewRowsAreLeftAlone proves the backfill does not even
// consult agentOf for a row a caller already wrote with agent_id set — the
// common case for everything written after #1946 ships.
func TestBackfillAgentIDs_NewRowsAreLeftAlone(t *testing.T) {
	withAPIDB(t)

	apiLog.insert(APIEntry{Timestamp: time.Now(), Session: "clutch/c123", Model: "m",
		CallType: "delegated_turn", TurnID: "T1", AgentID: "clutch"})

	agentOf := func(string) string {
		t.Fatal("agentOf must not be called for a row that already has agent_id and is not a subagent row")
		return ""
	}
	if err := BackfillAgentIDs(agentOf); err != nil {
		t.Fatalf("BackfillAgentIDs: %v", err)
	}
}
