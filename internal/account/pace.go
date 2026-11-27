package account

import (
	"slices"
	"sort"
	"time"

	"foci/internal/usagelog"
)

// pace.go: whether an agent uses its 7d allowance too fast (#3267, Dick
// 2026-11-24 22:08; the rule, "R1", ruled 22:24).
//
// A login's pace is
//
//	its 7d use per hour over the last paceWindow
//	--------------------------------------------
//	the 7d use per hour it can sustain to its 7d reset
//
// so 1 means: if every usual hour from now on is like the last one, the 7d
// allowance is used up exactly at its reset.
//
// An agent's pace is its pool's (#3291, Dick 2026-11-27: "this should
// actually be a weighted mean"; "We can imply the caps!"):
//
//	the sum of its logins' 7d use per hour, each x its cap
//	-------------------------------------------------------
//	the sum of the rates they can sustain, each x its cap
//
// so 1 means the agent's combined 7d allowance runs out, at this rate, as its
// logins renew. A login's cap is its dollars per 7d percent, 1 / its learned
// 7d percent per dollar (learnSpendRate); a login with no rate learned counts
// as the mean cap of those with one, and with none learned the caps count as
// equal. A login with no figure is left out; an idle one adds what it can
// sustain and no use. With one login the pool's pace is the login's.
//
// The use is the rise of the 5h percent times the login's learned 5h/7d
// ratio: both figures are whole percents, and one hour of the 7d percent is
// too coarse. A login with no 5h figure (codex) has its use from the dollars
// foci recorded (pace_spend.go).
//
// The sustainable rate is the 7d allowance left / the busy-hour equivalents to
// the reset: the demand the T-profile (tprofile.go) expects until then /
// the profile's demand-weighted mean hour. With no history it is the plain
// allowance left per clock hour.

const (
	// paceWindow is the span the use is measured over.
	paceWindow = time.Hour
	// An agent is over pace from paceOn until its pace is below paceOff.
	paceOn, paceOff = 1.0, 0.85
	// paceLookback bounds how far back OverPace looks for the reading that
	// set the state of a pace between paceOff and paceOn.
	paceLookback = 24 * time.Hour
	// spentLevel: a login that has used this much of its 7d allowance can
	// sustain nothing until its 7d reset, and a pool whose logins can all
	// sustain nothing is at a pace of paceOn, idle or not (#3269 for a login
	// with no 5h figure; #3273, Dick 2026-11-25 on a used-up login that
	// counted as 0: "Either count as full or null"; full chosen). In a pool
	// with another login it adds its use and nothing to what can be sustained
	// (#3291).
	spentLevel = 99.0

	shortLength = 5 * time.Hour
	longLength  = 7 * 24 * time.Hour
)

// OverPace reports whether the agent uses its 7d allowance too fast at now:
// its pace reached paceOn and has not been below paceOff since. It is worked
// out from the stored figures alone, so a restart does not change it and
// every caller gets the same answer: between the two thresholds the state is
// the one at the latest earlier reading that was outside them. False when no
// login has a figure, and for a nil pool.
func (p *Pool) OverPace(now time.Time) bool {
	if p == nil {
		return false
	}
	return overPace(p.loginPaces(now), now)
}

// loginPaces is what the paces of p's logins in the lookback from now are
// worked out from, each with its cap; a login with no figure is left out.
func (p *Pool) loginPaces(now time.Time) []*loginPace {
	d := p.demandAt(now)
	var logins []*loginPace
	for _, a := range p.accounts {
		if l := newLoginPace(d, a, now); l != nil {
			logins = append(logins, l)
		}
	}
	setCaps(logins, d)
	return logins
}

// setCaps sets each login's cap: 1 / its learned 7d percent per dollar, else
// the mean of the learned caps, else 1 (none learned: equal caps).
func setCaps(logins []*loginPace, d *demand) {
	var sum float64
	n := 0
	for _, l := range logins {
		if r := d.spendRate[l.key]; r > 0 {
			l.cap = 1 / r
			sum += l.cap
			n++
		}
	}
	for _, l := range logins {
		switch {
		case l.cap > 0:
		case n > 0:
			l.cap = sum / float64(n)
		default:
			l.cap = 1
		}
	}
}

// overPace is OverPace of the logins' readings.
func overPace(logins []*loginPace, now time.Time) bool {
	// The moments the pace is judged at: now, then the readings back from it.
	times := []time.Time{now}
	for _, l := range logins {
		for _, r := range l.readings {
			if r.ReceivedAt.Before(now) && now.Sub(r.ReceivedAt) <= paceLookback {
				times = append(times, r.ReceivedAt)
			}
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return b.Compare(a) })
	for _, t := range times {
		pace, ok := poolPace(logins, t)
		switch {
		case !ok || pace < paceOff:
			return false
		case pace >= paceOn:
			return true
		}
	}
	return false
}

// poolPace is the pace at t of the logins that have one, weighted by their
// caps (see the file comment); ok is false when none has. Logins that can
// sustain nothing (all spent) are at paceOn.
func poolPace(logins []*loginPace, t time.Time) (pace float64, ok bool) {
	var use, supply float64
	for _, l := range logins {
		if f, known := l.at(t); known {
			use += f.use * l.cap
			supply += f.supply * l.cap
			ok = true
		}
	}
	switch {
	case !ok:
		return 0, false
	case supply <= 0:
		return paceOn, true
	}
	return use / supply, true
}

