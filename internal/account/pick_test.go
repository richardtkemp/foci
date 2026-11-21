package account

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/usagehold"
	"foci/internal/usagelog"
)

// pickFixture is a pool's accounts over one usage history and one hold
// engine, as the gateway builds them.
type pickFixture struct {
	t     *testing.T
	hist  *usagelog.Store
	holds *usagehold.Registry
	accts map[string]*Account
}

func newPickFixture(t *testing.T, keys ...string) (*pickFixture, []*Account) {
	t.Helper()
	st, err := usagelog.Open(filepath.Join(t.TempDir(), usagelog.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &pickFixture{t: t, hist: st, holds: usagehold.New(), accts: map[string]*Account{}}
	var out []*Account
	for _, k := range keys {
		a := New(Options{Backend: "claude-code", Spec: delegator.AccountSpec{Key: k, Name: k}, Holds: f.holds, Login: &relogin.Gate{},
			History: func() *usagelog.Store { return st }})
		f.accts[k] = a
		out = append(out, a)
	}
	return f, out
}

// report stores one report of key's windows (percent figures), received now.
func (f *pickFixture) report(key string, windows map[string]float64) {
	f.t.Helper()
	r := delegator.UsageReport{Source: "test", ReceivedAt: time.Now(), Backend: "claude-code", Account: key, AgentID: "agent"}
	for name, pct := range windows {
		v := pct
		r.Windows = append(r.Windows, delegator.UsageReportWindow{Window: name, Used: &v, UsedUnit: delegator.UsageUnitPercent,
			ResetsAt: time.Now().Add(3 * time.Hour)})
	}
	if err := f.hist.Save(r); err != nil {
		f.t.Fatal(err)
	}
}

// hold puts key on a hold (reached: a stop) that lifts at lift.
func (f *pickFixture) hold(key string, reached bool, lift time.Time) {
	f.holds.Observe(key, usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, Reached: reached, ResetAt: lift}, time.Now())
}

// TestPick_MostUsageLeft (#3186 F2/F3, scored by rule T-profile since
// #3247): a new top-level session takes the account with the most allowance
// left per expected demand until its windows reset; an account with no
// figure counts as unused; a tie goes to the first in the list. Every window
// here resets in 3 h and there is no history, so the score is allowance left
// per hour, the 5h one at the default ratio 0.1 of a 7d one: a min(9, 20),
// b min(6, 70), c min(4, 95) (/3 h).
func TestPick_MostUsageLeft(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b", "c")
	p := NewPool(memStore{}, accts...)
	f.report("a", map[string]float64{"five_hour": 10, "seven_day": 80})
	f.report("b", map[string]float64{"five_hour": 40, "seven_day": 30})
	f.report("c", map[string]float64{"five_hour": 60, "seven_day": 5})
	if got := p.For("agent/c1"); got != f.accts["a"] {
		t.Fatalf("new session on %s, want a (5h left per hour: a 3, b 2, c 1.3)", got.Key())
	}

	f2, accts2 := newPickFixture(t, "a", "b", "c")
	f2.report("a", map[string]float64{"five_hour": 95})
	p2 := NewPool(memStore{}, accts2...)
	if got := p2.For("agent/c1"); got != f2.accts["b"] {
		t.Errorf("new session on %s, want b (no figure = unused; the tie with c goes to the first)", got.Key())
	}
}

// TestPick_KeepsTheAccountForLife (#3186 F2): once bound, a session keeps its
// account when the usage moves, in this pool and after a restart.
func TestPick_KeepsTheAccountForLife(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	st := memStore{}
	p := NewPool(st, accts...)
	f.report("a", map[string]float64{"five_hour": 90})
	if got := p.For("agent/c1"); got != f.accts["b"] {
		t.Fatalf("new session on %s, want b", got.Key())
	}
	f.report("b", map[string]float64{"five_hour": 95})
	f.report("a", map[string]float64{"five_hour": 1})
	if got := p.For("agent/c1"); got != f.accts["b"] {
		t.Errorf("session moved to %s when the usage changed", got.Key())
	}
	if got := NewPool(st, accts...).For("agent/c1"); got != f.accts["b"] {
		t.Errorf("after a restart the session is on %s, want b", got.Key())
	}
	if got := p.For("agent/c2"); got != f.accts["a"] {
		t.Errorf("a new session takes %s, want a (now the least used)", got.Key())
	}
}

// TestPick_PassesOverHeldAccounts (#3186 F4): an account that holds or stops
// is passed over while another is open, however used; when every account
// holds or stops, the one whose hold lifts first wins.
func TestPick_PassesOverHeldAccounts(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b", "c")
	p := NewPool(memStore{}, accts...)
	now := time.Now()
	f.hold("a", false, now.Add(4*time.Hour))
	f.report("b", map[string]float64{"seven_day": 95})
	f.hold("c", true, now.Add(time.Hour))
	if got := p.For("agent/c1"); got != f.accts["b"] {
		t.Fatalf("new session on %s, want b, the one open account", got.Key())
	}
	f.hold("b", false, now.Add(2*time.Hour))
	if got := p.For("agent/c2"); got != f.accts["c"] {
		t.Errorf("all held: new session on %s, want c (lifts first)", got.Key())
	}
}

// TestPick_SkipsAccountsOwingALogin (#3186 F5): an account that owes a
// login is passed over while another does not; when all owe one, the first
// is taken.
func TestPick_SkipsAccountsOwingALogin(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	p := NewPool(memStore{}, accts...)
	f.report("b", map[string]float64{"five_hour": 90})
	if !f.accts["a"].Login().StartAuto() {
		t.Fatal("could not start a's login")
	}
	if got := p.For("agent/c1"); got != f.accts["b"] {
		t.Fatalf("new session on %s, want b (a owes a login)", got.Key())
	}
	f.accts["b"].Login().StartAuto()
	if got := p.For("agent/c2"); got != f.accts["a"] {
		t.Errorf("both owe a login: new session on %s, want the first", got.Key())
	}
}

// TestScore_ReachedAndOldPeriods: a window reached in its current period
// scores 0; a figure from before its window reset does not count (the
// account scores as unused).
func TestScore_ReachedAndOldPeriods(t *testing.T) {
	f, _ := newPickFixture(t, "a")
	a := f.accts["a"]
	now := time.Now()
	d := buildDemand([]*Account{a}, now)
	unused := d.score(a, now)
	if unused <= 0 {
		t.Fatalf("unused score = %v", unused)
	}
	old := 70.0
	if err := f.hist.Save(delegator.UsageReport{Source: "t", ReceivedAt: now.Add(-6 * time.Hour), Backend: "claude-code", Account: "a", AgentID: "x",
		Windows: []delegator.UsageReportWindow{{Window: "seven_day", Used: &old, UsedUnit: delegator.UsageUnitPercent, ResetsAt: now.Add(-time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	if s := d.score(a, now); s != unused {
		t.Errorf("score = %v, want %v; a figure from before the reset must not count", s, unused)
	}
	if err := f.hist.Save(delegator.UsageReport{Source: "t", ReceivedAt: now, Backend: "claude-code", Account: "a", AgentID: "x",
		Windows: []delegator.UsageReportWindow{{Window: "seven_day", Status: "rejected", LimitReached: true, ResetsAt: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	if s := d.score(a, now); s != 0 {
		t.Errorf("score = %v; want 0 for a reached window", s)
	}
}

// TestPool_ChildKeepsItsAccountWhenTheRootIsReset (#3186 trap b): a reset
// root takes a new pick, and a child made before the reset stays on the
// account its files are in.
func TestPool_ChildKeepsItsAccountWhenTheRootIsReset(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	st := memStore{}
	p := NewPool(st, accts...)
	const root, child = "agent/c1", "agent/c1/b1709596800"
	p.For(root)
	if got := p.For(child); got != f.accts["a"] {
		t.Fatalf("child on %s, want its root's a", got.Key())
	}
	f.report("a", map[string]float64{"five_hour": 90})
	delete(st, root+"\x00"+session.MetaKeyAccount) // ClearSessionState
	p.Forget(root)
	if got := p.For(root); got != f.accts["b"] {
		t.Fatalf("reset root on %s, want the new pick b", got.Key())
	}
	if got := p.For(child); got != f.accts["a"] {
		t.Errorf("child moved to %s with its reset root; its files are on a", got.Key())
	}
	if got := NewPool(st, accts...).For(child); got != f.accts["a"] {
		t.Errorf("after a restart the child is on %s, want a", got.Key())
	}
}

// failStore fails every read while fail is set.
type failStore struct {
	memStore
	fail bool
}

var errRead = errors.New("disk I/O error")

func (s *failStore) GetSessionMetadata(key, name string) (string, error) {
	if s.fail {
		return "", errRead
	}
	return s.memStore.GetSessionMetadata(key, name)
}

// TestPool_ReadErrorPicksNothing (#3186 trap a): when a session's stored
// account cannot be read, no account is picked or stored for it; Resolve
// returns the error and For no account, and the next ask reads again.
func TestPool_ReadErrorPicksNothing(t *testing.T) {
	_, accts := newPickFixture(t, "a", "b")
	fs := &failStore{memStore: memStore{}, fail: true}
	p := NewPool(fs, accts...)
	if a, err := p.Resolve("agent/c1"); !errors.Is(err, errRead) || a != nil {
		t.Fatalf("Resolve = %v, %v; want no account and the read error", a, err)
	}
	if a := p.For("agent/c1/b17"); a != nil {
		t.Errorf("child of an unreadable session on %s", a.Key())
	}
	if len(fs.memStore) != 0 {
		t.Errorf("stored %v after a read error", fs.memStore)
	}
	fs.memStore["agent/c1\x00"+session.MetaKeyAccount] = "b"
	fs.fail = false
	if a := p.For("agent/c1"); a == nil || a.Key() != "b" {
		t.Errorf("the next ask did not read again: %v", a)
	}
}

// TestPool_OnLeftOnce (#3186 trap c): a session stored on an account the
// agent no longer has moves to the pick, and OnLeft is told once.
func TestPool_OnLeftOnce(t *testing.T) {
	_, accts := newPickFixture(t, "a", "b")
	st := memStore{"agent/c1\x00" + session.MetaKeyAccount: "gone"}
	var told []string
	mk := func() *Pool {
		p := NewPool(st, accts...)
		p.OnLeft = func(sk, was string, now *Account) { told = append(told, sk+" "+was+"->"+now.Key()) }
		return p
	}
	p := mk()
	p.For("agent/c1")
	p.For("agent/c1")
	mk().For("agent/c1") // a restart: the new binding is stored
	if len(told) != 1 || told[0] != "agent/c1 gone->a" {
		t.Errorf("OnLeft calls = %v, want one", told)
	}
}
