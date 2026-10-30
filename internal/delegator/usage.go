package delegator

import (
	"context"
	"encoding/json"
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
	FiveHour         UsageWindow // CC's "session" limit
	SevenDay         UsageWindow // CC's "week (all models)" limit
	SessionCostUSD   float64
	Day              UsageBehaviorWindow
	Week             UsageBehaviorWindow
	Raw              json.RawMessage // full get_usage response payload, for anything not modeled above
}

// UsageWindow is one rate-limit window's utilization. Percent is 0-100
// (CC's own scale for get_usage). ResetsAt is the zero Time if CC omitted
// or sent an unparseable resets_at.
type UsageWindow struct {
	Percent  int
	ResetsAt time.Time
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

// UsageQuery fetches the plan usage for the account agentID runs under. A
// backend whose usage is operator-wide (Claude Code) ignores agentID.
type UsageQuery func(ctx context.Context, agentID string) (*UsageInfo, error)

// UsageQueryFor returns the usage query of a backend (Spec.UsageQuery), and
// whether it has one. The command layer registers /mana iff ok is true.
func UsageQueryFor(name string) (UsageQuery, bool) {
	s, _ := SpecFor(name)
	return s.UsageQuery, s.UsageQuery != nil
}
