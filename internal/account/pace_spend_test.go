package account

import (
	"errors"
	"math"
	"testing"
	"time"

	"foci/internal/usagelog"
)

// ledger is a login's spend in a test.
type ledger struct {
	calls []Spend
	err   error
}

func (l *ledger) read(from, to time.Time) ([]Spend, error) {
	var out []Spend
	for _, c := range l.calls {
		if c.At.After(from) && !c.At.After(to) {
			out = append(out, c)
		}
	}
	return out, l.err
}

// at adds a call of usd the given time before paceNow (calls in time order).
func (l *ledger) at(before time.Duration, usd float64) {
	l.calls = append(l.calls, Spend{At: paceNow.Add(-before), USD: usd, Priced: true})
}

func ago(d time.Duration) time.Time { return paceNow.Add(-d) }

// codexReset is when the current 7d period of codexFixture's login resets.
var codexReset = paceNow.Add(40 * time.Hour)

// codexFixture is login "codex" with a 7d window only. From 60 h before
// paceNow, in a period that reset 20 h before it, it spent $100 in the hour
// its 7d figure rose to 1%, then $4 in each of n hours that rose it by 1%.
func codexFixture(t *testing.T, n int) (*pickFixture, *Account, *ledger) {
	t.Helper()
	f, accts := newPickFixture(t, "codex")
	l := &ledger{}
	accts[0].spend = l.read
	old := ago(20 * time.Hour)
	f.save("codex", ago(60*time.Hour), win{"codex/7d", 0, old})
	for i := range n + 1 {
		usd := 4.0
		if i == 0 {
			usd = 100
		}
		end := time.Duration(59-i) * time.Hour
		l.at(end+30*time.Minute, usd)
		f.save("codex", ago(end), win{"codex/7d", float64(i + 1), old})
	}
	return f, accts[0], l
}

// busyHoursLeft is the busy hours of a's demand profile from t to codexReset.
func busyHoursLeft(a *Account, t time.Time) float64 {
	d := NewPool(memStore{}, a).demandAt(paceNow)
	return d.profile.expected(t, codexReset) / d.profile.busyHour()
}

// TestSpendPace (#3269, Dick: "Why not base it on recorded API *cost*"): with
// no 5h figure the use is the dollars of the last 60 minutes x the login's own
// 7d percent per dollar, here 1/4, and a codex agent can go red. The rate is
// learned from step to step: the $100 of the first rise do not count.
func TestSpendPace(t *testing.T) {
	for _, usd := range []float64{0, 2, 80} {
		f, a, l := codexFixture(t, 10)
		l.at(61*time.Minute, 40) // before the window
		l.at(60*time.Minute, 40)
		l.at(59*time.Minute, usd/2)
		l.at(time.Minute, usd/2)
		f.save("codex", ago(90*time.Minute), win{"codex/7d", 30, codexReset})
		f.save("codex", ago(time.Minute), win{"codex/7d", 30, codexReset})
		got, ok := paceAt(t, []*Account{a}, paceNow)
		if want := usd / 4 / 70 * busyHoursLeft(a, paceNow); !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("$%v: pace = %v (known %v), want %v", usd, got, ok, want)
		}
		if over := NewPool(memStore{}, a).OverPace(paceNow); over != (usd == 80) {
			t.Errorf("$%v: over pace = %v (pace %v)", usd, over, got)
		}
	}
}

// TestSpendPace_ShortFigure: a login with a 5h figure has the 5h method of
// #3267, whatever its dollars and its 7d level.
func TestSpendPace_ShortFigure(t *testing.T) {
	for used, want := range map[float64]float64{80: 1, 98: 10} {
		f, accts := newPickFixture(t, "a")
		l := &ledger{}
		l.at(10*time.Minute, 1000)
		accts[0].spend = l.read
		f.use("a", used, 20*time.Hour, map[int]float64{70: 4, 60: 5, 30: 11, 0: 15})
		wantPace(t, accts, paceNow, want)
	}
}

