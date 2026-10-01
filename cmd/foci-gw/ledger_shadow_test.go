package main

import (
	"bytes"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/delegator/ccstream"
	"foci/internal/modelinfo"
)

// TestLedgerShadowCompare drives `foci-gw ledger-shadow` over a live ledger
// holding one Claude Code turn and a shadow holding the adapter's view
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
	if _, err := live.RecordCall(accounting.Turn{TurnID: "cap/c1@1", Session: "cap/c1", Backend: accounting.BackendCCStream,
		Source: accounting.SourceUser, StartedAt: at}, accounting.Call{Key: "msg_live", Backend: accounting.BackendCCStream,
		Model: "claude-opus-5", Session: "cap/c1", TurnID: "cap/c1@1", Kind: accounting.KindCall,
		Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodObserved, BilledAt: at,
		Tokens: modelinfo.Tokens{modelinfo.ClassOutput: 20000}}, nil); err != nil {
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

// TestLedgerShadowFlagsTurnlessTTLRemainder reproduces the 21:11 shape of
// #2130: a process whose tokens CC and the adapter agree on exactly, but whose
// Workflow agents' spend is a turn-less remainder with unknown-TTL cache
// writes, priced at 1h where CC priced 5m. A token-only comparison passes it;
// the report must flag the dollar gap as the TTL guess, the remainder as not
// overhead by design, and the unknown-TTL writes priced both ways (#2131) —
// while a clean process and a small by-design overhead row stay unflagged.
func TestLedgerShadowFlagsTurnlessTTLRemainder(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	const opus, sonnet = "claude-opus-5-5", "claude-sonnet-5"
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
	price := func(model string, tk modelinfo.Tokens) float64 {
		p, ok := modelinfo.CostAsOf(model, at, tk)
		if !ok {
			t.Fatalf("%s unpriced: %v", model, tk)
		}
		return p
	}
	// asCC is CC's own view of tokens: unknown-TTL writes were 5m.
	asCC := func(tk modelinfo.Tokens) modelinfo.Tokens {
		out := maps.Clone(tk)
		out[modelinfo.ClassCacheWrite5m] += out[modelinfo.ClassCacheWrite]
		delete(out, modelinfo.ClassCacheWrite)
		return out
	}
	n := 0
	book := func(scope, model string, window int, derived bool, tk modelinfo.Tokens) {
		t.Helper()
		n++
		session := scope[:strings.LastIndexByte(scope, '@')]
		c := accounting.Call{Key: fmt.Sprintf("msg_%d", n), Backend: accounting.BackendCCStream, Model: model,
			Session: session, TurnID: fmt.Sprintf("%s@t%d", session, window), Kind: accounting.KindCall,
			Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodObserved,
			BilledAt: at.Add(time.Duration(n) * time.Second), Tokens: tk, Detail: map[string]any{"scope": scope, "window": window}}
		turn := accounting.Turn{TurnID: c.TurnID, Session: session, Backend: accounting.BackendCCStream,
			Source: accounting.SourceUser, StartedAt: c.BilledAt}
		if derived {
			c.Key = fmt.Sprintf("%s:q%d:%s", scope, window, model)
			c.TurnID, turn.TurnID = "", ""
			c.Kind, c.Finality, c.ClassMethod = accounting.KindOverhead, accounting.FinalityDerived, accounting.ClassMethodUnknown
			c.Detail = map[string]any{"scope": scope, "through_window": window}
		}
		if _, err := shadow.RecordCall(turn, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	// report writes CC's cumulative figure per model, all at one instant.
	report := func(scope string, when time.Time, byModel map[string][]modelinfo.Tokens) {
		t.Helper()
		session := scope[:strings.LastIndexByte(scope, '@')]
		for model, tks := range byModel {
			sum, cost := modelinfo.Tokens{}, 0.0
			for _, tk := range tks {
				cost += price(model, asCC(tk))
				for c, k := range tk {
					sum[c] += k
				}
			}
			if err := shadow.Update(func(tx *accounting.Tx) error {
				return tx.Report(accounting.Report{Backend: accounting.BackendCCStream, Session: session, ScopeKey: scope,
					Model: model, Grain: accounting.GrainCumulative, At: when, CostUSD: &cost, Tokens: ccstream.ReportClasses(sum)})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	scope := func(session string, off time.Duration) string {
		return fmt.Sprintf("%s@%d", session, at.Add(off).UnixNano())
	}
	obs := func(read, w5m, w1h, out int) modelinfo.Tokens {
		return modelinfo.Tokens{modelinfo.ClassInput: 3, modelinfo.ClassCacheRead: read,
			modelinfo.ClassCacheWrite5m: w5m, modelinfo.ClassCacheWrite1h: w1h, modelinfo.ClassOutput: out}
	}

	// X, the 21:11 process: three windows of main-thread calls, then a Workflow
	// whose agents' spend is booked only as the window-2 remainder, per model,
	// with unknown-TTL writes (api_calls 533/534).
	x := scope("clutch/cX", 0)
	x0, x1, x2 := obs(100_000, 2_000, 1_000, 3_000), obs(150_000, 0, 3_000, 2_000), obs(200_000, 5_000, 0, 4_000)
	book(x, opus, 0, false, x0)
	book(x, opus, 1, false, x1)
	book(x, opus, 2, false, x2)
	rOpus := modelinfo.Tokens{modelinfo.ClassInput: 216, modelinfo.ClassCacheRead: 1_848_955,
		modelinfo.ClassCacheWrite: 305_815, modelinfo.ClassOutput: 40_827}
	rSonnet := modelinfo.Tokens{modelinfo.ClassInput: 440, modelinfo.ClassCacheRead: 2_924_957,
		modelinfo.ClassCacheWrite: 1_173_395, modelinfo.ClassOutput: 57_902}
	book(x, opus, 2, true, rOpus)
	book(x, sonnet, 2, true, rSonnet)
	report(x, at.Add(time.Hour), map[string][]modelinfo.Tokens{opus: {x0, x1, x2, rOpus}, sonnet: {rSonnet}})

	// Y: a clean process, every class observed — CC and the adapter agree.
	y := scope("coach/cY", time.Second)
	y0 := obs(80_000, 4_000, 0, 1_000)
	book(y, opus, 0, false, y0)
	report(y, at.Add(time.Hour), map[string][]modelinfo.Tokens{opus: {y0}})

	// Z: a small overhead remainder of the kind CC's utility calls leave.
	z := scope("coach/cZ", 2*time.Second)
	z0, zr := obs(90_000, 1_000, 0, 5_000), modelinfo.Tokens{modelinfo.ClassOutput: 197}
	book(z, opus, 0, false, z0)
	book(z, opus, 0, true, zr)
	report(z, at.Add(time.Hour), map[string][]modelinfo.Tokens{opus: {z0, zr}})
	_ = shadow.Close()

	var out, errb bytes.Buffer
	if code := runLedgerShadow([]string{"-live", livePath, "-shadow", shadowPath}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	// The token check agrees on all three: the 21:11 fault is invisible to it.
	if !strings.Contains(got, "CC processes: 3 compared, 0 unmatched") {
		t.Fatalf("report lacks the three compared processes:\n%s", got)
	}
	_, flags, ok := strings.Cut(got, "\nFLAGS")
	if !ok {
		t.Fatalf("report has no FLAGS section:\n%s", got)
	}
	gap := price(opus, rOpus) + price(sonnet, rSonnet) - price(opus, asCC(rOpus)) - price(sonnet, asCC(rSonnet))
	for _, want := range []string{
		// (a) the dollar gap, attributed to the TTL guess, with CC's implied split
		fmt.Sprintf("%s: diff %+.4f", x, gap), "100% at 5m",
		// (d) the Workflow spend is a turn-less remainder no by-design overhead explains
		"not overhead by design",
	} {
		if !strings.Contains(flags, want) {
			t.Errorf("FLAGS lacks %q:\n%s", want, got)
		}
	}
	for _, clean := range []string{y, z} {
		if strings.Contains(flags, clean) {
			t.Errorf("FLAGS names the clean process %s:\n%s", clean, got)
		}
	}
	// (c) the unknown-TTL writes counted and priced both ways; (b) the remainder share.
	for _, want := range []string{
		fmt.Sprintf("unknown TTL: %d cache-write tokens", 305_815+1_173_395),
		fmt.Sprintf("$%.4f rests on the TTL", gap),
		"remainder (finality=derived)",
		sonnet,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
