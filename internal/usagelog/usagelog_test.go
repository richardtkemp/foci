package usagelog

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

func f(v float64) *float64 { return &v }

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage-history.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// day is a fixed day in UTC; at(h, m) is a time on it.
var day = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func at(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func save(t *testing.T, st *Store, r delegator.UsageReport) {
	t.Helper()
	if r.Backend == "" {
		r.Backend = "claude-code"
	}
	if r.AgentID == "" {
		r.AgentID = "clutch"
	}
	if err := st.Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func window(c Current, name string) (CurrentWindow, bool) {
	for _, w := range c.Windows {
		if w.Window == name {
			return w, true
		}
	}
	return CurrentWindow{}, false
}

// TestSaveKeepsEveryField (requirement 6): a stored report reads back with
// its received time, backend, account, agent, source, every window's figures
// as reported (percent or fraction, reset, status, limit state) and the full
// payload.
func TestSaveKeepsEveryField(t *testing.T) {
	st, _ := openTemp(t)
	payload := json.RawMessage(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_opus","utilization":1,"resetsAt":1763200000,"overageStatus":"rejected"}}`)
	in := delegator.UsageReport{
		Source: delegator.UsageSourceCCEvent, Payload: payload,
		ReceivedAt: at(20, 40).Add(123 * time.Millisecond), Backend: "claude-code", Account: "claude-code:acctA", AgentID: "clutch",
		Windows: []delegator.UsageReportWindow{{
			Window: "seven_day_opus", Length: 7 * 24 * time.Hour, Used: f(1), UsedUnit: delegator.UsageUnitFraction,
			ResetsAt: time.Unix(1763200000, 0), Status: "rejected", LimitReached: true,
		}, {Window: "five_hour", Length: 5 * time.Hour, Status: "allowed"}},
	}
	if err := st.Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.Reports("claude-code:acctA")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reports, want 1", len(got))
	}
	r := got[0]
	if !r.ReceivedAt.Equal(in.ReceivedAt) || r.Backend != in.Backend || r.Account != in.Account || r.AgentID != in.AgentID || r.Source != in.Source {
		t.Errorf("report header = %+v, want %+v", r.UsageReport, in)
	}
	if string(r.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", r.Payload, payload)
	}
	if len(r.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", r.Windows)
	}
	w := r.Windows[0]
	if w.Window != "seven_day_opus" || w.Length != 7*24*time.Hour || w.Used == nil || *w.Used != 1 || w.UsedUnit != delegator.UsageUnitFraction ||
		!w.ResetsAt.Equal(time.Unix(1763200000, 0)) || w.Status != "rejected" || !w.LimitReached {
		t.Errorf("window 0 = %+v", w)
	}
	if w := r.Windows[1]; w.Used != nil || w.Status != "allowed" || w.LimitReached {
		t.Errorf("window 1 (no use reported) = %+v", w)
	}
}

// TestCurrent_PercentlessEventKeepsPercent (requirement 2, H4 first half): a
// later "allowed" event with no utilization does not hide the newest percent
// figure, and the figure keeps its own received time.
func TestCurrent_PercentlessEventKeepsPercent(t *testing.T) {
	st, _ := openTemp(t)
	reset := at(23, 0)
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(20, 40), Account: "A",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Length: 5 * time.Hour, Used: f(30), UsedUnit: delegator.UsageUnitPercent, ResetsAt: reset}}})
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceCCEvent, ReceivedAt: at(20, 55), Account: "A",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Length: 5 * time.Hour, ResetsAt: reset, Status: "allowed"}}})

	c, err := st.Current("A", at(21, 0))
	if err != nil {
		t.Fatal(err)
	}
	w, ok := window(c, "five_hour")
	if !ok {
		t.Fatalf("no five_hour window in %+v", c)
	}
	if !w.HasPercent || w.Percent != 30 || !w.PercentReceivedAt.Equal(at(20, 40)) || w.PercentSource != delegator.UsageSourceScheduled {
		t.Errorf("percent = %v %v at %v (%s), want 30 at 20:40 (scheduled)", w.HasPercent, w.Percent, w.PercentReceivedAt, w.PercentSource)
	}
	if !w.ResetsAt.Equal(reset) || !w.ReceivedAt.Equal(at(20, 55)) || w.Status != "allowed" {
		t.Errorf("newest report = reset %v received %v status %q, want 23:00, 20:55, allowed", w.ResetsAt, w.ReceivedAt, w.Status)
	}
}

