package account

import (
	"sort"
	"time"

	"foci/internal/usagelog"
)

// pace_spend.go: the pace of a login with no 5h figure (#3269, Dick
// 2026-11-24 23:42: "Why not base it on recorded API *cost*"; ruled 23:48). A
// codex plan has only a 7d window, in whole percents, so its use in the last
// hour is the dollars foci recorded for the login x its own learned 7d
// percent per dollar (learnSpendRate). The rest of the pace is pace.go's. At
// spentLevel used or more the login is over pace until its reset.
//
// A login's dollars (Options.Spend) are its backend's calls when it is the
// backend's one login, else, for a Claude Code login, the calls whose
// transcript is in its config folder (#3291). Such a login has a 5h figure,
// so its rate only sets its cap in its pool (pace.go). Use outside foci is
// not in the ledger and is not seen: it makes the rate too high and the cap
// too small.

const (
	// A rate is learned from the newest rateMaxRise percent of 7d use, and
	// needs rateMinRise percent. Over 126 one-percent steps of one login's
	// real use (#3269), a rate from 5 of them was 10% off the next 5, inside
	// the 15% between paceOff and paceOn; more than 20 was not better. The
	// maximum also lets a new plan replace an old one's rate, and bounds the
	// calls read.
	rateMinRise = 5.0
	rateMaxRise = 20.0
	// levelSlack: a codex figure goes back and forth by 1 at a step (the
	// replies of two threads cross), which is no fall of the level. Claude
	// Code's 7d figures from two sources differ by up to 1 too (#3268: of
	// the 536 falls of a 7d figure with the same reset time on the NUC and
	// the work Mac to 26 Nov, all but Cam's 96 -> 0 were at most 1).
	levelSlack = 1.0
	// slackTolerance: a figure stored as a fraction is x100 in floats, so a
	// step of 1 can read 1.00000000000001.
	slackTolerance = 1e-9
)

// fellBelow reports whether percent is below level by more than levelSlack.
func fellBelow(percent, level float64) bool {
	return percent < level-levelSlack-slackTolerance
}

// Spend is one API call of a login.
type Spend struct {
	At  time.Time // when it was billed
	USD float64
	// Priced is false when the call's cost is unknown.
	Priced bool
}

// longLevel is a login's 7d level as its readings arrive: the highest figure
// of the period. Codex also repeats old figures (an old thread's), so a figure
// below the level is taken as a fall (a reset before its time) only when the
// next one is below it too.
type longLevel struct {
	percent float64
	reset   time.Time
	known   bool
	low     bool // the last figure was below the level
}

// add takes the next 7d reading. rise is how much the level rose; fresh is
// true when the level started anew (the first figure, a new period, a fall).
func (l *longLevel) add(r usagelog.PercentReading) (rise float64, fresh bool) {
	if l.known && newPeriod(r.ResetsAt, l.reset) {
		return 0, false // a figure of an earlier period
	}
	fresh = !l.known || newPeriod(l.reset, r.ResetsAt)
	if !fresh {
		low := fellBelow(r.Percent, l.percent)
		fresh, l.low = low && l.low, low
	}
	if !r.ResetsAt.IsZero() {
		l.reset = r.ResetsAt
	}
	switch {
	case fresh:
		l.percent, l.known, l.low = r.Percent, true, false
	case r.Percent > l.percent:
		rise, l.percent = r.Percent-l.percent, r.Percent
	}
	return rise, fresh
}

// levelAt is the 7d level from at on.
type levelAt struct {
	at      time.Time
	percent float64
	reset   time.Time
}

// longLevels is the 7d level after each 7d reading, oldest first.
func longLevels(readings []usagelog.PercentReading) []levelAt {
	var (
		lv  longLevel
		out []levelAt
	)
	for _, r := range readings {
		if isLong(r.Window) {
			lv.add(r)
			out = append(out, levelAt{r.ReceivedAt, lv.percent, lv.reset})
		}
	}
	return out
}

// learnSpendRate is a login's 7d percent per dollar, from the figures
// received and the calls billed before until. Each rise of the level is
// paired with the dollars since the rise before it in the same period (from
// step to step, so the rounding of the figures does not count); a rise with a
// call of unknown cost in it is left out. ok is false with less than
// rateMinRise percent of rises or no dollars.
func learnSpendRate(readings []usagelog.PercentReading, spend func(from, to time.Time) ([]Spend, error), until time.Time) (rate float64, ok bool, err error) {
	var (
		lv    longLevel
		steps []rise
		edge  time.Time // the last rise of this period
	)
	for _, r := range readings {
		if !isLong(r.Window) || !r.ReceivedAt.Before(until) {
			continue
		}
		amount, fresh := lv.add(r)
		switch {
		case fresh:
			edge = time.Time{}
		case amount > 0:
			if !edge.IsZero() {
				steps = append(steps, rise{from: edge, to: r.ReceivedAt, amount: amount})
			}
			edge = r.ReceivedAt
		}
	}
	first, risen := len(steps), 0.0
	for first > 0 && risen < rateMaxRise {
		first--
		risen += steps[first].amount
	}
	if risen < rateMinRise {
		return 0, false, nil
	}
	steps = steps[first:]
	calls, err := spend(steps[0].from, steps[len(steps)-1].to)
	if err != nil {
		return 0, false, err
	}
	var percent, usd float64
	for _, s := range steps {
		if v, priced := sumSpend(calls, s.from, s.to); priced {
			percent += s.amount
			usd += v
		}
	}
	if percent < rateMinRise || usd <= 0 {
		return 0, false, nil
	}
	return percent / usd, true, nil
}

// sumSpend is the dollars of the calls (oldest first) in (from, to]; priced
// is false when the cost of one is unknown.
func sumSpend(calls []Spend, from, to time.Time) (usd float64, priced bool) {
	i := sort.Search(len(calls), func(i int) bool { return calls[i].At.After(from) })
	for ; i < len(calls) && !calls[i].At.After(to); i++ {
		if !calls[i].Priced {
			return 0, false
		}
		usd += calls[i].USD
	}
	return usd, true
}

// spendUse is one login's dollars, read from the ledger as far back as they
// are asked for: most answers need the last hour only, and pricing a day of
// calls takes a few hundred ms.
type spendUse struct {
	key      string // the login's
	rate     float64
	read     func(from, to time.Time) ([]Spend, error)
	from, to time.Time // calls holds (from, to]
	calls    []Spend
	failed   bool
}

// percent is the 7d percent used in (from, to]; ok is false when it is not
// known: no rate is learned, a call's cost is unknown, or the ledger cannot
// be read.
func (s *spendUse) percent(from, to time.Time) (use float64, ok bool) {
	if s.rate <= 0 || s.failed {
		return 0, false
	}
	if to.After(s.to) {
		s.from, s.to, s.calls = to, to, nil
	}
	if from.Before(s.from) {
		// One window more than asked, for the moments just before.
		start := from.Add(-paceWindow)
		calls, err := s.read(start, s.from)
		if err != nil {
			poolLog.Warnf("read the spend of account %s: %v", s.key, err)
			s.failed = true
			return 0, false
		}
		s.from, s.calls = start, append(calls, s.calls...)
	}
	usd, priced := sumSpend(s.calls, from, to)
	return usd * s.rate, priced
}
