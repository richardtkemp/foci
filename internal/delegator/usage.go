package delegator

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// usage.go — backend-contributed /mana (plan usage) query.
//
// A backend whose account has a plan-usage concept registers a UsageQuery from
// its init() (alongside Register). The command layer registers /mana (alias
// /usage) iff a query exists for the agent's backend, the same shape as /plan
// (plan.go, #857). Claude Code and Codex register one; each reports only its
// own account's numbers, so no agent is shown another backend's allowance
// (#1543), and a backend with no usage source (opencode, API mode) gets no
// /mana at all.

// UsageInfo is a backend's plan/rate-limit usage. Backend names the backend
// the numbers describe and heads the reply. The rest has two shapes:
//
//   - Allowance nil: Claude Code's plan usage — the data behind CC's /usage
//     command (session/weekly plan limits, cost, and the "what's
//     contributing" behavior breakdown), in the fields below.
//   - Allowance set: a per-bucket allowance (Codex). The Claude Code fields
//     are then unset and not rendered.
type UsageInfo struct {
	Backend          string // display name, e.g. "Claude Code", "Codex"
	Allowance        *Allowance
	SubscriptionType string
	// RateLimitsAvailable is CC's rate_limits_available: the read carries
	// the account's rate-limit windows.
	RateLimitsAvailable bool
	FiveHour            UsageWindow // CC's "session" limit
	SevenDay            UsageWindow // CC's "week (all models)" limit
	SessionCostUSD      float64
	Day                 UsageBehaviorWindow
	Week                UsageBehaviorWindow
	Raw                 json.RawMessage // the full response as received (CC get_usage; codex account/rateLimits/read on a fresh /usage), for anything not modeled above and the usage history
}

// UsageWindow is one rate-limit window's utilization. Percent is 0-100
// (CC's own scale for get_usage). ResetsAt is the zero Time if CC omitted
// or sent an unparseable resets_at.
type UsageWindow struct {
	Percent  int
	ResetsAt time.Time
	// Reported is true when the read carried this window (CC sends null for
	// a window it has no figure for, which reads as 0%).
	Reported bool
}

// Available reports whether this read shows usage available at a hold
// threshold of thresholdPct (#3043): it is a Claude Code read (no
// Allowance) that carries its rate-limit windows, reports at least one, and
// every one it reports is under the threshold. A read that cannot show it
// (no windows, a codex allowance) is not available: the caller must not
// lift a gate on it.
func (u *UsageInfo) Available(thresholdPct int) bool {
	if u == nil || u.Allowance != nil || !u.RateLimitsAvailable {
		return false
	}
	seen := false
	for _, w := range []UsageWindow{u.FiveHour, u.SevenDay} {
		if !w.Reported {
			continue
		}
		if w.Percent >= thresholdPct {
			return false
		}
		seen = true
	}
	return seen
}

// UsageBehaviorWindow is one window ("last 24h"/"last 7d") of what's
// contributing to plan-limit usage.
type UsageBehaviorWindow struct {
	RequestCount int
	SessionCount int
	Top          []UsageBehaviorItem // CC's own ordering
}

// UsageBehaviorItem is one contributing factor, e.g. {Key: "long_context", Pct: 88}.
type UsageBehaviorItem struct {
	Key   string
	Pct   int
	Count int
}

// Allowance is a usage allowance reported as metered buckets, each with its
// own windows, credits and limit state (Codex: account/rateLimits).
type Allowance struct {
	PlanType string // e.g. "team"; "" if unknown
	Buckets  []AllowanceBucket
	// AsOf is when foci last received these numbers from the backend (zero
	// when it has none). Fresh is true when they were read for this request.
	AsOf  time.Time
	Fresh bool
	// Note explains a stale or empty reply, e.g. why no fresh read was made.
	Note string
}

// AllowanceBucket is one metered bucket. Name prefixes its lines in the reply:
// "" for the backend's main bucket (codex's "codex"), else e.g. "premium".
type AllowanceBucket struct {
	Name         string
	Windows      []AllowanceWindow
	Credits      string // "unlimited", a balance, or "" when none is reported
	LimitReached string // in words; "" when no limit is reached
}

// AllowanceWindow is one usage window of a bucket.
type AllowanceWindow struct {
	Label    string // window length, e.g. "5h", "7d"
	Percent  int    // used, 0-100
	ResetsAt time.Time
}

// UsageTarget is the agent a usage read is for (#3104): its id and the parts
// of its backend config that decide which account a read reaches and how.
type UsageTarget struct {
	AgentID string
	// Env is the agent's backend_config.env (nil for none). For Claude Code it
	// can name the agent's own config folder, and so its own account.
	Env map[string]string
	// Binary is the agent's backend_config.binary ("" = the backend's
	// default).
	Binary string
}

// UsageQuery fetches the plan usage of the account t's agent runs under:
// Claude Code reads the agent's own login (its env and binary), codex reads
// the agent's own process.
type UsageQuery func(ctx context.Context, t UsageTarget) (*UsageInfo, error)

// UsageQueryFor returns the usage query of a backend (Spec.UsageQuery), and
// whether it has one. The command layer registers /mana iff ok is true.
func UsageQueryFor(name string) (UsageQuery, bool) {
	s, _ := SpecFor(name)
	return s.UsageQuery, s.UsageQuery != nil
}

// --- usage reports: every usage figure foci receives (#3103) ---
//
// A backend that reports plan usage (CapUsageQuery) also hands every usage
// figure it receives outside a UsageQuery (Claude Code's rate_limit_event,
// codex's account/rateLimits/updated and its start-up read) to the host
// through UsageReporter (CapUsageReports, which CapUsageQuery requires). The
// gateway stamps each report with the time, backend, account and agent and
// stores it (internal/usagelog). The figures a UsageQuery returns reach the
// store through the query's own result.