// TestCurrent_PercentFromBeforeResetIsNotCurrent (requirement 2, H4 second
// half): 80% measured at 17:50 for the window resetting at 18:00 is not
// shown as the use of the window that resets at 23:00.
func TestCurrent_PercentFromBeforeResetIsNotCurrent(t *testing.T) {
	st, _ := openTemp(t)
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(17, 50), Account: "B",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Length: 5 * time.Hour, Used: f(80), UsedUnit: delegator.UsageUnitPercent, ResetsAt: at(18, 0)}}})
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceCCEvent, ReceivedAt: at(18, 5), Account: "B",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Length: 5 * time.Hour, ResetsAt: at(23, 0), Status: "allowed"}}})

	c, err := st.Current("B", at(18, 10))
	if err != nil {
		t.Fatal(err)
	}
	w, ok := window(c, "five_hour")
	if !ok {
		t.Fatalf("no five_hour window in %+v", c)
	}
	if w.HasPercent {
		t.Errorf("percent %v shown for the window resetting at %v; want none held", w.Percent, w.ResetsAt)
	}
	if !w.ResetsAt.Equal(at(23, 0)) {
		t.Errorf("reset = %v, want 23:00", w.ResetsAt)
	}

	// With no later report, a figure whose window has reset is not current
	// either.
	c, _ = st.Current("B", at(18, 1))
	if w, _ := window(c, "five_hour"); w.HasPercent && w.ResetsAt.Equal(at(18, 0)) {
		t.Errorf("figure of a window that reset at 18:00 shown at 18:01")
	}
}

// TestCurrent_LimitReachedFromNewerReport: a newer report that says the
// window's limit is reached is what the current state shows.
func TestCurrent_LimitReachedFromNewerReport(t *testing.T) {
	st, _ := openTemp(t)
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(20, 0), Account: "A",
		Windows: []delegator.UsageReportWindow{{Window: "seven_day", Used: f(97), UsedUnit: delegator.UsageUnitPercent, ResetsAt: at(23, 0)}}})
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceCCEvent, ReceivedAt: at(20, 30), Account: "A",
		Windows: []delegator.UsageReportWindow{{Window: "seven_day", Used: f(1), UsedUnit: delegator.UsageUnitFraction, ResetsAt: at(23, 0), Status: "rejected", LimitReached: true}}})
	c, _ := st.Current("A", at(21, 0))
	w, _ := window(c, "seven_day")
	if !w.LimitReached || w.Status != "rejected" || w.Percent != 100 || w.Length != 7*24*time.Hour {
		t.Errorf("seven_day = %+v, want limit reached at 100%% (fraction scaled), 7d", w)
	}
}

// TestCurrent_SurvivesReopen (requirement 10): the figures read back after
// the store is closed and opened again, with their original received times.
func TestCurrent_SurvivesReopen(t *testing.T) {
	st, path := openTemp(t)
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(9, 0), Account: "A",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Used: f(42), UsedUnit: delegator.UsageUnitPercent, ResetsAt: at(12, 0)}}})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	c, err := st2.Current("A", at(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := window(c, "five_hour")
	if !w.HasPercent || w.Percent != 42 || !w.PercentReceivedAt.Equal(at(9, 0)) {
		t.Errorf("after reopen: %+v, want 42%% received 09:00", w)
	}
}

// TestCurrent_AccountsDoNotMix (requirement 7): figures of one account never
// appear in another's state.
func TestCurrent_AccountsDoNotMix(t *testing.T) {
	st, _ := openTemp(t)
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(9, 0), Account: "claude-code:A",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Used: f(11), UsedUnit: delegator.UsageUnitPercent}}})
	save(t, st, delegator.UsageReport{Source: delegator.UsageSourceScheduled, ReceivedAt: at(9, 5), Account: "claude-code:B",
		Windows: []delegator.UsageReportWindow{{Window: "five_hour", Used: f(77), UsedUnit: delegator.UsageUnitPercent}}})
	a, _ := st.Current("claude-code:A", at(10, 0))
	b, _ := st.Current("claude-code:B", at(10, 0))
	if w, _ := window(a, "five_hour"); w.Percent != 11 {
		t.Errorf("A = %+v, want 11", w)
	}
	if w, _ := window(b, "five_hour"); w.Percent != 77 {
		t.Errorf("B = %+v, want 77", w)
	}
	none, _ := st.Current("claude-code:C", at(10, 0))
	if len(none.Windows) != 0 || none.Backend != "" {
		t.Errorf("unknown account = %+v, want nothing", none)
	}
}

func TestAccountKey(t *testing.T) {
	if got := AccountKey("claude-code", "clutch", "claude-code:abc"); got != "claude-code:abc" {
		t.Errorf("CC key = %q", got)
	}
	if a, b := AccountKey("codex", "x", ""), AccountKey("codex", "y", ""); a == b {
		t.Errorf("two codex agents share key %q", a)
	}
}

// TestReportFromRead: a fresh read is a report; a codex /usage with no fresh
// read is not (its figures were stored when codex pushed them).
func TestReportFromRead(t *testing.T) {
	cc := &delegator.UsageInfo{Backend: "Claude Code", Raw: json.RawMessage(`{"x":1}`),
		FiveHour: delegator.UsageWindow{Percent: 30, Reported: true, ResetsAt: at(23, 0)}}
	r, ok := ReportFromRead(cc, delegator.UsageSourceMana)
	if !ok || len(r.Windows) != 1 || r.Windows[0].Window != "five_hour" || string(r.Payload) != `{"x":1}` {
		t.Errorf("CC read report = %+v, %v", r, ok)
	}
	stale := &delegator.UsageInfo{Backend: "Codex", Allowance: &delegator.Allowance{Fresh: false}}
	if _, ok := ReportFromRead(stale, delegator.UsageSourceMana); ok {
		t.Error("a codex reply with no fresh read became a report")
	}
}
