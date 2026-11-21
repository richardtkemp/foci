package account

import (
	"math"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagelog"
)

// win is one window of a stored test report.
type win struct {
	name   string
	pct    float64
	resets time.Time
}

// save stores one report of key's windows, received at.
func (f *pickFixture) save(key string, at time.Time, ws ...win) {
	f.t.Helper()
	r := delegator.UsageReport{Source: "test", ReceivedAt: at, Backend: "claude-code", Account: key, AgentID: "agent"}
	for _, w := range ws {
		v := w.pct
		r.Windows = append(r.Windows, delegator.UsageReportWindow{Window: w.name, Length: delegator.UsageWindowLength(w.name),
			Used: &v, UsedUnit: delegator.UsageUnitPercent, ResetsAt: w.resets})
	}
	if err := f.hist.Save(r); err != nil {
		f.t.Fatal(err)
	}
}

// TestPick_TPerTimeToReset (#3247, the ticket's example): on the 5h window,
// 90% used resetting in 10 min beats 40% used resetting in 4h50m, because
// its allowance left per hour to the reset is larger. The old rule (lowest
// used %) took the 40% one. The 7d windows are equal and reset in a day, so
// the 5h term decides.
func TestPick_TPerTimeToReset(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	now := time.Now()
	day := now.Add(24 * time.Hour)
	f.save("a", now, win{"five_hour", 40, now.Add(4*time.Hour + 50*time.Minute)}, win{"seven_day", 0, day})
	f.save("b", now, win{"five_hour", 90, now.Add(10 * time.Minute)}, win{"seven_day", 0, day})
	if got := NewPool(memStore{}, accts...).For("agent/c1"); got != f.accts["b"] {
		t.Fatalf("new session on %s, want b (10%% left over 10 min beats 60%% left over 4h50m)", got.Key())
	}
}

// busyHistory stores, on account key, a 7d reading at every clock hour of
// the last 14 days whose 7d percent rises by 1 in each hour that busy says is
// busy (by London hour of day), with a 7d reset every 7 days.
func (f *pickFixture) busyHistory(key string, now time.Time, busy func(londonHour int) bool) {
	f.t.Helper()
	end := hourOf(now)
	start := end - 14*24
	period := int64(7 * 24)
	pct := 0.0
	for k := start + 1; k <= end; k++ {
		at := time.Unix(k*3600, 0)
		// periods start at start + j*period + 30 min
		j := (k - 1 - start) / period
		reset := time.Unix((start+(j+1)*period)*3600+1800, 0)
		if k-1 == start+j*period && j > 0 {
			pct = 0 // first reading of a new period: only the hour after the reset counts
		}
		if _, hr := slotOf(k - 1); busy(hr) {
			pct++
		}
		f.save(key, at, win{"seven_day", pct, reset})
	}
}

// TestPick_ProfileBeatsClock (#3247, T-profile): history says demand comes
// only in the four hours of day from now's hour on. Account a has 30% of
// its 7d left and resets in 3 h, inside the busy hours; b has 60% left and
// resets in 24 h, so it sees the busy hours once and then 20 quiet hours.
// Plain T (allowance per clock hour) takes a: 30/3 h beats 60/24 h. T-profile
// takes b: 30 / 3 busy hours = 10 is less than 60 / (4 busy + 20 x the 30%
// floor) = 12. With no history the same figures pick a: T-profile is then T.
func TestPick_ProfileBeatsClock(t *testing.T) {
	now := time.Now()
	_, nowHour := slotOf(hourOf(now))
	busy := func(h int) bool { return (h-nowHour+24)%24 < 4 }
	figures := func(f *pickFixture) {
		f.save("a", now, win{"five_hour", 0, now.Add(10 * time.Minute)}, win{"seven_day", 70, now.Add(3 * time.Hour)})
		f.save("b", now, win{"five_hour", 0, now.Add(10 * time.Minute)}, win{"seven_day", 40, now.Add(24 * time.Hour)})
	}

	f, accts := newPickFixture(t, "h", "a", "b")
	f.busyHistory("h", now, busy)
	f.hold("h", true, now.Add(time.Hour)) // the history's own login is not a candidate
	figures(f)
	if got := NewPool(memStore{}, accts...).For("agent/c1"); got != f.accts["b"] {
		t.Errorf("with a busy-hours history the pick is %s, want b", got.Key())
	}

	f2, accts2 := newPickFixture(t, "h", "a", "b")
	f2.hold("h", true, now.Add(time.Hour))
	figures(f2)
	if got := NewPool(memStore{}, accts2...).For("agent/c1"); got != f2.accts["a"] {
		t.Errorf("with no history the pick is %s, want a (plain T)", got.Key())
	}
}

