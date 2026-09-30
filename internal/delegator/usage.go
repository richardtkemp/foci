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
// (plan.go, #857). Today only the Claude Code backends register one: the data
// is the operator's Claude Code plan usage, which means nothing to an opencode,
// codex or API-mode agent (#1543).

// UsageInfo is a backend's plan/rate-limit usage — for Claude Code, the data
// behind CC's /usage command (session/weekly plan limits, cost, and the
// "what's contributing" behavior breakdown), in structured form.
type UsageInfo struct {
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

// UsageQuery fetches the plan usage for the account a backend runs under.
type UsageQuery func(ctx context.Context) (*UsageInfo, error)

var usageQueries = make(map[string]UsageQuery)

// RegisterUsage associates a usage query with a backend name. Typically called
// from a backend package's init(), alongside Register. Backends that never call
// this simply don't get a /mana command.
func RegisterUsage(name string, q UsageQuery) {
	registryMu.Lock()
	defer registryMu.Unlock()
	usageQueries[name] = q
}

// UsageQueryFor returns the usage query registered for a backend name, and
// whether one exists. The command layer registers /mana iff ok is true.
func UsageQueryFor(name string) (UsageQuery, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	q, ok := usageQueries[name]
	return q, ok
}
