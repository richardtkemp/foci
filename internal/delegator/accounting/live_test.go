package accounting

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// useLive makes l the live ledger for the test.
func useLive(t *testing.T, l *Ledger) {
	t.Helper()
	SetLive(l)
	t.Cleanup(func() { SetLive(nil) })
}

// hook collects every booking the observers see.
func hook(t *testing.T) *[]Booking {
	t.Helper()
	var seen []Booking
	BookedHook = func(b Booking) { seen = append(seen, b) }
	t.Cleanup(func() { BookedHook = nil })
	return &seen
}

func apiTurn(id string) Turn {
	return Turn{TurnID: id, Session: "gil/c1", AgentID: "gil", Backend: BackendAPI, Source: SourceUser, StartedAt: t0}
}

func apiResponse(id string, tokens modelinfo.Tokens) APIResponse {
	return APIResponse{
		ID: id, Kind: KindCall, Provider: "anthropic", Model: "claude-opus-5",
		Session: "gil/c1", AgentID: "gil", TurnID: "gil/c1@1",
		Start: t0, Duration: 1500 * time.Millisecond, Tokens: tokens, StopReason: "end_turn",
	}
}

// TestRecordBooksADirectAPICall is the direct API adapter end to end (#2111
// P2): one response is one call on its turn, keyed by the response id, its
// cache writes 1h by the backend rule, priced by the views exactly as Go
// prices it (T11), and observed once — api.jsonl and BookedHook — only after
// its transaction commits. The same response booked again is a no-op.
func TestRecordBooksADirectAPICall(t *testing.T) {
	l, alarms := openLedger(t)
	useLive(t, l)
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openJSONL(t, path)
	seen := hook(t)

	tokens := modelinfo.Tokens{modelinfo.ClassInput: 120, modelinfo.ClassOutput: 45,
		modelinfo.ClassCacheRead: 9000, modelinfo.ClassCacheWrite1h: 800}
	c := apiResponse("msg_01", tokens).Call()
	for i := 0; i < 2; i++ {
		if err := Record(apiTurn("gil/c1@1"), c); err != nil {
			t.Fatalf("Record #%d: %v", i, err)
		}
	}
	_ = f.Close()

	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("calls = %d (%v), want 1: a response booked twice is booked once", len(rows), err)
	}
	r := rows[0]
	want, _ := modelinfo.CostAsOf("claude-opus-5", t0, tokens)
	if r.Backend != BackendAPI || r.Kind != KindCall || r.TurnID != "gil/c1@1" || r.CostUSD == nil || !near(*r.CostUSD, want) {
		t.Fatalf("call = %+v, want an api call on gil/c1@1 costing $%.6f", r, want)
	}
	if r.Fill != 120+9000+800 {
		t.Errorf("fill = %d, want the call's in-context classes", r.Fill)
	}
	var method string
	if err := l.db.QueryRow(`SELECT class_method FROM api_calls`).Scan(&method); err != nil || method != ClassMethodBackendRule {
		t.Errorf("class_method = %q (%v), want backend_rule", method, err)
	}
	if len(*seen) != 1 || (*seen)[0].CostUSD == nil || !near(*(*seen)[0].CostUSD, want) {
		t.Fatalf("hook saw %d bookings, want exactly one priced at $%.6f", len(*seen), want)
	}
	if lines := ReadJSONL(path); len(lines) != 1 || !near(lines[0].Cost(), want) || lines[0].Count(modelinfo.ClassCacheWrite1h) != 800 {
		t.Errorf("api.jsonl = %+v, want one line carrying the call", lines)
	}
	if len(*alarms) != 0 {
		t.Errorf("alarms: %+v", *alarms)
	}

	// A rolled-back booking is never observed.
	*seen = nil
	_ = l.Update(func(tx *Tx) error {
		if _, err := tx.Book(apiResponse("msg_02", tokens).Call()); err != nil {
			return err
		}
		return errors.New("roll back")
	})
	if len(*seen) != 0 {
		t.Errorf("a rolled-back booking reached the hook: %+v", *seen)
	}
}

// TestRecordWithoutALedger: a gateway with no api_db (and a unit test) still
// writes api.jsonl and feeds the hook, as the pre-ledger writer did.
func TestRecordWithoutALedger(t *testing.T) {
	SetLive(nil)
	seen := hook(t)
	c := apiResponse("msg_01", modelinfo.Tokens{modelinfo.ClassInput: 10}).Call()
	if err := Record(apiTurn("gil/c1@1"), c); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Key != "msg_01" {
		t.Errorf("hook saw %+v, want the call", *seen)
	}
	bad := c
	bad.BilledAt = time.Time{}
	if err := Record(apiTurn("gil/c1@1"), bad); err == nil {
		t.Error("a call with no billing time was accepted")
	}
}

