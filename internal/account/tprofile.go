package account

import (
	"math"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagelog"
)

// tprofile.go: the score of the account pick, rule T-profile (#3247, Dick
// 2026-11-21 16:17, after the #3245 simulation).
//
// An open account's score is
//
//	min( 5h allowance left / expected demand until the 5h reset ,
//	     7d allowance left / expected demand until the 7d reset )
//
// and the pick is the open account with the largest score. Expected demand is
// the sum, over the hours until the reset, of a demand profile learned from
// the pool's own usage readings (demandProfile). With no history every hour
// expects the same demand, and the score is rule T (allowance left per hour
// until the reset).
//
// Units. Both "left" values and the demand are in percent of ONE login's 7d
// window. The 5h allowance left is converted with the login's ratio
// cap(5h) / cap(7d), learned from its own readings (learnRatio). ASSUMPTION
// (stated, not checked by code): every login in one pool has the same 7d cap
// (the same plan). The comparison across accounts and the pool's summed
// demand both rest on it. The #3245 simulation measured the two work Mac
// logins at 13.0 and 12.6 $ per 7d %, equal within noise.

const (
	// profileFloor: each hour slot of the profile is at least this share of
	// the overall mean hourly demand (ruled). Without it a reset in a quiet
	// period would give an unbounded score.
	profileFloor = 0.3
	// defaultShortRatio is cap(5h) / cap(7d) for a login whose readings do
	// not yet give one (learnRatio). Measured: NUC 0.074, work Mac 0.105
	// (#3245 calibration).
	defaultShortRatio = 0.1
	// ratioMinPeriods closed-or-open 5h periods, each with a 5h rise of at
	// least ratioMinRise percent, are needed before a login's own ratio is
	// used.
	ratioMinPeriods = 3
	ratioMinRise    = 10.0
	// The time to a reset is floored (as in the #3245 simulator), so that a
	// window about to reset does not score without bound.
	shortResetFloor = 10 * time.Minute
	longResetFloor  = time.Hour
	// periodSlack: two reset times this close name the same window period
	// (codex's reset time wobbles by a second; usagelog uses the same slack).
	periodSlack = 5 * time.Minute

	shortLabel = "5h"
	longLabel  = "7d"
)

// isShort and isLong name the two windows rule T reads: Claude Code's
// five_hour and seven_day, a codex main bucket's 5h and 7d. Other windows (a
// model's own 7d window, a codex side bucket) are not part of the score.
func isShort(window string) bool { return delegator.UsageWindowLabel(window) == shortLabel }
func isLong(window string) bool  { return delegator.UsageWindowLabel(window) == longLabel }

var london = func() *time.Location {
	if l, err := time.LoadLocation("Europe/London"); err == nil {
		return l
	}
	return time.UTC
}()

// hourOf is the index of the clock hour t is in (Unix hours).
func hourOf(t time.Time) int64 { return int64(math.Floor(float64(t.Unix()) / 3600)) }

// slotOf is the profile slot of hour h: (weekend, hour of day), London time
// (ruled).
func slotOf(h int64) (weekend, hour int) {
	d := time.Unix(h*3600, 0).In(london)
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		weekend = 1
	}
	return weekend, d.Hour()
}

// demandProfile is the expected demand per hour slot, learned from the
// completed hours [start, end) of the pool's history only (the hour now is
// in is not part of it).
type demandProfile struct {
	n     int64 // hours of history
	total float64
	sum   [2][24]float64
	cnt   [2][24]int
}

// newDemandProfile builds the profile from hourly demand (by hour index; an
// hour of the span with no entry had no demand) over [start, end).
func newDemandProfile(hourly map[int64]float64, start, end int64) *demandProfile {
	p := &demandProfile{}
	for h := start; h < end; h++ {
		u := hourly[h]
		we, hr := slotOf(h)
		p.sum[we][hr] += u
		p.cnt[we][hr]++
		p.total += u
		p.n++
	}
	return p
}

// mean is the overall mean hourly demand; 1 when there is no history or no
// demand in it (every hour then expects the same, and the score is rule T).
func (p *demandProfile) mean() float64 {
	if p == nil || p.n == 0 || p.total <= 0 {
		return 1
	}
	return p.total / float64(p.n)
}

// value is the expected demand in hour h: the mean of its slot's past hours
// (the overall mean when the slot has none yet), at least profileFloor x the
// overall mean.
func (p *demandProfile) value(h int64) float64 {
	m := p.mean()
	if p == nil || p.total <= 0 {
		return m
	}
	we, hr := slotOf(h)
	return p.slotValue(we, hr)
}

