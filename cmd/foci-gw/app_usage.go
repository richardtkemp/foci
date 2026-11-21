package main

import (
	"math"
	"time"

	"foci/internal/delegator"
	"foci/internal/fap"
	"foci/internal/usagelog"
)

// The app's per-agent allowance indicator (#3151). An agent whose backend has
// a usage source gets an app usage source that reads its account's newest
// stored state from the usage history (#3103: scheduled reads and the
// stream's rate-limit figures alike), so the roster shows what /mana cached
// would show. Only time windows with a figure for the current period are
// sent; an account with none sends nothing and the app shows no indicator.

// appUsageFromBinding returns the app usage source of the agent whose usage
// history view is b (nil b: no source).
func appUsageFromBinding(b *usagelog.Binding) func() *fap.AgentUsage {
	if b == nil {
		return nil
	}
	return func() *fap.AgentUsage {
		cur, now, err := b.Current()
		if err != nil {
			mainLog.Debugf("app usage for %s: %v", b.AgentID, err)
			return nil
		}
		return appUsageFrom(cur, now)
	}
}

// appUsageFrom is the wire form of an account's stored state at now: one
// entry per window with a known length and a percent figure for its current
// period, in Current's order (shortest window first). Nil when there is none.
func appUsageFrom(cur usagelog.Current, now time.Time) *fap.AgentUsage {
	var ws []fap.UsageWindow
	for _, w := range cur.Windows {
		if w.Length <= 0 || !w.HasPercent {
			continue // not a time window (overage, a bucket's limit), or no figure this period
		}
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			continue // reset since the figure: the new period's use is unknown
		}
		uw := fap.UsageWindow{
			Label:        delegator.UsageWindowLabel(w.Window),
			LengthMs:     w.Length.Milliseconds(),
			UsedPct:      int(math.Round(math.Min(math.Max(w.Percent, 0), 100))),
			LimitReached: w.LimitReached,
		}
		if !w.ResetsAt.IsZero() {
			uw.ResetsMs = w.ResetsAt.UnixMilli()
		}
		if !w.PercentReceivedAt.IsZero() {
			uw.AsOfMs = w.PercentReceivedAt.UnixMilli()
		}
		ws = append(ws, uw)
	}
	if len(ws) == 0 {
		return nil
	}
	return &fap.AgentUsage{Windows: ws}
}

// usageLogin is one login of an agent's pool as the app's usage source sees
// it (#3253): its name for a person, whether a new conversation would start
// on it now (the pick), and its usage source (nil: none).
type usageLogin struct {
	name   string
	picked bool
	read   func() *fap.AgentUsage
}

// appUsageOfLogins is the app usage of an agent on logins, in the agent's
// configured order. With one login it is that login's usage, as before
// #3253. With two or more, Windows is the pick's (what an app that does not
// read Accounts shows) and Accounts holds every login, one with no current
// figure included with no windows so each login keeps its place. Nil when no
// login has a figure.
func appUsageOfLogins(logins []usageLogin) *fap.AgentUsage {
	readOf := func(l usageLogin) *fap.AgentUsage {
		if l.read == nil {
			return nil
		}
		return l.read()
	}
	if len(logins) == 1 {
		return readOf(logins[0])
	}
	var out fap.AgentUsage
	seen := false
	for _, l := range logins {
		a := fap.AccountUsage{Name: l.name, Picked: l.picked, Windows: []fap.UsageWindow{}}
		if u := readOf(l); u != nil && len(u.Windows) > 0 {
			a.Windows = u.Windows
			seen = true
			if l.picked {
				out.Windows = u.Windows
			}
		}
		out.Accounts = append(out.Accounts, a)
	}
	if !seen {
		return nil
	}
	return &out
}