// TestDemandProfile: a slot's value is the mean of its past hours, floored
// at 30% of the overall mean; a slot with no past hour takes the overall
// mean; hours at or after the end (now's hour and later) are not learned
// from; with no history every hour expects 1 (rule T).
func TestDemandProfile(t *testing.T) {
	// Monday 2026-11-16 00:00 London (= UTC in November), two weekdays of history.
	start := hourOf(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	end := start + 48
	hourly := map[int64]float64{start + 9: 10, start + 24 + 9: 6} // 09:00 both days
	learned := newDemandProfile(hourly, start, end)
	hourly[end] = 1000     // the hour now is in
	hourly[end+24+9] = 500 // a future 09:00
	if p2 := newDemandProfile(hourly, start, end); *p2 != *learned {
		t.Fatal("hours at or after the end changed the profile")
	}
	mean := 16.0 / 48
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if v := learned.value(end + 9); !near(v, 8) {
		t.Errorf("weekday 09:00 = %v, want 8 (mean of 10 and 6)", v)
	}
	if v := learned.value(end + 3); !near(v, profileFloor*mean) {
		t.Errorf("weekday 03:00 = %v, want the floor %v", v, profileFloor*mean)
	}
	sat := hourOf(time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
	if v := learned.value(sat); !near(v, mean) {
		t.Errorf("weekend 09:00 with no history = %v, want the overall mean %v", v, mean)
	}
	from := time.Unix((end+8)*3600+1800, 0) // 08:30 to 09:30
	if e := learned.expected(from, from.Add(time.Hour)); !near(e, 0.5*profileFloor*mean+0.5*8) {
		t.Errorf("expected 08:30-09:30 = %v", e)
	}
	var none *demandProfile
	if e := none.expected(from, from.Add(90*time.Minute)); !near(e, 1.5) {
		t.Errorf("no history: expected over 1.5 h = %v, want 1.5", e)
	}
	if e := newDemandProfile(map[int64]float64{}, start, end).expected(from, from.Add(time.Hour)); !near(e, 1) {
		t.Errorf("history with no demand: expected over 1 h = %v, want 1", e)
	}
}

// TestAddHourlyDemand: demand is the rise of the 7d percent from the highest
// figure so far in the period (a figure that rounds lower and back up adds
// nothing), spread over the time between readings; after a reset the new
// figure is all rise, from the reset time.
func TestAddHourlyDemand(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	reset1, reset2 := t0.Add(5*time.Hour+30*time.Minute), t0.Add(7*24*time.Hour)
	at := func(d time.Duration, pct float64, reset time.Time) usagelog.PercentReading {
		return usagelog.PercentReading{Window: "seven_day", ReceivedAt: t0.Add(d), Percent: pct, ResetsAt: reset}
	}
	rs := []usagelog.PercentReading{
		at(0, 10, reset1),
		{Window: "five_hour", ReceivedAt: t0.Add(30 * time.Minute), Percent: 50, ResetsAt: reset1}, // not a 7d figure
		at(time.Hour, 12, reset1),
		at(90*time.Minute, 11.6, reset1), // another source, rounded lower
		at(2*time.Hour, 12, reset1),
		at(3*time.Hour, 14, reset1),
		at(6*time.Hour, 3, reset2), // new period: reset at 15:30, read 16:00
	}
	hourly := map[int64]float64{}
	addHourlyDemand(hourly, rs)
	h := hourOf(t0)
	want := map[int64]float64{h: 2, h + 2: 2, h + 5: 3}
	for k, v := range hourly {
		if math.Abs(v-want[k]) > 1e-9 {
			t.Errorf("hour +%d = %v, want %v", k-h, v, want[k])
		}
	}
	for k, v := range want {
		if _, ok := hourly[k]; !ok {
			t.Errorf("hour +%d missing, want %v", k-h, v)
		}
	}
}

// TestLearnRatio: cap(5h)/cap(7d) is the 7d rise over the 5h rise, summed
// over 5h periods with a 5h rise of at least 10%; a period with a 7d reset
// inside is skipped; fewer than three periods give no ratio.
func TestLearnRatio(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	week := t0.Add(7 * 24 * time.Hour)
	var rs []usagelog.PercentReading
	id := int64(0)
	add := func(at time.Time, s, l float64, sReset, lReset time.Time) {
		id++
		rs = append(rs,
			usagelog.PercentReading{Window: "five_hour", ReportID: id, ReceivedAt: at, Percent: s, ResetsAt: sReset},
			usagelog.PercentReading{Window: "seven_day", ReportID: id, ReceivedAt: at, Percent: l, ResetsAt: lReset})
	}
	long := 0.0
	week2 := week.Add(7 * 24 * time.Hour)
	period := func(i int, dShort, dLong float64, lFirst, lLast time.Time) {
		ps := t0.Add(time.Duration(i) * 5 * time.Hour)
		sReset := ps.Add(5 * time.Hour)
		add(ps.Add(time.Minute), 0, long, sReset, lFirst)
		add(ps.Add(2*time.Hour), dShort/2, long+dLong/2, sReset, lFirst)
		long += dLong
		add(ps.Add(4*time.Hour), dShort, long, sReset, lLast)
	}
	period(0, 20, 2, week, week)
	period(1, 40, 4, week, week)
	if _, ok := learnRatio(rs); ok {
		t.Fatal("a ratio from two periods")
	}
	period(2, 5, 3, week, week)   // 5h rise under 10%: skipped
	period(3, 30, 9, week, week2) // 7d reset inside: skipped
	if _, ok := learnRatio(rs); ok {
		t.Fatal("a ratio from two qualifying periods")
	}
	period(4, 20, 2, week2, week2)
	// qualifying: 0, 1, 4 -> (2+4+2)/(20+40+20)
	r, ok := learnRatio(rs)
	if !ok || math.Abs(r-0.1) > 1e-9 {
		t.Errorf("ratio = %v, %v; want 0.1", r, ok)
	}
}

// TestBuildDemand_RatioPerLogin: a login with enough history uses its own
// 5h/7d ratio; one without uses the default.
func TestBuildDemand_RatioPerLogin(t *testing.T) {
	f, accts := newPickFixture(t, "a", "b")
	now := time.Now()
	week := now.Add(3 * 24 * time.Hour)
	long := 0.0
	for i := 4; i >= 1; i-- {
		ps := now.Add(-time.Duration(i) * 5 * time.Hour)
		sReset := ps.Add(5 * time.Hour)
		f.save("a", ps.Add(time.Minute), win{"five_hour", 0, sReset}, win{"seven_day", long, week})
		long += 4
		f.save("a", ps.Add(4*time.Hour), win{"five_hour", 20, sReset}, win{"seven_day", long, week})
	}
	d := buildDemand(accts, now)
	if r := d.shortRatio("a"); math.Abs(r-0.2) > 1e-9 {
		t.Errorf("a's ratio = %v, want 0.2", r)
	}
	if r := d.shortRatio("b"); r != defaultShortRatio {
		t.Errorf("b's ratio = %v, want the default", r)
	}
}
