package accounting

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// TestZeroCostEndpointBooksAtZero is #3035: a direct-API call on an
// [endpoints] entry set zero_cost (a local model server) is booked at $0 —
// in the views, in Go and in api.jsonl — and neither modelinfo's
// unpriced-model warning nor invModelNotInTable fires for its model, which
// is in no rate table. The same model on any other endpoint stays unpriced
// and alarmed (the control arm).
func TestZeroCostEndpointBooksAtZero(t *testing.T) {
	const localModel = "local/qwen3.6:35b-a3b-mtp-q4_K_M"
	modelinfo.SetZeroCostEndpoints([]string{"local"})
	t.Cleanup(func() { modelinfo.SetZeroCostEndpoints(nil) })
	prev := modelinfo.UnpricedModelHook
	var warned []string
	modelinfo.UnpricedModelHook = func(m string) { warned = append(warned, m) }
	t.Cleanup(func() { modelinfo.UnpricedModelHook = prev })

	l, alarms := openLedger(t)
	useLive(t, l)
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openJSONL(t, path)
	seen := hook(t)

	tokens := modelinfo.Tokens{modelinfo.ClassInput: 1200, modelinfo.ClassOutput: 300, modelinfo.ClassCacheRead: 5000}
	resp := apiResponse("local_1", tokens)
	resp.Provider, resp.Model, resp.Endpoint = "openai", localModel, "local"
	c := resp.Call()
	if !c.ZeroCost {
		t.Fatal("a call on the zero_cost endpoint is not marked ZeroCost")
	}
	if err := Record(apiTurn("gil/c1@1"), c); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_ = f.Close()

	if len(warned) != 0 {
		t.Errorf("UnpricedModelHook fired for %v on a zero_cost endpoint", warned)
	}
	if len(*alarms) != 0 {
		t.Errorf("alarms on a zero_cost endpoint: %+v", *alarms)
	}
	var rateModel string
	if err := l.db.QueryRow(`SELECT rate_model FROM api_calls WHERE call_key = 'local_1'`).Scan(&rateModel); err != nil || rateModel != modelinfo.ZeroCostRateModel {
		t.Errorf("rate_model = %q (%v), want %q", rateModel, err, modelinfo.ZeroCostRateModel)
	}
	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("calls = %d (%v), want 1", len(rows), err)
	}
	if r := rows[0]; r.CostUSD == nil || *r.CostUSD != 0 || r.Model != localModel {
		t.Errorf("call = %+v, want model %q at $0 (not NULL)", r, localModel)
	}
	if len(*seen) != 1 || (*seen)[0].CostUSD == nil || *(*seen)[0].CostUSD != 0 {
		t.Errorf("hook saw %+v, want one booking at $0", *seen)
	}
	lines := ReadJSONL(path)
	if len(lines) != 1 || lines[0].CostUSD == nil || *lines[0].CostUSD != 0 {
		t.Fatalf("api.jsonl = %+v, want one line at $0", lines)
	}
	for class, cc := range lines[0].Classes {
		if cc.CostUSD == nil || *cc.CostUSD != 0 {
			t.Errorf("api.jsonl class %s = %+v, want $0", class, cc)
		}
	}
	var cost *float64
	if err := l.db.QueryRow(`SELECT cost_usd FROM call_costs WHERE call_key = 'local_1'`).Scan(&cost); err != nil || cost == nil || *cost != 0 {
		t.Errorf("call_costs.cost_usd = %v (%v), want 0", cost, err)
	}

	// Control: the same model on an endpoint not set zero_cost is unpriced.
	other := apiResponse("local_2", tokens)
	other.Provider, other.Model, other.Endpoint = "openai", localModel, "elsewhere"
	if err := Record(apiTurn("gil/c1@1"), other.Call()); err != nil {
		t.Fatalf("Record control: %v", err)
	}
	if len(warned) != 1 {
		t.Errorf("UnpricedModelHook fired %v on the control, want once", warned)
	}
	if len(*alarms) != 1 || (*alarms)[0].Invariant != InvModelNotInTable {
		t.Errorf("control alarms = %+v, want one invModelNotInTable", *alarms)
	}
}

// TestZeroCostEndpointIsLive: the zero-cost set is replaced whole, so an
// endpoint taken out of it prices normally again, and the empty endpoint
// name is never zero-cost.
func TestZeroCostEndpointIsLive(t *testing.T) {
	t.Cleanup(func() { modelinfo.SetZeroCostEndpoints(nil) })
	modelinfo.SetZeroCostEndpoints([]string{"local"})
	if !(APIResponse{Endpoint: "local", Model: "m"}).Call().ZeroCost {
		t.Error("local not zero-cost after it was set")
	}
	if (APIResponse{Model: "m"}).Call().ZeroCost {
		t.Error("a call with no endpoint is zero-cost")
	}
	modelinfo.SetZeroCostEndpoints(nil)
	if (APIResponse{Endpoint: "local", Model: "m"}).Call().ZeroCost {
		t.Error("local still zero-cost after the set was cleared")
	}
}
