package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// TestLedgerShadowCompare drives `foci-gw ledger-shadow` over a live ledger
// holding one legacy Claude Code turn and a shadow holding the adapter's view
// of the same turn plus CC's own reports for its process: the turn pairs up,
// the process's CC cost is compared with the adapter's, and the day totals
// are printed.
func TestLedgerShadowCompare(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	livePath, shadowPath := filepath.Join(dir, "api.db"), filepath.Join(dir, "api-shadow.db")

	live, _, err := accounting.Open(livePath, accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	calc := 0.50
	if err := live.BookLegacy(accounting.LegacyRow{At: at, Backend: accounting.BackendCCStream, Session: "cap/c1",
		Model: "claude-opus-5", TurnID: "cap/c1@1", Turn: &modelinfo.TokenCounts{Input: 100}, CalculatedCostUSD: &calc}); err != nil {
		t.Fatal(err)
	}
	_ = live.Close()

	shadow, _, err := accounting.Open(shadowPath, accounting.Options{Shadow: true, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	tokens := modelinfo.Tokens{modelinfo.ClassInput: 1000, modelinfo.ClassOutput: 10000}
	priced, _ := modelinfo.CostAsOf("claude-opus-5", at, tokens)
	if _, err := shadow.RecordCall(accounting.Turn{TurnID: "cap/c1@1", Session: "cap/c1", Backend: accounting.BackendCCStream,
		Source: accounting.SourceUser, StartedAt: at}, accounting.Call{Key: "msg_1", Backend: accounting.BackendCCStream,
		Model: "claude-opus-5", Session: "cap/c1", TurnID: "cap/c1@1", Kind: accounting.KindCall,
		Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodObserved, BilledAt: at, Tokens: tokens,
		Detail: map[string]any{"scope": "cap/c1@1"}}, nil); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	for _, r := range []accounting.Report{
		{Backend: accounting.BackendCCStream, Session: "cap/c1", ScopeKey: "cap/c1@1", Model: "claude-opus-5",
			Grain: accounting.GrainCumulative, At: at, CostUSD: &zero},
		{Backend: accounting.BackendCCStream, Session: "cap/c1", ScopeKey: "cap/c1@1", Model: "claude-opus-5",
			Grain: accounting.GrainCumulative, At: at.Add(time.Minute), CostUSD: &priced},
	} {
		if err := shadow.Update(func(tx *accounting.Tx) error { return tx.Report(r) }); err != nil {
			t.Fatal(err)
		}
	}
	_ = shadow.Close()

	var out, errb bytes.Buffer
	if code := runLedgerShadow([]string{"-live", livePath, "-shadow", shadowPath}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"turns in both: 1", "1 of 1 processes within 3%", "2026-09-30"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if code := runLedgerShadow(nil, &out, &errb); code != 2 {
		t.Errorf("no flags: exit %d, want 2", code)
	}
}
