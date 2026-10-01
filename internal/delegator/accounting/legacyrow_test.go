package accounting

import (
	"math"
	"os"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func sessTurn(name string) string { return "sess@" + name }

var startT1 = time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC)

// TestBookLegacyUnnamedSubagent: a share whose usage arrived before anything
// named its subagent is still a subagent's call, never the session's own
// thread (actor ""), and each is booked on its own.
func TestBookLegacyUnnamedSubagent(t *testing.T) {
	l, _ := openLedger(t)
	for i := 0; i < 2; i++ {
		c := modelinfo.TokenCounts{Output: 10}
		cost := 0.5
		if err := l.BookLegacy(LegacyRow{
			At: startT1, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
			Subagent: true, Turn: &c, CalculatedCostUSD: &cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM api_calls WHERE actor = ?`, UnnamedSubagent).Scan(&n); err != nil || n != 2 {
		t.Errorf("unnamed subagent calls = %d (%v), want 2", n, err)
	}
}

// TestBookLegacyStoresTheBackendReport: a CC parent row's provided cost is
// CC's cumulative per-process figure; it is kept as a backend report (R5),
// never as the call's cost.
func TestBookLegacyStoresTheBackendReport(t *testing.T) {
	l, _ := openLedger(t)
	provided, calc := 12.5, 0.25
	c := modelinfo.TokenCounts{Input: 10, Output: 20}
	if err := l.BookLegacy(LegacyRow{
		At: startT1, Backend: BackendCCStream, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
		Turn: &c, ProvidedCostUSD: &provided, CalculatedCostUSD: &calc,
	}); err != nil {
		t.Fatal(err)
	}
	var grain string
	var reported float64
	if err := l.db.QueryRow(`SELECT grain, cost_usd FROM backend_reports WHERE backend = 'ccstream'`).Scan(&grain, &reported); err != nil ||
		grain != GrainCumulative || reported != provided {
		t.Errorf("report = %s $%v (%v), want cumulative $12.5", grain, reported, err)
	}
	var cost float64
	var basis string
	if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM call_costs`).Scan(&cost, &basis); err != nil ||
		cost != calc || basis != CostBasisRecorded {
		t.Errorf("call cost = $%v on %s (%v), want the recorded $0.25", cost, basis, err)
	}
}

// openJSONL makes path the api.jsonl for the test.
func openJSONL(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log.SetAPIWriter(f)
	t.Cleanup(func() { log.SetAPIWriter(nil) })
	return f
}
