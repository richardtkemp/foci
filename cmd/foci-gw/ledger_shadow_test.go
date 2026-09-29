package main

import (
	"bytes"
	"fmt"
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
		Detail: map[string]any{"scope": "cap/c1@1", "window": 0}}, nil); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	for _, r := range []accounting.Report{
		{Backend: accounting.BackendCCStream, Session: "cap/c1", ScopeKey: "cap/c1@1", Model: "claude-opus-5",
			Grain: accounting.GrainCumulative, At: at, CostUSD: &zero},
		{Backend: accounting.BackendCCStream, Session: "cap/c1", ScopeKey: "cap/c1@1", Model: "claude-opus-5",
			Grain: accounting.GrainCumulative, At: at.Add(time.Minute), CostUSD: &priced, Tokens: tokens},
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

// TestLedgerShadowProcessInterval pins the per-process check to one interval
// on both sides (#2122): CC's side runs from the totals it restored at launch
// (its baseline report, else zero) to a report, and the adapter's side prices
// exactly the calls that report counts — found by token equality, never by a
// cut at the report's time.
func TestLedgerShadowProcessInterval(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	const model = "claude-opus-5"
	livePath, shadowPath := filepath.Join(dir, "api.db"), filepath.Join(dir, "api-shadow.db")
	live, _, err := accounting.Open(livePath, accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = live.Close()
	shadow, _, err := accounting.Open(shadowPath, accounting.Options{Shadow: true, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	toks := func(in, out int) modelinfo.Tokens {
		return modelinfo.Tokens{modelinfo.ClassInput: in, modelinfo.ClassOutput: out}
	}
	price := func(tk modelinfo.Tokens) float64 { p, _ := modelinfo.CostAsOf(model, at, tk); return p }
	sum := func(ts ...modelinfo.Tokens) modelinfo.Tokens {
		out := modelinfo.Tokens{}
		for _, tk := range ts {
			for c, n := range tk {
				out[c] += n
			}
		}
		return out
	}
	n := 0
	book := func(scope, actor string, window int, billed time.Time, tk modelinfo.Tokens) {
		t.Helper()
		n++
		session := scope[:strings.LastIndexByte(scope, '@')]
		turn := fmt.Sprintf("%s@t%d", session, window)
		if _, err := shadow.RecordCall(accounting.Turn{TurnID: turn, Session: session, Backend: accounting.BackendCCStream,
			Source: accounting.SourceUser, StartedAt: billed}, accounting.Call{Key: fmt.Sprintf("msg_%d", n),
			Backend: accounting.BackendCCStream, Model: model, Session: session, TurnID: turn, Actor: actor,
			Kind: accounting.KindCall, Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodObserved,
			BilledAt: billed, Tokens: tk, Detail: map[string]any{"scope": scope, "window": window}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	report := func(scope string, when time.Time, cost float64, tk modelinfo.Tokens) {
		t.Helper()
		session := scope[:strings.LastIndexByte(scope, '@')]
		if err := shadow.Update(func(tx *accounting.Tx) error {
			return tx.Report(accounting.Report{Backend: accounting.BackendCCStream, Session: session, ScopeKey: scope,
				Model: model, Grain: accounting.GrainCumulative, At: when, CostUSD: &cost, Tokens: tk})
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := func(session string, launch time.Time) string { return fmt.Sprintf("%s@%d", session, launch.UnixNano()) }

	// A: a FRESH process — no baseline report; its first report comes after
	// its first turn, whose spend it includes.
	a := scope("coach/cA", at)
	a0, a1 := toks(1000, 20000), toks(2000, 5000)
	book(a, "", 0, at.Add(10*time.Second), a0)
	report(a, at.Add(11*time.Second), price(a0), a0)
	book(a, "", 1, at.Add(20*time.Second), a1)
	report(a, at.Add(21*time.Second), price(a0)+price(a1), sum(a0, a1))

	// B: a fresh branch whose result and closing cost-state report agree.
	b := scope("coach/cA/bB", at.Add(time.Second))
	b0 := toks(3000, 30000)
	book(b, "", 0, at.Add(5*time.Second), b0)
	report(b, at.Add(6*time.Second), price(b0), b0)
	report(b, at.Add(7*time.Second), price(b0), b0)

	// C: a RESUMED process — CC restored $100 of earlier spend at launch —
	// with a turn in flight after its last report.
	launchC := at.Add(2 * time.Second)
	c := scope("clutch/cC", launchC)
	base := toks(9_000_000, 900_000)
	report(c, launchC, 100, base)
	c0, c1 := toks(1500, 15000), toks(700, 70000)
	book(c, "", 0, at.Add(30*time.Second), c0)
	report(c, at.Add(31*time.Second), 100+price(c0), sum(base, c0))
	book(c, "", 1, at.Add(40*time.Second), c1)

	// D: a subagent spawned in window 0 still running past the only report:
	// no report counts what the adapter booked, so nothing is compared.
	d := scope("coach/cD", at.Add(3*time.Second))
	d0, ds := toks(1000, 1000), toks(500, 50000)
	book(d, "", 0, at.Add(50*time.Second), d0)
	report(d, at.Add(51*time.Second), price(d0), d0)
	book(d, "toolu_x", 0, at.Add(55*time.Second), ds)
	_ = shadow.Close()

	var out, errb bytes.Buffer
	if code := runLedgerShadow([]string{"-live", livePath, "-shadow", shadowPath}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"CC processes: 3 compared, 1 unmatched", "3 of 3 processes within 3%", "unmatched", d} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	wantCC := price(a0) + price(a1) + price(b0) + price(c0)
	if want := fmt.Sprintf("live $%.4f  shadow $%.4f  diff $+0.0000; 3 of 3 processes", wantCC, wantCC); !strings.Contains(got, want) {
		t.Errorf("report lacks %q:\n%s", want, got)
	}
}
