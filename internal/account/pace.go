package account

import (
	"slices"
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
// allowance is used up exactly at its reset. An agent's pace is the mean of
// its logins' paces.
//
// The use is the rise of the 5h percent times the login's learned 5h/7d
// ratio: both figures are whole percents, and one hour of the 7d percent is
// too coarse.
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
	// minLongLeft is the least 7d allowance left used as a divisor: the figure
	// is a whole percent, so 100% used may have up to this much left.
	minLongLeft = 0.5

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
	d := p.demandAt(now)
	// The earliest figure a pace in the lookback reads: one window before it,
	// and that window's 5h period from its start.
	since := now.Add(-paceLookback - paceWindow - shortLength)
	var logins []*loginPace
	for _, a := range p.accounts {
		if l := newLoginPace(d, a, since); l != nil {
			logins = append(logins, l)
		}
	}
	return overPace(logins, now)
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
		pace, ok := meanPace(logins, t)
		switch {
		case !ok || pace < paceOff:
			return false
		case pace >= paceOn:
			return true
		}
	}
	return false
}

// meanPace is the mean pace at t of the logins that have one; ok is false
// when none has.
func meanPace(logins []*loginPace, t time.Time) (pace float64, ok bool) {
	var sum float64
	n := 0
	for _, l := range logins {
		if v, ok := l.at(t); ok {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// loginPace is what one login's pace is worked out from.
type loginPace struct {
	profile  *demandProfile
	ratio    float64                   // the login's cap(5h) / cap(7d)
	readings []usagelog.PercentReading // oldest first
	short    []rise                    // the rises of the 5h percent
}

// newLoginPace reads a's figures since; nil when there are none.
func newLoginPace(d *demand, a *Account, since time.Time) *loginPace {
	if a == nil || a.history == nil {
		return nil
	}
	st := a.history()
	if st == nil {
		return nil
	}
	rs, err := st.PercentReadingsSince(a.Key(), since)
	if err != nil {
		poolLog.Warnf("read the usage history of account %s: %v", a.Key(), err)
		return nil
	}
	if len(rs) == 0 {
		return nil
	}
	return &loginPace{profile: d.profile, ratio: d.shortRatio(a.Key()), readings: rs, short: rises(rs, isShort)}
}

// at is the login's pace at t, from the figures received by then; ok is
// false unless they hold both a 5h and a 7d figure (a codex plan with no 5h
// window has no pace). A 7d window that reset since its last figure is unused
// and resets one length later.
func (l *loginPace) at(t time.Time) (pace float64, ok bool) {
	var long *usagelog.PercentReading
	hasShort := false
	for i := range l.readings {
		r := &l.readings[i]
		if r.ReceivedAt.After(t) {
			break
		}
		if isLong(r.Window) {
			long = r
		}
		hasShort = hasShort || isShort(r.Window)
	}
	if long == nil || !hasShort {
		return 0, false
	}
	left, reset := 100-long.Percent, long.ResetsAt
	if reset.IsZero() {
		reset = t.Add(longLength)
	}
	for !reset.After(t) {
		left, reset = 100, reset.Add(longLength)
	}
	rate := l.shortUse(t.Add(-paceWindow), t) * l.ratio / paceWindow.Hours()
	busyHours := l.profile.expected(t, reset) / l.profile.busyHour()
	return rate * busyHours / max(minLongLeft, left), true
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