// slotValue is value for a slot of a profile with demand in it.
func (p *demandProfile) slotValue(weekend, hour int) float64 {
	m := p.mean()
	v := m
	if c := p.cnt[weekend][hour]; c > 0 {
		v = p.sum[weekend][hour] / float64(c)
	}
	return max(v, profileFloor*m)
}

// busyHour is the demand-weighted mean hour of one profile week (5 weekdays,
// 2 weekend days): sum v^2 / sum v over its hours, the demand of the hour an
// average unit of use falls in (#3267). 1 with no history.
func (p *demandProfile) busyHour() float64 {
	if p == nil || p.total <= 0 {
		return p.mean()
	}
	var sumSq, sum float64
	for we, days := range [2]float64{5, 2} {
		for hr := range 24 {
			v := p.slotValue(we, hr)
			sumSq += days * v * v
			sum += days * v
		}
	}
	return sumSq / sum
}

// expected is the expected demand from t to end (part hours pro rata).
func (p *demandProfile) expected(t, end time.Time) float64 {
	var e float64
	for cur := t; cur.Before(end); {
		h := hourOf(cur)
		next := time.Unix((h+1)*3600, 0)
		if next.After(end) {
			next = end
		}
		e += p.value(h) * next.Sub(cur).Hours()
		cur = next
	}
	return e
}

// addHourlyDemand adds to hourly the demand one login's 7d readings show:
// each rise of the 7d percent (rises), spread evenly over its time.
func addHourlyDemand(hourly map[int64]float64, readings []usagelog.PercentReading) {
	for _, r := range rises(readings, isLong) {
		spread(hourly, r.from, r.to, r.amount)
	}
}

// rise is an increase of one window's percent between two readings.
type rise struct {
	from, to time.Time // to is when the reading that shows it was received
	amount   float64
}

// rises lists the increases of the percent between consecutive readings of
// the windows match names. A rise is measured from the highest figure seen so
// far in the window's period, so figures from two sources that differ in
// rounding do not count as demand; after a reset the first figure is all
// rise, from the reset time. A figure with no reset time (Claude Code's 0%
// while no 5h window is open) starts no period: the next one is judged by the
// last reset time known.
func rises(readings []usagelog.PercentReading, match func(window string) bool) []rise {
	var (
		out     []rise
		prev    *usagelog.PercentReading
		reset   time.Time // the last reset time reported
		highest float64
	)
	for i := range readings {
		r := &readings[i]
		if !match(r.Window) {
			continue
		}
		if prev == nil {
			prev, reset, highest = r, r.ResetsAt, r.Percent
			continue
		}
		from, amount := prev.ReceivedAt, r.Percent-highest
		if newPeriod(reset, r.ResetsAt) {
			amount, highest = r.Percent, 0
			if reset.After(from) && reset.Before(r.ReceivedAt) {
				from = reset
			}
		}
		highest = max(highest, r.Percent)
		if amount > 0 {
			out = append(out, rise{from: from, to: r.ReceivedAt, amount: amount})
		}
		prev = r
		if !r.ResetsAt.IsZero() {
			reset = r.ResetsAt
		}
	}
	return out
}

// newPeriod reports whether a figure whose window resets at b belongs to a
// later period than one that resets at a.
func newPeriod(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && b.Sub(a) > periodSlack
}

// spread adds amount to hourly, spread evenly over [from, to] by the share
// of that time in each hour; all of it to the hour of to when from >= to.
func spread(hourly map[int64]float64, from, to time.Time, amount float64) {
	total := to.Sub(from)
	if total <= 0 {
		hourly[hourOf(to)] += amount
		return
	}
	for cur := from; cur.Before(to); {
		h := hourOf(cur)
		next := time.Unix((h+1)*3600, 0)
		if next.After(to) {
			next = to
		}
		hourly[h] += amount * float64(next.Sub(cur)) / float64(total)
		cur = next
	}
}

