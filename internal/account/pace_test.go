package account

import (
	"math"
	"testing"
	"time"
)

// paceNow is a Tuesday 12:00 in London. The fixtures' 7d percent does not
// move, so the demand profile is flat (no demand) and a login's sustainable
// rate is its 7d allowance left per clock hour; the ratio is the default, 0.1.
var paceNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// use stores key's figures at the given minutes before paceNow: the 5h
// percent of each, and a 7d percent of long that resets in longLeft.
func (f *pickFixture) use(key string, long float64, longLeft time.Duration, short map[int]float64) {
	f.t.Helper()
	for m := 24 * 60; m >= 0; m-- {
		if pct, ok := short[m]; ok {
			f.save(key, paceNow.Add(-time.Duration(m)*time.Minute),
				win{"five_hour", pct, paceNow.Add(2 * time.Hour)}, win{"seven_day", long, paceNow.Add(longLeft)})
		}
	}
}

func paceAt(t *testing.T, accts []*Account, at time.Time) (float64, bool) {
	t.Helper()
	return poolPace(NewPool(memStore{}, accts...).loginPaces(at), at)
}

func wantPace(t *testing.T, accts []*Account, at time.Time, want float64) {
	t.Helper()
	got, ok := paceAt(t, accts, at)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Errorf("pace = %v (known %v), want %v", got, ok, want)
	}
}

// TestPace_AllowanceLeft (#3267, Dick: "suppose it's 5days until reset and
// I've used 80% already. it should now be much easier to hit the red zone"):
// the same hour of use is a pace of 1 exactly at the rate that uses up what
// is left at the reset, and four times as fast at 80% used as at 20%.
func TestPace_AllowanceLeft(t *testing.T) {
	for _, c := range []struct{ used, want float64 }{{20, 0.25}, {50, 0.4}, {80, 1}, {95, 4}} {
		f, accts := newPickFixture(t, "a")
		// 10% of 5h in the hour = 1% of 7d an hour, 20 h to the reset.
		f.use("a", c.used, 20*time.Hour, map[int]float64{70: 4, 60: 5, 30: 11, 0: 15})
		wantPace(t, accts, paceNow, c.want)
		if got := NewPool(memStore{}, accts...).OverPace(paceNow); got != (c.want >= 1) {
			t.Errorf("%v%% used: over pace = %v, want %v", c.used, got, c.want >= 1)
		}
	}
}

// TestPace_Window: use before the last 60 minutes does not count, and a rise
// across the start of the window counts by its share inside.
func TestPace_Window(t *testing.T) {
	f, accts := newPickFixture(t, "a")
	// 40 before the window; 8 over 80 to 40 min ago, half inside; 3 after.
	f.use("a", 80, 20*time.Hour, map[int]float64{200: 1, 80: 41, 40: 49, 10: 52})
	wantPace(t, accts, paceNow, 0.7)
	// 45 min on with no new figure: only the last 5 of the 3's 30 min are inside.
	wantPace(t, accts, paceNow.Add(45*time.Minute), 0.1*3*(5.0/30)*(20-0.75)/20)
	wantPace(t, accts, paceNow.Add(time.Hour), 0)
}

// TestPace_ShortReset: use after a 5h reset counts in full. Claude Code
// reports 0% with no reset time while no 5h window is open; the next period's
// use counts from its first figure, not from when it passes the last one's.
func TestPace_ShortReset(t *testing.T) {
	f, accts := newPickFixture(t, "a")
	long := win{"seven_day", 80, paceNow.Add(20 * time.Hour)}
	at := func(m int) time.Time { return paceNow.Add(-time.Duration(m) * time.Minute) }
	f.save("a", at(300), win{"five_hour", 60, at(120)}, long)
	f.save("a", at(60), win{"five_hour", 0, time.Time{}}, long)
	f.save("a", at(30), win{"five_hour", 6, at(-270)}, long)
	f.save("a", at(0), win{"five_hour", 10, at(-270)}, long)
	wantPace(t, accts, paceNow, 1)
}

// TestPace_LongReset: a 7d window that reset since its figure is unused, and
// resets 7 days later.
func TestPace_LongReset(t *testing.T) {
	f, accts := newPickFixture(t, "a")
	f.use("a", 90, -time.Minute, map[int]float64{60: 0, 10: 10})
	wantPace(t, accts, paceNow, (7*24-1.0/60)/100)
}

