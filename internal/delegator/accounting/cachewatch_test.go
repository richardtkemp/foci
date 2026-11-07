package accounting

import (
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

const watchModel = "claude-opus-5-5"

// day7 is 7 Nov 2026, the day the #2930 facts come from.
var day7 = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

func at(hms string) time.Time {
	d, err := time.ParseDuration(hms)
	if err != nil {
		panic(err)
	}
	return day7.Add(d)
}

// cc is one Claude Code call as the ledger books it: cache writes 1h.
func cc(id int64, session, actor, kind string, when time.Time, read, write int) Booking {
	return Booking{ID: id, Call: Call{
		Backend: BackendCCStream, Model: watchModel, Session: session, Actor: actor,
		Kind: kind, BilledAt: when,
		Tokens: modelinfo.Tokens{modelinfo.ClassCacheRead: read, modelinfo.ClassCacheWrite1h: write, modelinfo.ClassInput: 2},
	}}
}

// run observes each booking on w and returns the reports.
func run(w *cacheWatch, bs ...Booking) []CacheReport {
	var out []CacheReport
	for _, b := range bs {
		if r, ok := w.observe(b); ok {
			out = append(out, r)
		}
	}
	return out
}

func wantNone(t *testing.T, rs []CacheReport) {
	t.Helper()
	for _, r := range rs {
		t.Errorf("unexpected report: %s", r)
	}
}

func wantOne(t *testing.T, rs []CacheReport, class, cause string) CacheReport {
	t.Helper()
	if len(rs) != 1 {
		for _, r := range rs {
			t.Logf("report: %s", r)
		}
		t.Fatalf("got %d reports, want 1 %s/%s", len(rs), class, cause)
	}
	if rs[0].Class != class || rs[0].Cause != cause {
		t.Fatalf("got %s/%s, want %s/%s: %s", rs[0].Class, rs[0].Cause, class, cause, rs[0])
	}
	return rs[0]
}

// A lineage whose every call reads the previous call's read plus write, and
// one whose calls also write a lot (a write alone is never a bust), report
// nothing (#2930 req 2).
func TestCacheWatch_ContinuousLineageAndLargeWritesAreSilent(t *testing.T) {
	w := newCacheWatch()
	wantNone(t, run(w,
		cc(1, "a/c1", "", KindCall, at("3h40m26s"), 12199, 60675),
		cc(2, "a/c1", "", KindCall, at("3h40m46s"), 72874, 13002),
		cc(3, "a/c1", "", KindCall, at("3h40m54s"), 85876, 250000),
		cc(4, "a/c1", "", KindCall, at("3h41m10s"), 335876, 10),
	))
}

// A read well below what the request should have read, inside the TTL, is a
// bust; the report names the session, expected and actual read, the lost
// tokens and the rewrite's cost (#2930 req 5).
func TestCacheWatch_DroppedReadIsABust(t *testing.T) {
	w := newCacheWatch()
	rs := run(w,
		cc(1, "a/c1", "", KindCall, at("8h20m5s"), 396209, 936),
		cc(2, "a/c1", "", KindCall, at("8h20m7s"), 397145, 633),
		cc(3, "a/c1", "", KindCall, at("8h30m0s"), 46847, 353000),
	)
	r := wantOne(t, rs, CacheClassBust, CacheCausePrefixChanged)
	if r.Expected != 397778 || r.Read != 46847 || r.Lost != 397778-46847 || r.Session != "a/c1" {
		t.Errorf("figures: %+v", r)
	}
	if !r.Priced || r.RewriteUSD <= r.ReadUSD || r.RewriteUSD <= 0 {
		t.Errorf("cost: rewrite %v read %v priced %v", r.RewriteUSD, r.ReadUSD, r.Priced)
	}
	for _, want := range []string{"cache bust", "a/c1", "397778", "46847", "rewrite $", "prefix_changed"} {
		if !strings.Contains(r.String(), want) {
			t.Errorf("report line lacks %q: %s", want, r)
		}
	}
}

// A shortfall inside the slack (a fork re-rendering a few thousand tail
// tokens) is not reported; one just over it is.
func TestCacheWatch_Slack(t *testing.T) {
	w := newCacheWatch()
	wantNone(t, run(w,
		cc(1, "a/c1", "", KindCall, at("1h"), 100000, 0),
		cc(2, "a/c1", "", KindCall, at("1h1m"), 95000, 0),
	))
	wantOne(t, run(w, cc(3, "a/c1", "", KindCall, at("1h2m"), 90000, 0)), CacheClassBust, CacheCausePrefixChanged)
}

// A read lost because the TTL elapsed since the last touch is an expiry, not
// a bust (#2930 req 4): 05:01 7 Nov, the Temporal chat's fork 61 minutes after
// its source's last call on a 1h cache.
func TestCacheWatch_ExpiryIsReportedApart(t *testing.T) {
	w := newCacheWatch()
	run(w, cc(1, "clutch/c45", "", KindCall, at("4h0m7s"), 125408, 1232))
	w.noteFork("clutch/c45/b1", "clutch/c45", at("5h1m0s"))
	r := wantOne(t, run(w, cc(2, "clutch/c45/b1", "", KindCall, at("5h1m6s"), 12199, 118655)), CacheClassExpiry, CacheCauseExpiry)
	if r.Source != "clutch/c45" || r.TTL != time.Hour || r.Since < time.Hour || !r.Priced {
		t.Errorf("expiry report: %+v", r)
	}
	// A 5m cache expires after 5 minutes.
	w2 := newCacheWatch()
	b1 := cc(1, "a/c1", "", KindCall, at("1h"), 50000, 0)
	b1.Tokens = modelinfo.Tokens{modelinfo.ClassCacheRead: 50000, modelinfo.ClassCacheWrite5m: 1000}
	run(w2, b1)
	wantOne(t, run(w2, cc(2, "a/c1", "", KindCall, at("1h6m"), 0, 51000)), CacheClassExpiry, CacheCauseExpiry)
}

// 03:43 7 Nov: depth leaves forked from a parent just after its compaction
// read 85,876 of the parent's 88,465. The old check compared with the
// pre-compaction 202k read and warned five times; this one is silent. A
// source that keeps working after the fork does not raise the bar.
func TestCacheWatch_ForkAfterCompactionIsSilent(t *testing.T) {
	w := newCacheWatch()
	root := "clutch/c67"
	wantNone(t, run(w,
		cc(1, root, "", KindCall, at("3h38m40s"), 202224, 1252),
		cc(2, root, "", KindCompaction, at("3h40m21s"), 202224, 933),
		cc(3, root, "", KindCall, at("3h40m26s"), 12199, 60675),
		cc(4, root, "", KindCall, at("3h40m46s"), 72874, 13002),
		cc(5, root, "", KindCall, at("3h40m54s"), 85876, 2589),
	))
	for _, br := range []string{"/b1", "/b2", "/b3"} {
		w.noteFork(root+br, root, at("3h43m"))
	}
	run(w, cc(6, root, "", KindCall, at("3h43m10s"), 88465, 30000)) // the source moves on
	wantNone(t, run(w,
		cc(7, root+"/b1", "", KindCall, at("3h43m25s"), 85876, 5001),
		cc(8, root+"/b2", "", KindCall, at("3h44m6s"), 85876, 5030),
		cc(9, root+"/b3", "", KindCall, at("3h44m25s"), 85876, 5245),
		cc(10, root+"/b1", "", KindCall, at("3h43m28s"), 90877, 14468),
	))
}

// 09:15-14:12 7 Nov: each hourly keepalive fork of the clutch root missed
// its cache — the first read 0, later ones 46,847 — a real bust every time
// (#2929). Each fork's first call touched the root's prefix, so the next fork
// 56 minutes later is a bust, not an expiry.
func TestCacheWatch_KeepaliveForkMisses(t *testing.T) {
	w := newCacheWatch()
	root := "clutch/c67"
	run(w,
		cc(1, root, "", KindCall, at("8h20m5s"), 396209, 936),
		cc(2, root, "", KindCall, at("8h20m7s"), 397145, 633),
	)
	w.noteFork(root+"/b915", root, at("9h15m30s"))
	r := wantOne(t, run(w, cc(3, root+"/b915", "", KindCall, at("9h15m36s"), 0, 399927)), CacheClassBust, CacheCauseForkMiss)
	if r.Expected != 397778 || r.Read != 0 || r.Source != root {
		t.Errorf("first keepalive: %+v", r)
	}
	// $3.20 per the ledger's own pricing of 399,927 1h writes; the lost
	// 397,778 cost slightly less.
	if r.RewriteUSD < 2.5 || r.RewriteUSD > 3.5 {
		t.Errorf("rewrite cost %.2f, want about $3.18", r.RewriteUSD)
	}
	w.noteFork(root+"/b1011", root, at("10h11m"))
	r = wantOne(t, run(w, cc(4, root+"/b1011", "", KindCall, at("10h11m10s"), 46847, 353000)), CacheClassBust, CacheCauseForkMiss)
	if r.Read != 46847 {
		t.Errorf("second keepalive: %+v", r)
	}
}

// A prompt change and a model switch that drop the read are busts, reported
// with that cause (Dick, 2026-11-07: "A cache bust on a model switch or a
// prompt change is not a false alarm!").
func TestCacheWatch_ModelSwitchAndPromptChangeAreBusts(t *testing.T) {
	w := newCacheWatch()
	w.notePrompt("a/c1", "h1")
	run(w, cc(1, "a/c1", "", KindCall, at("1h"), 100000, 500))
	w.notePrompt("a/c1", "h2") // the backend relaunched with a new prompt
	wantOne(t, run(w, cc(2, "a/c1", "", KindCall, at("1h1m"), 12000, 89000)), CacheClassBust, CacheCausePromptChanged)

	b := cc(3, "a/c1", "", KindCall, at("1h2m"), 0, 101500)
	b.Model = "claude-sonnet-5"
	r := wantOne(t, run(w, b), CacheClassBust, CacheCauseModelSwitch)
	if r.PrevModel != watchModel || !strings.Contains(r.String(), watchModel+" -> claude-sonnet-5") {
		t.Errorf("model switch report: %s", r)
	}

	// A fork launched with a prompt other than its source's (#2051).
	w.notePrompt("a/c1/b1", "h9")
	w.noteFork("a/c1/b1", "a/c1", at("1h3m"))
	wantOne(t, run(w, func() Booking {
		b := cc(4, "a/c1/b1", "", KindCall, at("1h3m5s"), 40000, 70000)
		b.Model = "claude-sonnet-5"
		return b
	}()), CacheClassBust, CacheCausePromptChanged)
}

// A compaction or a deliberate reset starts a new lineage: its first call is
// not checked (#2930 req 3).
func TestCacheWatch_CompactionAndResetStartNewLineages(t *testing.T) {
	w := newCacheWatch()
	wantNone(t, run(w,
		cc(1, "a/c1", "", KindCall, at("1h"), 200000, 2000),
		cc(2, "a/c1", "", KindCompaction, at("1h1m"), 202000, 900),
		cc(3, "a/c1", "", KindCall, at("1h1m5s"), 12199, 50000),
	))
	w.noteReset("a/c1", at("1h2m"))
	wantNone(t, run(w,
		cc(4, "a/c1", "", KindCall, at("1h1m30s"), 62199, 100), // booked late, from before the reset
		cc(5, "a/c1", "", KindCall, at("1h3m"), 12199, 30000),
		cc(6, "a/c1", "", KindCall, at("1h3m5s"), 42199, 100),
	))
}

// A subagent is its own lineage: its calls interleaved with the session's own
// are no bust of either (2026-11-01..07: 517 such false drops when keyed on
// the session alone).
func TestCacheWatch_SubagentsAreTheirOwnLineage(t *testing.T) {
	w := newCacheWatch()
	wantNone(t, run(w,
		cc(1, "a/c1", "", KindCall, at("1h"), 150000, 1000),
		cc(2, "a/c1", "toolu_1", KindCall, at("1h0m1s"), 0, 20000),
		cc(3, "a/c1", "", KindCall, at("1h0m2s"), 151000, 300),
		cc(4, "a/c1", "toolu_1", KindCall, at("1h0m3s"), 20000, 900),
		cc(5, "a/c1", "toolu_2", KindCall, at("1h0m4s"), 0, 18000),
	))
}

// The same call observed again (an extended subagent share) or a call with no
// cache tokens is not a new call of the lineage.
func TestCacheWatch_RepeatsAndUsagelessCallsAreIgnored(t *testing.T) {
	w := newCacheWatch()
	b := cc(1, "a/c1", "", KindCall, at("1h"), 100000, 500)
	wantNone(t, run(w, b, b))
	none := cc(2, "a/c1", "", KindCall, at("1h1m"), 0, 0)
	wantNone(t, run(w, none, cc(3, "a/c1", "", KindCall, at("1h2m"), 100500, 10)))
	other := cc(4, "a/c1", "", KindSpawn, at("1h3m"), 0, 5000)
	wantNone(t, run(w, other, cc(5, "a/c1", "", KindCall, at("1h4m"), 100510, 10)))
}

// After a restart the watch holds nothing: a session's first call is checked
// against its last call in api.db, and a compaction there starts a new
// lineage. End to end through Record and the hook.
func TestCacheWatch_SeedsFromTheLedgerAfterARestart(t *testing.T) {
	l, _ := openLedger(t)
	useLive(t, l)
	var got []CacheReport
	CacheReportHook = func(r CacheReport) { got = append(got, r) }
	t.Cleanup(func() { CacheReportHook = nil })

	book := func(id, session, kind string, when time.Time, read, write int) {
		t.Helper()
		r := APIResponse{ID: id, Kind: kind, Provider: "anthropic", Model: watchModel,
			Session: session, AgentID: "a", TurnID: session + "@1", Start: when, StopReason: "end_turn",
			Tokens: modelinfo.Tokens{modelinfo.ClassInput: 2, modelinfo.ClassCacheRead: read, modelinfo.ClassCacheWrite1h: write}}
		tn := Turn{TurnID: session + "@1", Session: session, AgentID: "a", Backend: BackendAPI, Source: SourceUser, StartedAt: when}
		if err := Record(tn, r.Call()); err != nil {
			t.Fatalf("Record %s: %v", id, err)
		}
	}
	book("m1", "seed/c1", KindCall, at("1h"), 100000, 2000)
	book("m2", "seed/c2", KindCall, at("1h"), 100000, 2000)
	book("m3", "seed/c2", KindCompaction, at("1h1m"), 102000, 500)

	saved := watch
	watch = newCacheWatch() // the restart
	t.Cleanup(func() { watch = saved })

	book("m4", "seed/c1", KindCall, at("1h2m"), 12000, 90000)
	book("m5", "seed/c2", KindCall, at("1h2m"), 12000, 40000)
	if len(got) != 1 || got[0].Session != "seed/c1" || got[0].Expected != 102000 || got[0].Class != CacheClassBust {
		t.Fatalf("reports after a restart: %+v", got)
	}
}

// After a restart, NoteCacheFork's baseline for the agent's warm decision
// (#2932) is the source's last call in api.db; 0 when the last one there is a
// compaction, or the source has none.
func TestNoteCacheFork_ReturnsTheSourcesReadAfterARestart(t *testing.T) {
	l, _ := openLedger(t)
	useLive(t, l)
	book := func(id, session, kind string, when time.Time, read, write int) {
		t.Helper()
		r := APIResponse{ID: id, Kind: kind, Provider: "anthropic", Model: watchModel,
			Session: session, AgentID: "a", TurnID: session + "@1", Start: when, StopReason: "end_turn",
			Tokens: modelinfo.Tokens{modelinfo.ClassInput: 2, modelinfo.ClassCacheRead: read, modelinfo.ClassCacheWrite1h: write}}
		tn := Turn{TurnID: session + "@1", Session: session, AgentID: "a", Backend: BackendCCStream, Source: SourceUser, StartedAt: when}
		if err := Record(tn, r.Call()); err != nil {
			t.Fatalf("Record %s: %v", id, err)
		}
	}
	now := time.Now()
	book("f1", "fork/c1", KindCall, now.Add(-50*time.Minute), 12199, 44744)
	book("f2", "fork/c2", KindCall, now.Add(-50*time.Minute), 90000, 100)
	book("f3", "fork/c2", KindCompaction, now.Add(-49*time.Minute), 90100, 500)

	saved := watch
	watch = newCacheWatch() // the restart
	t.Cleanup(func() { watch = saved })

	if got := NoteCacheFork("fork/c1/b1", "fork/c1"); got != 12199 {
		t.Errorf("source's read after a restart = %d, want 12199 from api.db", got)
	}
	if got := NoteCacheFork("fork/c2/b1", "fork/c2"); got != 0 {
		t.Errorf("source just compacted: read = %d, want 0", got)
	}
	if got := NoteCacheFork("fork/c3/b1", "fork/c3"); got != 0 {
		t.Errorf("source with no calls: read = %d, want 0", got)
	}
}

// Lineages untouched past every TTL are pruned; a pruned lineage's next call
// is re-seeded (here: none, no ledger) and the watch stays bounded.
func TestCacheWatch_PrunesStaleLineages(t *testing.T) {
	w := newCacheWatch()
	w.notePrompt("old/c1", "h")
	run(w, cc(1, "old/c1", "toolu_1", KindCall, at("1h"), 0, 5000))
	w.notePrompt("new/c1", "h")
	w.notePrompt("gone/c1", "h")
	for i := 0; i < cachePruneEvery; i++ {
		run(w, cc(int64(10+i), "new/c1", "", KindCall, at("4h").Add(time.Duration(i)*time.Second), 1000+i, 1))
	}
	if _, ok := w.lineages[lineageKey("old/c1", "toolu_1")]; ok {
		t.Error("a lineage untouched for 3h was not pruned")
	}
	if _, ok := w.lineages[lineageKey("new/c1", "")]; !ok {
		t.Error("a live lineage was pruned")
	}
	if _, ok := w.prompts["new/c1"]; !ok {
		t.Error("a live session's prompt was pruned")
	}
	if _, ok := w.prompts["gone/c1"]; ok {
		t.Error("the prompt of a session with no lineage was kept")
	}
}