// TestSpendPace_NotLearned: with less than 5% of rises there is no pace.
func TestSpendPace_NotLearned(t *testing.T) {
	for n, want := range map[int]bool{4: false, 5: true} {
		f, a, l := codexFixture(t, n)
		l.at(10*time.Minute, 200)
		f.save("codex", ago(time.Minute), win{"codex/7d", 30, codexReset})
		if _, ok := paceAt(t, []*Account{a}, paceNow); ok != want {
			t.Errorf("%d%% of rises: pace known = %v, want %v", n, ok, want)
		}
		if got := NewPool(memStore{}, a).OverPace(paceNow); got != want {
			t.Errorf("%d%% of rises: over pace = %v, want %v", n, got, want)
		}
	}
}

// TestSpendPace_Unpriced: a call of unknown cost in the hour is no pace, not
// zero use; one before it changes nothing; an unreadable ledger is no pace.
func TestSpendPace_Unpriced(t *testing.T) {
	for _, c := range []struct {
		name   string
		before time.Duration
		err    error
		want   bool
	}{
		{"in the hour", 30 * time.Minute, nil, false},
		{"before it", 90 * time.Minute, nil, true},
		{"read error", 90 * time.Minute, errors.New("locked"), false},
	} {
		f, a, l := codexFixture(t, 10)
		p := NewPool(memStore{}, a)
		p.demandAt(paceNow) // the rate is learned
		l.calls = append(l.calls, Spend{At: ago(c.before)})
		l.at(20*time.Minute, 200)
		l.err = c.err
		f.save("codex", ago(time.Minute), win{"codex/7d", 30, codexReset})
		if got := p.OverPace(paceNow); got != c.want {
			t.Errorf("%s: over pace = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSpendPace_Spent (ruled): at 99% or more used a login with no 5h figure is
// over pace until its reset: idle, with no rate learned, when its last figure
// is days old, when the figure goes back by one, and when codex repeats an old
// figure. In the mean of a pool it counts as 1.
func TestSpendPace_Spent(t *testing.T) {
	type fig struct {
		before time.Duration
		pct    float64
	}
	for _, c := range []struct {
		name   string
		figs   []fig
		resets time.Duration // after paceNow
		want   bool
	}{
		{"98", []fig{{2 * time.Hour, 98}}, time.Hour, false},
		{"99", []fig{{2 * time.Hour, 99}}, time.Hour, true},
		{"100", []fig{{2 * time.Hour, 100}}, time.Hour, true},
		{"days old", []fig{{60 * time.Hour, 99}}, time.Hour, true},
		{"days old, reset", []fig{{60 * time.Hour, 99}}, -time.Minute, false},
		{"reset", []fig{{2 * time.Hour, 100}}, -time.Minute, false},
		{"back by one", []fig{{2 * time.Hour, 99}, {time.Hour, 98}, {time.Minute, 98}}, time.Hour, true},
		{"old figure", []fig{{2 * time.Hour, 99}, {time.Minute, 74}}, time.Hour, true},
		{"reset early", []fig{{2 * time.Hour, 99}, {time.Hour, 0}, {time.Minute, 0}}, time.Hour, false},
	} {
		f, accts := newPickFixture(t, "codex")
		accts[0].spend = (&ledger{}).read
		for _, g := range c.figs {
			f.save("codex", ago(g.before), win{"codex/7d", g.pct, paceNow.Add(c.resets)})
		}
		if got := NewPool(memStore{}, accts...).OverPace(paceNow); got != c.want {
			t.Errorf("%s: over pace = %v, want %v", c.name, got, c.want)
		}
		if v, ok := paceAt(t, accts, paceNow); ok != c.want || (ok && v != 1) {
			t.Errorf("%s: pace = %v (known %v)", c.name, v, ok)
		}
	}
	f, accts := newPickFixture(t, "codex", "b")
	accts[0].spend = (&ledger{}).read
	f.save("codex", ago(time.Hour), win{"codex/7d", 100, paceNow.Add(time.Hour)})
	f.use("b", 95, 20*time.Hour, map[int]float64{60: 5, 0: 15}) // pace 4
	wantPace(t, accts, paceNow, 2.5)
}

// TestSpendPace_Hysteresis: between the thresholds the state is the one at
// the latest reading outside them, whose dollars are read further back.
func TestSpendPace_Hysteresis(t *testing.T) {
	for _, c := range []struct {
		name  string
		early float64 // the pace 5 h before
		want  bool
	}{{"was over", 1.2, true}, {"was under", 0.5, false}} {
		f, a, l := codexFixture(t, 10)
		for h := 5; h >= 0; h-- {
			f.save("codex", ago(time.Duration(h)*time.Hour), win{"codex/7d", 50, codexReset})
		}
		for h := 5; h >= 0; h-- {
			pace := 0.92
			if h == 5 {
				pace = c.early
			}
			before := time.Duration(h) * time.Hour
			l.at(before+10*time.Minute, pace*50*4/busyHoursLeft(a, ago(before)))
		}
		if got := NewPool(memStore{}, a).OverPace(paceNow); got != c.want {
			v, ok := paceAt(t, []*Account{a}, paceNow)
			t.Errorf("%s: over pace = %v, want %v (pace now %v %v)", c.name, got, c.want, v, ok)
		}
	}
}

// TestLearnSpendRate: the newest 20% of rises count; a rise with a call of
// unknown cost, one across a reset, one after an old figure and what came from
// until on do not.
func TestLearnSpendRate(t *testing.T) {
	var (
		rs    []usagelog.PercentReading
		l     = &ledger{}
		m     int // minutes since base
		base  = ago(100 * time.Hour)
		at    = func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
		reset = at(5000)
	)
	read := func(pct float64, reset time.Time) {
		m += 10
		rs = append(rs, usagelog.PercentReading{Window: "codex/7d", ReceivedAt: at(m), Percent: pct, ResetsAt: reset})
	}
	// step spends usd, then reads pct.
	step := func(pct, usd float64) {
		l.calls = append(l.calls, Spend{At: at(m + 5), USD: usd, Priced: true})
		read(pct, reset)
	}
	check := func(name string, until time.Time, want float64) {
		t.Helper()
		got, ok, err := learnSpendRate(rs, l.read, until)
		if err != nil || ok != (want != 0) || (ok && math.Abs(1/got-want) > 1e-9) {
			t.Errorf("%s: $ per 1%% = %v (%v, %v), want %v", name, 1/got, ok, err, want)
		}
	}
	// 10 steps at $9 after the first rise, then 20 at $2.
	read(10, reset)
	for i := range 11 {
		step(float64(11+i), 9)
	}
	cheap := m + 1
	for i := range 20 {
		step(float64(22+i), 2)
	}
	check("the newest 20%", at(m+1), 2)
	check("until the cheap ones", at(cheap), 9)
	check("until the 4th step", at(cheap-60), 0)

	read(20, reset)
	read(41, reset)
	check("an old figure", at(m+1), 2)

	l.calls = append(l.calls, Spend{At: at(m + 1)})
	step(42, 70)
	check("a step with an unpriced call", at(m+1), 2)

	reset = at(20000)
	step(0, 500)
	step(1, 500)
	step(2, 2)
	check("a reset", at(m+1), 2)

	step(0, 0)
	step(0, 500)
	step(1, 500)
	step(2, 2)
	check("an early reset", at(m+1), 2)

	read(80, at(5000))
	step(3, 2)
	check("an old period's figure", at(m+1), 2)

	if _, ok, _ := learnSpendRate(rs, (&ledger{}).read, at(m+1)); ok {
		t.Error("a rate from no dollars")
	}
	l.err = errors.New("locked")
	if _, ok, err := learnSpendRate(rs, l.read, at(m+1)); ok || err == nil {
		t.Errorf("a rate from an unreadable ledger (%v, %v)", ok, err)
	}
}