// TestPace_NoFigure: no 7d figure, no 5h figure (nor spend, as a Claude
// login), no history and no pool are
// no pace: not over.
func TestPace_NoFigure(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	f.save("a", paceNow.Add(-time.Hour), win{"five_hour", 0, paceNow.Add(time.Hour)})
	f.save("a", paceNow, win{"five_hour", 90, paceNow.Add(time.Hour)})
	if v, ok := paceAt(t, accts, paceNow); ok {
		t.Errorf("pace with no 7d figure = %v, want none", v)
	}
	if NewPool(memStore{}, accts...).OverPace(paceNow) {
		t.Error("over pace with no 7d figure")
	}
	f2, accts2 := newPickFixture(t, "a")
	f2.save("a", paceNow.Add(-time.Hour), win{"seven_day", 10, paceNow.Add(time.Hour)})
	f2.save("a", paceNow, win{"seven_day", 90, paceNow.Add(time.Hour)})
	if v, ok := paceAt(t, accts2, paceNow); ok {
		t.Errorf("pace with no 5h figure = %v, want none", v)
	}
	if (*Pool)(nil).OverPace(paceNow) {
		t.Error("a nil pool is over pace")
	}
}

// TestPace_PoolOfLogins (#3291): an agent's pace is its pool's total use
// over the total it can sustain; a login with no figure is left out. Here
// both can sustain 1% an hour, so it is the mean of their paces.
func TestPace_PoolOfLogins(t *testing.T) {
	// 80% used, 20 h left: pace = 5h rise / 10.
	for _, c := range []struct{ a, b, want float64 }{{16, 5, 1.05}, {16, 3, 0.95}, {16, 0, 0.8}} {
		f, accts := newPickFixture(t, "a", "b", "nofigure")
		f.use("a", 80, 20*time.Hour, map[int]float64{60: 0, 0: c.a})
		f.use("b", 80, 20*time.Hour, map[int]float64{60: 0, 0: c.b})
		wantPace(t, accts, paceNow, c.want)
		if got := NewPool(memStore{}, accts...).OverPace(paceNow); got != (c.want >= 1) {
			t.Errorf("logins %v and %v: over pace = %v, want %v", c.a, c.b, got, c.want >= 1)
		}
	}
}

// TestPace_SpentLogin (#3273, Dick: "Either count as full or null"; full): at
// 99% or more of its 7d allowance used a login can sustain nothing until its
// reset, so alone it is a pace of 1, idle or not; in a pool it adds its use
// and nothing to what the pool can sustain (#3291).
func TestPace_SpentLogin(t *testing.T) {
	idle, busy := map[int]float64{60: 5, 0: 5}, map[int]float64{60: 0, 0: 40}
	for _, c := range []struct {
		name string
		used float64
		left time.Duration
		use  map[int]float64
		want float64
	}{
		{"98 idle", 98, 20 * time.Hour, idle, 0},
		{"99 idle", 99, 20 * time.Hour, idle, 1},
		{"100 idle", 100, 20 * time.Hour, idle, 1},
		{"100 busy", 100, 20 * time.Hour, busy, 1},
		{"100, reset", 100, -time.Minute, idle, 0},
	} {
		f, accts := newPickFixture(t, "a")
		f.use("a", c.used, c.left, c.use)
		wantPace(t, accts, paceNow, c.want)
		if got := NewPool(memStore{}, accts...).OverPace(paceNow); got != (c.want >= 1) {
			t.Errorf("%s: over pace = %v, want %v", c.name, got, c.want >= 1)
		}
	}
	// Dick's Q12: one login at 100%, the other at 1.9 (80% used, 20 h left):
	// the pool is at the other's pace.
	f, accts := newPickFixture(t, "spent", "b")
	f.use("spent", 100, 20*time.Hour, idle)
	f.use("b", 80, 20*time.Hour, map[int]float64{60: 0, 0: 19})
	wantPace(t, accts, paceNow, 1.9)
	if !NewPool(memStore{}, accts...).OverPace(paceNow) {
		t.Error("one login spent, the other at 1.9: not over pace")
	}
}