// flow is one login's side of a pace at a moment, in 7d percent per hour:
// its use over the last paceWindow, and the use it can sustain to its 7d
// reset (per busy hour; 0 when spent). Its own pace is use / supply.
type flow struct{ use, supply float64 }

// loginPace is what one login's pace is worked out from.
type loginPace struct {
	key      string
	cap      float64 // dollars per 7d percent, or the stand-in (setCaps)
	profile  *demandProfile
	ratio    float64                   // the login's cap(5h) / cap(7d)
	readings []usagelog.PercentReading // oldest first
	short    []rise                    // the rises of the 5h percent
	shortAt  time.Time                 // when the first 5h figure was received; zero = none
	levels   []levelAt                 // the 7d level, when spend is set
	spend    *spendUse                 // nil = the login's dollars are not known
}

// newLoginPace reads a's figures for the paces in the lookback from now; nil
// when there are none.
func newLoginPace(d *demand, a *Account, now time.Time) *loginPace {
	if a == nil || a.history == nil {
		return nil
	}
	st := a.history()
	if st == nil {
		return nil
	}
	// The earliest figure a pace in the lookback reads: one window before it,
	// and that window's 5h period from its start.
	const span = paceLookback + paceWindow + shortLength
	rs, err := st.PercentReadingsSince(a.Key(), now.Add(-span))
	if err == nil && len(rs) == 0 {
		// Idle for longer, with what is left of its 7d period the same: it
		// adds what it can sustain to its pool (#3291).
		var cur usagelog.Current
		if cur, err = st.Current(a.Key(), now); err == nil {
			for _, w := range cur.Windows {
				if isLong(w.Window) && w.HasPercent {
					rs, err = st.PercentReadingsSince(a.Key(), w.PercentReceivedAt.Add(-span))
				}
			}
		}
	}
	if err != nil {
		poolLog.Warnf("read the usage history of account %s: %v", a.Key(), err)
		return nil
	}
	if len(rs) == 0 {
		return nil
	}
	l := &loginPace{key: a.Key(), profile: d.profile, ratio: d.shortRatio(a.Key()), readings: rs, short: rises(rs, isShort)}
	if i := slices.IndexFunc(rs, func(r usagelog.PercentReading) bool { return isShort(r.Window) }); i >= 0 {
		l.shortAt = rs[i].ReceivedAt
	}
	if a.spend != nil {
		l.levels = longLevels(rs)
		l.spend = &spendUse{key: a.Key(), rate: d.spendRate[a.Key()], read: a.spend}
	}
	return l
}

// at is the login's flow at t, from the figures received by then; ok is
// false unless they hold both a 5h and a 7d figure, or a 7d figure and the
// login's dollars (spendPace).
func (l *loginPace) at(t time.Time) (f flow, ok bool) {
	if l.shortAt.IsZero() || l.shortAt.After(t) {
		return l.spendPace(t)
	}
	n := sort.Search(len(l.readings), func(i int) bool { return l.readings[i].ReceivedAt.After(t) })
	for n--; n >= 0 && !isLong(l.readings[n].Window); n-- {
	}
	if n < 0 {
		return flow{}, false
	}
	left, reset := longLeft(l.readings[n].Percent, l.readings[n].ResetsAt, t)
	return l.flow(l.shortUse(t.Add(-paceWindow), t)*l.ratio, left, reset, t), true
}

// spendPace is at for a login with no 5h figure.
func (l *loginPace) spendPace(t time.Time) (f flow, ok bool) {
	n := sort.Search(len(l.levels), func(i int) bool { return l.levels[i].at.After(t) })
	if n == 0 {
		return flow{}, false // no 7d figure, or no dollars (spend is nil)
	}
	left, reset := longLeft(l.levels[n-1].percent, l.levels[n-1].reset, t)
	use, ok := l.spend.percent(t.Add(-paceWindow), t)
	if !ok {
		if 100-left < spentLevel {
			return flow{}, false
		}
		use = 0 // spent: over pace, whatever its use (#3273)
	}
	return l.flow(use, left, reset, t), true
}

// longLeft is the 7d allowance left at t and when it resets, of a figure that
// resets at reset. A 7d window that reset since its last figure is unused and
// resets one length later.
func longLeft(percent float64, reset, t time.Time) (left float64, next time.Time) {
	left, next = 100-percent, reset
	if next.IsZero() {
		next = t.Add(longLength)
	}
	for !next.After(t) {
		left, next = 100, next.Add(longLength)
	}
	return left, next
}

// flow is the flow of use (7d percent, over the paceWindow before t) with
// left to last until reset; a spent login (spentLevel) sustains nothing.
func (l *loginPace) flow(use, left float64, reset, t time.Time) flow {
	f := flow{use: use / paceWindow.Hours()}
	if 100-left < spentLevel {
		busyHours := l.profile.expected(t, reset) / l.profile.busyHour()
		f.supply = left / busyHours
	}
	return f
}

// shortUse is the 5h percent used in [from, to], of the rises known by to
// (each spread evenly over its time).
func (l *loginPace) shortUse(from, to time.Time) float64 {
	var use float64
	for _, r := range l.short {
		if r.to.After(to) || !r.to.After(from) {
			continue
		}
		if r.from.Before(from) {
			use += r.amount * float64(r.to.Sub(from)) / float64(r.to.Sub(r.from))
		} else {
			use += r.amount
		}
	}
	return use
}