// Usage report sources: where a stored figure came from.
const (
	UsageSourceScheduled   = "scheduled" // the spawn reserve's scheduled read (#2662)
	UsageSourceSpawn       = "spawn"     // a read a spawn's allowance check started
	UsageSourceMana        = "mana"      // a fresh /mana or /usage read
	UsageSourceCCEvent     = "cc_rate_limit_event"
	UsageSourceCodexUpdate = "codex_rate_limits_updated"
	UsageSourceCodexRead   = "codex_startup_read"
)

// Units of UsageReportWindow.Used.
const (
	UsageUnitPercent  = "percent"  // 0-100
	UsageUnitFraction = "fraction" // 0-1
)

// UsageReport is one usage report foci received. The backend fills Source,
// Windows and Payload; the host fills the rest when it stores the report.
type UsageReport struct {
	Source  string
	Windows []UsageReportWindow
	// Payload is the report as received (the event's JSON line, the read's
	// response), so no reported field is lost.
	Payload json.RawMessage

	ReceivedAt time.Time
	Backend    string // Spec.Name, e.g. "claude-code"
	Account    string // the usage account the figures describe
	AgentID    string // the agent through which they came
}

// UsageReportWindow is one window's figures in a report, as reported.
type UsageReportWindow struct {
	// Window names the window. Claude Code: its own type ("five_hour",
	// "seven_day", ...). Codex: "<bucket>/<length>", e.g. "codex/5h"; a
	// bucket's limit state with no window of its own is "<bucket>/limit".
	Window string
	// Length is the window's length; zero when unknown or not a time window.
	Length time.Duration
	// Used is the use as reported, in UsedUnit; nil when the report carries
	// none (a Claude Code "allowed" event).
	Used     *float64
	UsedUnit string
	ResetsAt time.Time // zero = not reported
	// Status is the backend's own word for the window's state, when it sends
	// one (Claude Code: allowed, allowed_warning, rejected; codex: the
	// bucket's rateLimitReachedType).
	Status string
	// LimitReached is true when the report says the window's limit is
	// reached.
	LimitReached bool
}

// Percent is the window's use on the 0-100 scale, and whether it has one.
func (w UsageReportWindow) Percent() (float64, bool) {
	if w.Used == nil {
		return 0, false
	}
	if w.UsedUnit == UsageUnitFraction {
		return *w.Used * 100, true
	}
	return *w.Used, true
}

// UsageSink receives a backend's usage reports. It must not block for long:
// a backend may call it from the goroutine that reads its process.
type UsageSink func(UsageReport)

// UsageReporter is implemented by a backend that hands its usage reports to
// the host (CapUsageReports). The gateway calls SetUsageSink once per
// instance, before Start.
type UsageReporter interface {
	SetUsageSink(sink UsageSink)
}

// UsageWindowLength is the length of a named window: Claude Code's
// "five_hour…" and "seven_day…" types, and a codex "<bucket>/<n>{m,h,d}"
// window. Zero when the name does not fix it.
func UsageWindowLength(window string) time.Duration {
	switch {
	case strings.HasPrefix(window, "five_hour"):
		return 5 * time.Hour
	case strings.HasPrefix(window, "seven_day"):
		return 7 * 24 * time.Hour
	}
	if i := strings.LastIndexByte(window, '/'); i >= 0 {
		return labelLength(window[i+1:])
	}
	return 0
}

// labelLength parses a window label such as "5h", "7d" or "90m".
func labelLength(label string) time.Duration {
	if len(label) < 2 {
		return 0
	}
	n, err := strconv.Atoi(label[:len(label)-1])
	if err != nil || n <= 0 {
		return 0
	}
	switch label[len(label)-1] {
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	}
	return 0
}

// FreshRead reports whether u holds figures read for this call: a Claude
// Code read always does; a codex allowance only when Fresh (else it repeats
// what codex pushed earlier, which was stored when it arrived).
func (u *UsageInfo) FreshRead() bool {
	if u == nil {
		return false
	}
	return u.Allowance == nil || u.Allowance.Fresh
}

// ReportWindows lists the window figures of a read, in the shape a stored
// usage report keeps them.
func (u *UsageInfo) ReportWindows() []UsageReportWindow {
	if u == nil {
		return nil
	}
	if a := u.Allowance; a != nil {
		var out []UsageReportWindow
		for _, b := range a.Buckets {
			bucket := b.Name
			if bucket == "" {
				bucket = "codex"
			}
			for _, w := range b.Windows {
				pct := float64(w.Percent)
				out = append(out, UsageReportWindow{
					Window: bucket + "/" + w.Label, Length: labelLength(w.Label),
					Used: &pct, UsedUnit: UsageUnitPercent, ResetsAt: w.ResetsAt,
					Status: b.LimitReached, LimitReached: b.LimitReached != "",
				})
			}
			if len(b.Windows) == 0 && b.LimitReached != "" {
				out = append(out, UsageReportWindow{Window: bucket + "/limit", Status: b.LimitReached, LimitReached: true})
			}
		}
		return out
	}
	var out []UsageReportWindow
	for _, w := range []struct {
		name string
		win  UsageWindow
	}{{"five_hour", u.FiveHour}, {"seven_day", u.SevenDay}} {
		if !w.win.Reported {
			continue
		}
		pct := float64(w.win.Percent)
		out = append(out, UsageReportWindow{
			Window: w.name, Length: UsageWindowLength(w.name),
			Used: &pct, UsedUnit: UsageUnitPercent, ResetsAt: w.win.ResetsAt,
		})
	}
	return out
}
