package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// fixtureLines is testdata/tokenusage_synthetic.json's notifications, in
// order: the four REAL cycles of one turn (#1855 capture), a verbatim
// re-delivery of the last, and two SYNTHETIC cycles with cache writes and
// reasoning set.
func fixtureLines(t *testing.T) [][]byte {
	t.Helper()
	data, err := os.ReadFile("testdata/tokenusage_synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Events []struct {
			Line json.RawMessage `json:"line"`
		} `json:"events"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, len(f.Events))
	for i, e := range f.Events {
		out[i] = e.Line
	}
	return out
}

// ledgerFacade is a guarded test facade on session "test/session" with a live
// ledger and an open foci turn "test/session@1" recorded as the agent records
// it. Not for parallel tests.
func ledgerFacade(t *testing.T) (*Backend, *delegator.ExpectationGuard, *[]accounting.Alarm, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	var alarms []accounting.Alarm
	l, _, err := accounting.Open(path, accounting.Options{OnAlarm: func(a accounting.Alarm) { alarms = append(alarms, a) }})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	b, g := guardedBackend(t)
	b.agentID = "gil"
	b.mu.Lock()
	b.model = "gpt-5.1-codex-max"
	b.mu.Unlock()
	openLedgerTurn(t, b, "test/session@1")
	return b, g, &alarms, path
}

// openLedgerTurn opens foci turn id on the facade and records it in the ledger.
func openLedgerTurn(t *testing.T, b *Backend, id string) {
	t.Helper()
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{TurnID: id}
	b.turnMu.Unlock()
	if err := accounting.RecordTurn(accounting.Turn{TurnID: id, Session: "test/session", Backend: accounting.BackendCodex,
		Source: accounting.SourceUser, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

type cycleCall struct {
	key, turn, actor string
	tokens           map[string]int
	detail           string
}

func readCycles(t *testing.T, path string) []cycleCall {
	t.Helper()
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id, call_key, turn_id, actor, COALESCE(detail, '') FROM api_calls ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []cycleCall
	var ids []int64
	for rows.Next() {
		var id int64
		var c cycleCall
		if err := rows.Scan(&id, &c.key, &c.turn, &c.actor, &c.detail); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		out = append(out, c)
	}
	_ = rows.Close()
	for i, id := range ids {
		out[i].tokens = map[string]int{}
		tr, err := db.Query(`SELECT class, count FROM call_tokens WHERE call_id = ?`, id)
		if err != nil {
			t.Fatal(err)
		}
		for tr.Next() {
			var c string
			var n int
			_ = tr.Scan(&c, &n)
			out[i].tokens[c] = n
		}
		_ = tr.Close()
	}
	return out
}

// TestCodexCyclesAreCallsOnTheTurn is T17-codex: replaying the fixture books
// every API cycle once — keyed by its thread and the running total after it,
// so the re-delivered notification is not booked twice — on the open turn,
// in disjoint classes (cached input taken out of input, cache writes as
// cache_write, reasoning inside output and so only in detail). The four real
// cycles sum to codex's own running total exactly (the token-only
// conservation control), the child thread's cycles are its subagent's calls,
// and conservation holds throughout.
func TestCodexCyclesAreCallsOnTheTurn(t *testing.T) {
	b, g, alarms, path := ledgerFacade(t)
	for _, line := range fixtureLines(t) {
		b.dispatch(line)
	}

	calls := readCycles(t, path)
	if len(calls) != 6 {
		t.Fatalf("calls = %d, want 4 real cycles + 2 synthetic (the re-delivery booked once)", len(calls))
	}
	var sum map[string]int = map[string]int{}
	for _, c := range calls[:4] {
		if c.turn != "test/session@1" || c.actor != "" || !strings.HasPrefix(c.key, "th_1:") {
			t.Errorf("real cycle = %+v, want a th_1 call on the open turn", c)
		}
		for k, n := range c.tokens {
			sum[k] += n
		}
	}
	if sum["input"] != 9241 || sum["output"] != 151 || sum["cache_read"] != 53376 {
		t.Errorf("real cycles sum to %v, want input 9241 output 151 cache_read 53376", sum)
	}
	if got := sum["input"] + sum["cache_read"]; got != 62617 {
		t.Errorf("input+cache_read = %d, want codex's own total.inputTokens 62617", got)
	}
	if calls[3].key != "th_1:62768" {
		t.Errorf("last real key = %q, want thread:running total th_1:62768", calls[3].key)
	}
	cw := calls[4]
	if cw.actor != "" || cw.turn != "test/session@1" {
		t.Errorf("synthetic cycle = %+v, want a th_1 call on the open turn", cw)
	}
	if cw.tokens["cache_write"] != 4096 || cw.tokens["output"] != 300 || cw.tokens["reasoning"] != 0 ||
		!strings.Contains(cw.detail, `"reasoning_output_tokens":120`) {
		t.Errorf("synthetic classes = %v detail %s, want cache_write 4096, output 300 (reasoning inside it, in detail)", cw.tokens, cw.detail)
	}
	if n := g.Count(expectBackend, invTotalIsSumOfLast); n != 0 {
		t.Errorf("conservation violated %d times on consistent traffic", n)
	}
	// The one cycle with cache writes alarms: no codex model publishes a
	// cache-write rate, and an unpriced class is NULL and loud, never $0.
	if len(*alarms) != 1 || (*alarms)[0].Invariant != accounting.InvClassNoRate ||
		!strings.Contains((*alarms)[0].Detail, "cache_write") {
		t.Errorf("alarms = %+v, want exactly the unpriced cache_write", *alarms)
	}
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var reports, withCost int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(cost_usd IS NOT NULL) FROM backend_reports
		WHERE backend = 'codex' AND grain = 'cumulative'`).Scan(&reports, &withCost); err != nil || reports != 6 || withCost != 0 {
		t.Errorf("reports = %d (%d with a cost, %v), want 6 cumulative token-only reports", reports, withCost, err)
	}
}