// learnRatio is cap(5h) / cap(7d) of one login from its own readings: over
// each 5h period, the rise of the 7d percent divided by the rise of the 5h
// percent, both read from the same reports, summed over the periods with a
// 5h rise of at least ratioMinRise and no 7d reset inside. ok is false when
// fewer than ratioMinPeriods periods qualify, or the sum is not positive.
func learnRatio(readings []usagelog.PercentReading) (ratio float64, ok bool) {
	type pair struct {
		short, long     float64
		shortAt, longAt time.Time
	}
	byReport := map[int64]*pair{}
	var order []int64
	for _, r := range readings {
		short, long := isShort(r.Window), isLong(r.Window)
		if !short && !long {
			continue
		}
		p := byReport[r.ReportID]
		if p == nil {
			p = &pair{short: -1, long: -1}
			byReport[r.ReportID] = p
			order = append(order, r.ReportID)
		}
		if short {
			p.short, p.shortAt = r.Percent, r.ResetsAt
		} else {
			p.long, p.longAt = r.Percent, r.ResetsAt
		}
	}
	var (
		sumShort, sumLong float64
		periods           int
		first, last       *pair
		hiShort, hiLong   float64
		longChanged       bool
	)
	closePeriod := func() {
		if first != nil && !longChanged {
			dShort, dLong := hiShort-first.short, hiLong-first.long
			if dShort >= ratioMinRise && dLong >= 0 {
				sumShort += dShort
				sumLong += dLong
				periods++
			}
		}
	}
	for _, id := range order {
		p := byReport[id]
		if p.short < 0 || p.long < 0 || p.shortAt.IsZero() {
			continue
		}
		if first == nil || newPeriod(last.shortAt, p.shortAt) {
			closePeriod()
			first, hiShort, hiLong, longChanged = p, p.short, p.long, false
		} else if newPeriod(last.longAt, p.longAt) {
			longChanged = true
		}
		hiShort, hiLong = max(hiShort, p.short), max(hiLong, p.long)
		last = p
	}
	closePeriod()
	if periods < ratioMinPeriods || sumShort <= 0 || sumLong <= 0 {
		return 0, false
	}
	return sumLong / sumShort, true
}

// demand is what the pick scores with, rebuilt once per clock hour
// (Pool.demandAt): the pool's profile and each login's 5h/7d ratio.
type demand struct {
	hour    int64
	profile *demandProfile
	ratio   map[string]float64 // by account key; missing = defaultShortRatio
}

func (d *demand) shortRatio(key string) float64 {
	if r, ok := d.ratio[key]; ok {
		return r
	}
	return defaultShortRatio
}

// buildDemand reads every account's stored percent figures and builds the
// profile (the pool's summed 7d demand over the completed hours since its
// first figure) and the ratios. An account with no store, or whose figures
// cannot be read, adds nothing.
func buildDemand(accounts []*Account, now time.Time) *demand {
	d := &demand{hour: hourOf(now), ratio: map[string]float64{}}
	hourly := map[int64]float64{}
	start := int64(math.MaxInt64)
	seen := map[*usagelog.Store]map[string]bool{}
	for _, a := range accounts {
		if a == nil || a.history == nil {
			continue
		}
		st := a.history()
		if st == nil {
			continue
		}
		if seen[st] == nil {
			seen[st] = map[string]bool{}
		}
		if seen[st][a.Key()] {
			continue
		}
		seen[st][a.Key()] = true
		rs, err := st.PercentReadings(a.Key())
		if err != nil {
			poolLog.Warnf("read the usage history of account %s: %v", a.Key(), err)
			continue
		}
		if len(rs) == 0 {
			continue
		}
		start = min(start, hourOf(rs[0].ReceivedAt))
		addHourlyDemand(hourly, rs)
		if r, ok := learnRatio(rs); ok {
			d.ratio[a.Key()] = r
		}
	}
	if start < d.hour {
		d.profile = newDemandProfile(hourly, start, d.hour)
	}
	return d
}

// score is a's T-profile score at now (see the file comment); larger is
// better. An account with no stored figure counts as unused. A window whose
// limit is reached in its current period (any window, as the #3186 rule
// counts it) scores 0.
func (d *demand) score(a *Account, now time.Time) float64 {
	shortLeft, shortEnd := 100.0, now.Add(5*time.Hour)
	longLeft, longEnd := 100.0, now.Add(7*24*time.Hour)
	if a != nil && a.history != nil {
		if st := a.history(); st != nil {
			cur, err := st.Current(a.Key(), now)
			if err != nil {
				poolLog.Warnf("read the usage of account %s: %v", a.Key(), err)
			}
			for _, w := range cur.Windows {
				live := w.ResetsAt.IsZero() || w.ResetsAt.After(now)
				if w.LimitReached && live {
					return 0
				}
				short, long := isShort(w.Window), isLong(w.Window)
				if (!short && !long) || !w.HasPercent {
					continue
				}
				left := max(0, 100-w.Percent)
				end := now.Add(w.Length)
				if !w.ResetsAt.IsZero() {
					end = w.ResetsAt
				}
				if short {
					shortLeft, shortEnd = left, maxTime(end, now.Add(shortResetFloor))
				} else {
					longLeft, longEnd = left, maxTime(end, now.Add(longResetFloor))
				}
			}
		}
	}
	shortLeft *= d.shortRatio(a.Key())
	return min(shortLeft/d.expect(now, shortEnd), longLeft/d.expect(now, longEnd))
}

func (d *demand) expect(now, end time.Time) float64 {
	return max(1e-9, d.profile.expected(now, end))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
