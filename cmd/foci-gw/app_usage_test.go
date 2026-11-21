package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/fap"
	"foci/internal/usagelog"
)

// #3151: the app gets each time window with a current-period figure, and
// nothing for a window that reset since its figure, has no figure, or is not
// a time window.
func TestAppUsageFrom(t *testing.T) {
	now := time.Date(2026, 11, 17, 14, 0, 0, 0, time.UTC)
	cur := usagelog.Current{Windows: []usagelog.CurrentWindow{
		{Window: "five_hour", Length: 5 * time.Hour, HasPercent: true, Percent: 34.4, ResetsAt: now.Add(time.Hour), PercentReceivedAt: now.Add(-time.Minute)},
		{Window: "seven_day", Length: 7 * 24 * time.Hour, HasPercent: true, Percent: 77.6, ResetsAt: now.Add(48 * time.Hour)},
		{Window: "seven_day_opus", Length: 7 * 24 * time.Hour},                                                      // no figure this period
		{Window: "codex/5h", Length: 5 * time.Hour, HasPercent: true, Percent: 90, ResetsAt: now.Add(-time.Minute)}, // reset since
		{Window: "overage", HasPercent: true, Percent: 5},                                                           // not a time window
	}}
	u := appUsageFrom(cur, now)
	if u == nil || len(u.Windows) != 2 {
		t.Fatalf("appUsageFrom = %+v, want the 5h and 7d windows", u)
	}
	w0, w1 := u.Windows[0], u.Windows[1]
	if w0.Label != "5h" || w0.UsedPct != 34 || w0.ResetsMs != now.Add(time.Hour).UnixMilli() || w0.AsOfMs != now.Add(-time.Minute).UnixMilli() || w0.LengthMs != (5*time.Hour).Milliseconds() {
		t.Errorf("5h window = %+v", w0)
	}
	if w1.Label != "7d" || w1.UsedPct != 78 {
		t.Errorf("7d window = %+v", w1)
	}
	if appUsageFrom(usagelog.Current{}, now) != nil {
		t.Error("an account with no windows gives usage, want nil (no indicator)")
	}
	if appUsageFromBinding(nil) != nil {
		t.Error("no usage history view gives a source, want none")
	}
}

// #3151 end to end over a real usage history: a stored Claude Code read comes
// back as the app's usage for the agent.
func TestAppUsageFromBinding_StoredRead(t *testing.T) {
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	b := &usagelog.Binding{Store: st, Backend: "claude-code", Account: "acct", AgentID: "clutch"}
	b.RecordRead(&delegator.UsageInfo{
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: 34, ResetsAt: now.Add(2 * time.Hour), Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: 78, ResetsAt: now.Add(72 * time.Hour), Reported: true},
		Raw:                 []byte(`{}`),
	}, delegator.UsageSourceScheduled)
	u := appUsageFromBinding(b)()
	if u == nil || len(u.Windows) != 2 || u.Windows[0].Label != "5h" || u.Windows[0].UsedPct != 34 || u.Windows[1].Label != "7d" || u.Windows[1].UsedPct != 78 {
		t.Fatalf("usage from a stored read = %+v, want 5h 34%% then 7d 78%%", u)
	}
}

// #3253: one login sends what it sent before (no Accounts); two or more send
// the pick's windows as Windows and every login, in order, in Accounts, a
// login with no figure kept with empty windows; no figure anywhere sends nil.
func TestAppUsageOfLogins(t *testing.T) {
	fixed := func(pcts ...int) func() *fap.AgentUsage {
		return func() *fap.AgentUsage {
			if len(pcts) == 0 {
				return nil
			}
			var ws []fap.UsageWindow
			for i, p := range pcts {
				ws = append(ws, fap.UsageWindow{Label: []string{"5h", "7d"}[i], UsedPct: p})
			}
			return &fap.AgentUsage{Windows: ws}
		}
	}
	one := appUsageOfLogins([]usageLogin{{name: "a@x", picked: true, read: fixed(34, 78)}})
	if one == nil || len(one.Windows) != 2 || one.Accounts != nil {
		t.Fatalf("one login = %+v, want its windows and no accounts", one)
	}
	two := appUsageOfLogins([]usageLogin{
		{name: "a@x", read: fixed(34, 78)},
		{name: "b@x", picked: true, read: fixed(12, 40)},
		{name: "c@x", read: fixed()},
	})
	if two == nil || len(two.Windows) != 2 || two.Windows[0].UsedPct != 12 {
		t.Fatalf("Windows = %+v, want the pick's (b: 12/40)", two)
	}
	if len(two.Accounts) != 3 {
		t.Fatalf("Accounts = %+v, want 3 logins", two.Accounts)
	}
	a, b, c := two.Accounts[0], two.Accounts[1], two.Accounts[2]
	if a.Name != "a@x" || a.Picked || a.Windows[1].UsedPct != 78 {
		t.Errorf("login a = %+v", a)
	}
	if b.Name != "b@x" || !b.Picked {
		t.Errorf("login b = %+v, want the pick", b)
	}
	if c.Name != "c@x" || c.Windows == nil || len(c.Windows) != 0 {
		t.Errorf("login c = %+v, want kept with empty (non-nil) windows", c)
	}
	raw, _ := json.Marshal(c)
	if !strings.Contains(string(raw), `"windows":[]`) {
		t.Errorf("login c JSON = %s, want windows:[] (the Kotlin field is not optional)", raw)
	}
	none := appUsageOfLogins([]usageLogin{{name: "a@x", picked: true, read: fixed()}, {name: "b@x", read: nil}})
	if none != nil {
		t.Errorf("no figure on any login = %+v, want nil", none)
	}
}