// TestCodexConservationIsChecked: a running total that does not grow by its
// cycle is a conservation break.
func TestCodexConservationIsChecked(t *testing.T) {
	b, g, _, _ := ledgerFacade(t)
	lines := fixtureLines(t)
	for _, line := range lines[:6] {
		b.dispatch(line)
	}
	broken := strings.Replace(string(lines[6]), `"inputTokens": 81117`, `"inputTokens": 99999`, 1)
	if broken == string(lines[6]) {
		broken = strings.Replace(string(lines[6]), `"inputTokens":81117`, `"inputTokens":99999`, 1)
	}
	if broken == string(lines[6]) {
		t.Fatalf("fixture premise: running total not found in %s", lines[6])
	}
	b.dispatch([]byte(broken))
	if n := g.Count(expectBackend, invTotalIsSumOfLast); n != 1 {
		t.Errorf("conservation violations = %d, want 1 for the broken total", n)
	}
}

// TestCycleTokensAreDisjoint pins the normalisation on its own.
func TestCycleTokensAreDisjoint(t *testing.T) {
	got := cycleTokens(tokenUsageBreakdown{InputTokens: 100, CachedInputTokens: 60, CacheWriteInputTokens: 7,
		OutputTokens: 20, ReasoningOutputTokens: 5, TotalTokens: 120})
	want := modelinfo.Tokens{modelinfo.ClassInput: 40, modelinfo.ClassCacheRead: 60,
		modelinfo.ClassCacheWrite: 7, modelinfo.ClassOutput: 20}
	for c, n := range want {
		if got[c] != n {
			t.Errorf("%s = %d, want %d", c, got[c], n)
		}
	}
	if got[modelinfo.ClassReasoning] != 0 {
		t.Error("codex reasoning is inside output; a reasoning class would bill it twice")
	}
}
