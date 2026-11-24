package app

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/fap"
)

// setUsageSource registers fn for agentID for the test's length.
func setUsageSource(t *testing.T, agentID string, fn func() *fap.AgentUsage) {
	t.Helper()
	SetAgentUsageSource(agentID, fn)
	t.Cleanup(func() {
		SetAgentUsageSource(agentID, nil)
		noteRosterUsage(nil)
	})
}

// #3151: the roster carries the allowance of an agent with a usage source,
// and nothing for one without (an older client ignores the field; the "if
// available" half of the ruling).
func TestRoster_CarriesAgentUsageWhereAvailable(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "cc")
	registerBareAgent(h, "api")
	registerBareAgent(h, "empty")
	setUsageSource(t, "cc", func() *fap.AgentUsage {
		return &fap.AgentUsage{Windows: []fap.UsageWindow{{Label: "5h", UsedPct: 34}, {Label: "7d", UsedPct: 78}}}
	})
	setUsageSource(t, "empty", func() *fap.AgentUsage { return nil })

	raw, err := json.Marshal(h.agentRoster())
	if err != nil {
		t.Fatal(err)
	}
	var got []fap.AgentInfo
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	by := map[string]fap.AgentInfo{}
	for _, a := range got {
		by[a.ID] = a
	}
	if u := by["cc"].Usage; u == nil || len(u.Windows) != 2 || u.Windows[1].UsedPct != 78 || u.Windows[0].Label != "5h" {
		t.Fatalf("cc usage = %+v, want 5h 34%% and 7d 78%%", u)
	}
	if by["api"].Usage != nil || by["empty"].Usage != nil {
		t.Fatalf("agents with no reading carry usage: api=%+v empty=%+v", by["api"].Usage, by["empty"].Usage)
	}
	// The field is omitted, not null, for an agent without a reading.
	for _, frag := range strings.Split(string(raw), `"id":`)[1:] {
		if strings.HasPrefix(frag, `"api"`) && strings.Contains(frag, `"usage"`) {
			t.Fatalf("api agent's JSON has a usage key: %s", frag)
		}
	}
}

// #3151: a stored read whose figures differ from the last roster re-sends the
// roster to live sockets; one that changes only AsOfMs sends nothing.
func TestAgentUsageChanged_ResendsRosterOnlyOnChange(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "cc")
	c := fakeClientForHub(h)
	h.clients[c] = struct{}{}
	activeMu.Lock()
	prev := activeHub
	activeHub = h
	activeMu.Unlock()
	t.Cleanup(func() { activeMu.Lock(); activeHub = prev; activeMu.Unlock() })

	var mu sync.Mutex
	used, asOf := 10, int64(1)
	setUsageSource(t, "cc", func() *fap.AgentUsage {
		mu.Lock()
		defer mu.Unlock()
		return &fap.AgentUsage{Windows: []fap.UsageWindow{{Label: "5h", UsedPct: used, AsOfMs: asOf}}}
	})
	h.pushHelloAll() // every socket now holds the current figures
	drain(t, c)

	mu.Lock()
	asOf = 2
	mu.Unlock()
	AgentUsageChanged()
	for _, d := range drain(t, c) {
		if d.t == fap.TypeHello {
			t.Fatalf("a read that moved only AsOfMs re-sent the roster")
		}
	}

	mu.Lock()
	used = 11
	mu.Unlock()
	AgentUsageChanged()
	var hellos int
	for _, d := range drain(t, c) {
		if d.t == fap.TypeHello {
			hellos++
		}
	}
	if hellos != 1 {
		t.Fatalf("hello frames after a changed figure = %d, want 1", hellos)
	}
}

// #3253: for an agent on two or more logins, a change only in another
// login's figures, or only in which login is the pick, re-sends the roster;
// a read that moves only a login's AsOfMs does not.
func TestSameShownUsage_PerLogin(t *testing.T) {
	mk := func(aPct int, bAsOf int64, pickB bool) map[string]*fap.AgentUsage {
		a := fap.AccountUsage{Name: "a@x", Picked: !pickB, Windows: []fap.UsageWindow{{Label: "5h", UsedPct: aPct}}}
		b := fap.AccountUsage{Name: "b@x", Picked: pickB, Windows: []fap.UsageWindow{{Label: "5h", UsedPct: 12, AsOfMs: bAsOf}}}
		top := a.Windows
		if pickB {
			top = b.Windows
		}
		return map[string]*fap.AgentUsage{"cam": {Windows: top, Accounts: []fap.AccountUsage{a, b}}}
	}
	base := mk(34, 1, true)
	if !sameShownUsage(base, mk(34, 2, true)) {
		t.Error("a login's AsOfMs alone counted as a change")
	}
	if sameShownUsage(base, mk(35, 1, true)) {
		t.Error("another login's figure changing (the pick's unchanged) did not count")
	}
	if sameShownUsage(base, mk(34, 1, false)) {
		t.Error("the pick moving to another login did not count")
	}
	over := mk(34, 1, true)
	over["cam"].OverPace = true
	if sameShownUsage(base, over) {
		t.Error("the agent going over pace did not count (#3267)")
	}
}

// #3253: an agent whose logins carry figures only in Accounts (the pick has
// none yet) still gets usage in the roster.
func TestRoster_CarriesAccountsWithoutPickFigures(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "cam")
	setUsageSource(t, "cam", func() *fap.AgentUsage {
		return &fap.AgentUsage{Accounts: []fap.AccountUsage{
			{Name: "a@x", Picked: true, Windows: []fap.UsageWindow{}},
			{Name: "b@x", Windows: []fap.UsageWindow{{Label: "5h", UsedPct: 50}}},
		}}
	})
	var got *fap.AgentUsage
	for _, a := range h.agentRoster() {
		if a.ID == "cam" {
			got = a.Usage
		}
	}
	if got == nil || len(got.Accounts) != 2 || got.Accounts[1].Windows[0].UsedPct != 50 {
		t.Fatalf("cam usage = %+v, want both logins", got)
	}
}