// TestOverPace_Hysteresis: on from 1.0, off below 0.85; in between the state
// is the one before, worked out from the readings alone (a new Pool, as after
// a restart, gives the same answer).
func TestOverPace_Hysteresis(t *testing.T) {
	// 80% used, 20 h left: pace = 5h rise in the last hour / 10, within the
	// few percent the time to the reset differs.
	for _, c := range []struct {
		name  string
		short map[int]float64
		want  bool
	}{
		{"reached 1.0, now 0.9", map[int]float64{120: 0, 60: 12, 0: 21}, true},
		{"reached 1.0, now 0.8", map[int]float64{120: 0, 60: 12, 0: 20}, false},
		{"never 1.0, now 0.9", map[int]float64{120: 0, 60: 8, 0: 17}, false},
		{"reached 1.0, then 0.5, now 0.9", map[int]float64{180: 0, 120: 12, 60: 17, 0: 26}, false},
		{"reached 1.0, then 0.9 and 0.9", map[int]float64{180: 0, 120: 12, 60: 21, 0: 30}, true},
		{"now 1.0 from nothing", map[int]float64{60: 0, 0: 10}, true},
	} {
		f, accts := newPickFixture(t, "a")
		f.use("a", 80, 20*time.Hour, c.short)
		if got := NewPool(memStore{}, accts...).OverPace(paceNow); got != c.want {
			v, _ := paceAt(t, accts, paceNow)
			t.Errorf("%s: over pace = %v, want %v (pace now %v)", c.name, got, c.want, v)
		}
	}
}

// TestPace_UsagePattern (#3267, Dick: "I won't use foci 24h a day so this
// also needs to be adjusted for my usage patterns"): the time to the reset
// counts in busy hours. Use only in hours 10 to 17 each day, 8 of 24: 24 h to
// the reset is fewer busy hours than clock hours, so the same use is a lower
// pace than with no history.
func TestPace_UsagePattern(t *testing.T) {
	figures := func(f *pickFixture) {
		f.use("a", 80, 24*time.Hour, map[int]float64{60: 0, 0: 10})
	}
	f, accts := newPickFixture(t, "h", "a")
	f.busyHistory("h", paceNow, func(h int) bool { return h >= 10 && h < 18 })
	figures(f)
	flat, flatAccts := newPickFixture(t, "a")
	figures(flat)
	wantPace(t, flatAccts, paceNow, 1.2)

	d := NewPool(memStore{}, accts...).demandAt(paceNow)
	l := newLoginPace(d, f.accts["a"], paceNow)
	fl, ok := l.at(paceNow)
	got := fl.use / fl.supply
	// Slots: 1 in 8 hours, the floor 0.3 x 1/3 = 0.1 in 16.
	busy := (8*1.0 + 16*0.01) / (8*1.0 + 16*0.1)
	want := 1.0 / 20 * (8*1.0 + 16*0.1) / busy
	if !ok || math.Abs(got-want) > 0.02 {
		t.Errorf("pace = %v, want about %v", got, want)
	}
}