// TestAPIResponseCall: the adapter's normalisation — a response with no id is
// keyed by its kind, session and start (unique per session and kind), a
// session-less call gets NoSession, and duration and pre-compaction message
// count are kept in detail.
func TestAPIResponseCall(t *testing.T) {
	r := apiResponse("", modelinfo.Tokens{modelinfo.ClassInput: 1})
	r.Kind, r.Session, r.PreMessages = KindCompaction, "", 42
	c := r.Call()
	if want := "compaction:" + NoSession + "@" + itoa(t0.UnixNano()); c.Key != want {
		t.Errorf("key = %q, want %q", c.Key, want)
	}
	if c.Session != NoSession || c.Detail["pre_messages"] != 42 || c.Detail["duration_ms"] != int64(1500) {
		t.Errorf("call = %+v", c)
	}
	if err := c.validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
	if got := MintTurnID("a/c1", KindSpawn, t0); got != "a/c1@"+itoa(t0.UnixNano())+":spawn" {
		t.Errorf("MintTurnID = %q", got)
	}
	for model, want := range map[string]string{"gemini-2.5-pro": "gemini", "gpt-4": "openai", "claude-haiku-4-5": "anthropic"} {
		r := apiResponse("x", nil)
		r.Provider, r.Model = "", model
		if got := r.Call().Provider; got != want {
			t.Errorf("provider for %s = %q, want %q", model, got, want)
		}
	}
	if got := apiResponse("x", nil).Call().Provider; got != "anthropic" {
		t.Errorf("an explicit provider was overridden: %q", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestEmptyModelIsUnpricedWithoutAskingModelinfo is #1290 on the ledger: a
// call whose model is not known yet is booked unpriced and alarmed, without
// modelinfo's unpriced-model warning, which is for a model it does not know.
func TestEmptyModelIsUnpricedWithoutAskingModelinfo(t *testing.T) {
	l, alarms := openLedger(t)
	prev := modelinfo.UnpricedModelHook
	var fired []string
	modelinfo.UnpricedModelHook = func(m string) { fired = append(fired, m) }
	t.Cleanup(func() { modelinfo.UnpricedModelHook = prev })

	c := call("k1", modelinfo.Tokens{modelinfo.ClassInput: 10})
	c.Model = ""
	mustUpdate(t, l, func(tx *Tx) error {
		if err := tx.RecordTurn(turn("T")); err != nil {
			return err
		}
		_, err := tx.Book(c)
		return err
	})
	if len(fired) != 0 {
		t.Errorf("UnpricedModelHook fired for %v", fired)
	}
	if len(*alarms) != 1 || (*alarms)[0].Invariant != InvModelNotInTable {
		t.Errorf("alarms = %+v, want one invModelNotInTable", *alarms)
	}
	if c.cost() != nil {
		t.Error("an unnamed model priced its tokens")
	}
}

// TestCutoverThenBook is the startup cutover (#2111 P2): Open on a live
// pre-ledger api.db migrates it, and the first bookings after it — a direct
// API call and a delegated legacy row — land in the same ledger with ids
// above every carried-over row, so an id watermark (the Langfuse ETL's) keeps
// working across the cutover.
func TestCutoverThenBook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	writeV1(t, path, []v1{
		{id: 7, ts: t0.Add(-time.Hour), session: "gil/c1", model: "claude-opus-5", callType: "conversation",
			fill: [4]int{10, 20, 30, 40}, calc: f(0.01)},
		{id: 9, ts: t0.Add(-time.Hour), session: "helen/c1", model: "claude-opus-5", callType: "delegated_turn",
			fill: [4]int{1, 2, 3, 4}, turn: &[4]int{5, 6, 7, 8}, calc: f(0.5), turnID: "helen/c1@1"},
	})
	l, rep, err := Open(path, Options{BackupPath: filepath.Join(t.TempDir(), "backup.db")})
	if err != nil || rep == nil {
		t.Fatalf("Open: report=%v err=%v, want a migration", rep, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	useLive(t, l)

	resp := apiResponse("msg_new", modelinfo.Tokens{modelinfo.ClassInput: 5})
	resp.TurnID = "gil/c1@2"
	if err := Record(apiTurn("gil/c1@2"), resp.Call()); err != nil {
		t.Fatal(err)
	}
	calc := 0.2
	if err := l.BookLegacy(LegacyRow{At: t0, Session: "helen/c1", Model: "claude-opus-5", TurnID: "helen/c1@2",
		Turn: &modelinfo.TokenCounts{Input: 1}, CalculatedCostUSD: &calc}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 4 {
		t.Fatalf("calls = %d (%v), want 2 migrated + 2 new", len(rows), err)
	}
	for _, r := range rows[2:] {
		if r.ID <= 9 {
			t.Errorf("new call id %d is not above the carried-over ids", r.ID)
		}
	}
	// Reopening is not a second migration.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, rep2, err := Open(path, Options{})
	if err != nil || rep2 != nil {
		t.Fatalf("reopen: report=%v err=%v, want no migration", rep2, err)
	}
	_ = l2.Close()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'api_calls_v1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("api_calls_v1 still present (%d, %v)", n, err)
	}
}

// TestConcurrentBookings: every agent books through one ledger at once, and a
// subagent share's transaction reads before it writes. Writes are serialised,
// so none fails with SQLITE_BUSY and nothing is lost.
func TestConcurrentBookings(t *testing.T) {
	l, _ := openLedger(t)
	const n = 40
	errs := make(chan error, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			turnID := "gil/c1@" + itoa(int64(i))
			r := apiResponse("msg_"+itoa(int64(i)), modelinfo.Tokens{modelinfo.ClassInput: 10})
			r.TurnID = turnID
			errs <- l.Update(func(tx *Tx) error {
				if err := tx.RecordTurn(apiTurn(turnID)); err != nil {
					return err
				}
				_, err := tx.Book(r.Call())
				return err
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			cost := 0.01
			_, err := l.AccumulateLegacySubagent(LegacyRow{
				At: t0, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T"),
				Subagent: true, SubagentID: "agent-" + itoa(int64(i%4)),
				Turn: &modelinfo.TokenCounts{Output: 1}, CalculatedCostUSD: &cost,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	var shares float64
	if err := l.db.QueryRow(`SELECT COUNT(*), TOTAL(legacy_calculated_cost_usd) FROM api_calls`).Scan(&calls, &shares); err != nil {
		t.Fatal(err)
	}
	if calls != n+4 || !near(shares, n*0.01) {
		t.Errorf("calls = %d, share total $%.4f; want %d calls and $%.2f", calls, shares, n+4, n*0.01)
	}
}