// TestBusyHour: the demand-weighted mean hour of a profile week, weekdays 5
// to weekend days' 2.
func TestBusyHour(t *testing.T) {
	// Monday 2026-11-16 to Sunday: 10 at 09:00 on weekdays, 5 at 09:00 at the
	// weekend.
	start := hourOf(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	hourly := map[int64]float64{}
	for day := range int64(7) {
		hourly[start+day*24+9] = 10
		if day >= 5 {
			hourly[start+day*24+9] = 5
		}
	}
	p := newDemandProfile(hourly, start, start+7*24)
	fl := 0.3 * 60 / (7 * 24)
	want := (5*(100+23*fl*fl) + 2*(25+23*fl*fl)) / (5*(10+23*fl) + 2*(5+23*fl))
	if got := p.busyHour(); math.Abs(got-want) > 1e-9 {
		t.Errorf("busyHour = %v, want %v", got, want)
	}
	if got := (*demandProfile)(nil).busyHour(); got != 1 {
		t.Errorf("busyHour with no history = %v, want 1", got)
	}
}

// TestPace_DicksExample (#3291, Dick 2026-11-27: "one at 99% used and expires
// in one hour, one at 0% used and expires in 24 hours. There's no way I will
// use all 100% of the second account in 24 hours, so actually my situation is
// very healthy!"): 3% of 7d an hour, all on the second login, is a pace of
// 3 / (100 / 24) = 0.72, and not over; the mean of the two logins' paces was
// (1 + 0.72) / 2 = 0.86, over once it reached 1. At 98% the first login adds
// its 2% in the hour to what the pool can sustain.
func TestPace_DicksExample(t *testing.T) {
	for _, c := range []struct {
		used float64
		want float64
	}{{99, 3 / (100.0 / 24)}, {98, 3 / (2 + 100.0/24)}} {
		f, accts := newPickFixture(t, "a", "b")
		f.use("a", c.used, time.Hour, map[int]float64{60: 5, 0: 5})
		f.use("b", 0, 24*time.Hour, map[int]float64{60: 0, 0: 30}) // 30% of 5h = 3% of 7d
		wantPace(t, accts, paceNow, c.want)
		if NewPool(memStore{}, accts...).OverPace(paceNow) {
			t.Errorf("first login at %v%%: over pace", c.used)
		}
	}
	// At 5% of 7d an hour the second login alone would be over, but not the
	// pool with the first at 98%: 5 / 6.17.
	f, accts := newPickFixture(t, "a", "b")
	f.use("a", 98, time.Hour, map[int]float64{60: 5, 0: 5})
	f.use("b", 0, 24*time.Hour, map[int]float64{60: 0, 0: 50})
	wantPace(t, accts, paceNow, 5/(2+100.0/24))
}

// TestPace_IdleLogin (#3291): a login idle for longer than the lookback adds
// what it can sustain, from its last figures: 50% left 50 h from its reset is
// 1% an hour, so the busy login's pace of 1 is a pool pace of 0.5. Before,
// it was left out and the agent was over pace.
func TestPace_IdleLogin(t *testing.T) {
	f, accts := newPickFixture(t, "busy", "idle")
	f.use("busy", 80, 20*time.Hour, map[int]float64{60: 0, 0: 10})
	f.save("idle", ago(72*time.Hour), win{"five_hour", 3, ago(70 * time.Hour)}, win{"seven_day", 50, paceNow.Add(50 * time.Hour)})
	wantPace(t, accts, paceNow, 0.5)
	if NewPool(memStore{}, accts...).OverPace(paceNow) {
		t.Error("over pace with an idle login beside a busy one at 1")
	}
}

// capFixture is newPickFixture with each login's 7d percent per dollar
// learned: from 60 h before paceNow, in a period that reset 20 h before it,
// its 7d figure rose 1% an hour for 11 hours, with usdPerPercent[key] in
// each hour (the first rise does not count). A login missing from it has no
// dollars.
func capFixture(t *testing.T, usdPerPercent map[string]float64, keys ...string) (*pickFixture, []*Account) {
	t.Helper()
	f, accts := newPickFixture(t, keys...)
	old := ago(20 * time.Hour)
	for _, a := range accts {
		usd, ok := usdPerPercent[a.Key()]
		if !ok {
			continue
		}
		l := &ledger{}
		a.spend = l.read
		f.save(a.Key(), ago(60*time.Hour), win{"seven_day", 0, old})
		for i := range 11 {
			end := time.Duration(59-i) * time.Hour
			l.at(end+30*time.Minute, usd)
			f.save(a.Key(), ago(end), win{"seven_day", float64(i + 1), old})
		}
	}
	return f, accts
}

// TestPace_Caps (#3291, Dick: "We can imply the caps! We have the data"):
// each login weighs by its cap, its dollars per 7d percent, learned from its
// own calls. A login of 4 times the cap with as much left can sustain 4
// times the use, so 2% of its own 7d an hour on the small login is a pace of
// 2 / (4 x 1 + 1) = 0.4, where equal caps would make it 2 / 2 = 1. A login
// with no rate learned weighs as the mean of the learned caps; with none
// learned the caps are equal.
func TestPace_Caps(t *testing.T) {
	f, accts := capFixture(t, map[string]float64{"big": 4, "small": 1}, "big", "small", "new")
	for _, k := range []string{"big", "small", "new"} {
		f.use(k, 80, 20*time.Hour, map[int]float64{60: 0, 0: map[string]float64{"small": 20}[k]})
	}
	caps := map[string]float64{}
	for _, l := range NewPool(memStore{}, accts...).loginPaces(paceNow) {
		caps[l.key] = l.cap
	}
	for k, want := range map[string]float64{"big": 4, "small": 1, "new": 2.5} {
		if math.Abs(caps[k]-want) > 1e-9 {
			t.Errorf("cap of %s = %v, want %v", k, caps[k], want)
		}
	}
	// The 7d rises make a demand profile, so a login sustains its 20% left
	// over the busy hours to the reset, the same for all three.
	d := NewPool(memStore{}, accts...).demandAt(paceNow)
	busy := d.profile.expected(paceNow, paceNow.Add(20*time.Hour)) / d.profile.busyHour()
	wantPace(t, accts[:2], paceNow, 2/(4*20/busy+20/busy))
	wantPace(t, accts, paceNow, 2/(4*20/busy+20/busy+2.5*20/busy))

	f2, accts2 := newPickFixture(t, "a", "b")
	f2.use("a", 80, 20*time.Hour, map[int]float64{60: 0, 0: 20})
	f2.use("b", 80, 20*time.Hour, map[int]float64{60: 0, 0: 0})
	wantPace(t, accts2, paceNow, 1)
}
