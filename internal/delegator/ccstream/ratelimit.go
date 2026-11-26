package ccstream

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/timeutil"
	"foci/internal/usagehold"
)

// fireRateLimited invokes the rate-limit warning hook if one is registered,
// passing this Backend's owning foci session key so the handler can deliver the
// notice to the session that triggered it rather than only the default chat
// (#1857). Safe to call whether or not a hook is set.
func (b *Backend) fireRateLimited(detail string) {
	if b.onRateLimited != nil {
		b.onRateLimited(b.startOpts.SessionKey, detail)
	}
}

// reportUsageWindow feeds one rate_limit_event to the account-wide usage hold
// (#2491): the five-hour and weekly windows, with their use, whether they are
// rejected, and when they reset. Every event is reported, "allowed" ones too:
// an event under the threshold is what clears its window.
func (b *Backend) reportUsageWindow(info RateLimitInfo) {
	if b.onUsageWindow == nil || (info.RateLimitType != "five_hour" && info.RateLimitType != "seven_day") {
		return
	}
	ev := usagehold.Event{Window: info.RateLimitType, Reached: info.Status == "rejected"}
	if info.Utilization != nil {
		ev.Util, ev.HasUtil = *info.Utilization, true
	}
	if info.ResetsAt != nil {
		ev.ResetAt = time.Unix(int64(*info.ResetsAt), 0)
	}
	if !ev.HasUtil && !ev.Reached {
		return // nothing to judge the window by
	}
	b.onUsageWindow(ev)
}

// SetUsageSink implements delegator.UsageReporter (#3103): every
// rate_limit_event, of every type, goes to sink.
func (b *Backend) SetUsageSink(sink delegator.UsageSink) { b.usageSink = sink }

// reportUsageEvent hands one rate_limit_event to the usage store: its window,
// utilization (a 0-1 fraction, absent on "allowed"), reset and status, and
// the event line as received.
func (b *Backend) reportUsageEvent(ev *RateLimitEvent) {
	if b.usageSink == nil {
		return
	}
	info := ev.RateLimitInfo
	w := delegator.UsageReportWindow{
		Window:       info.RateLimitType,
		Length:       delegator.UsageWindowLength(info.RateLimitType),
		Status:       info.Status,
		LimitReached: info.Status == "rejected",
	}
	if info.Utilization != nil {
		u := *info.Utilization
		w.Used, w.UsedUnit = &u, delegator.UsageUnitFraction
	}
	if info.ResetsAt != nil {
		w.ResetsAt = time.Unix(int64(*info.ResetsAt), 0)
	}
	payload := ev.Raw
	if len(payload) == 0 {
		payload, _ = json.Marshal(ev)
	}
	b.usageSink(delegator.UsageReport{
		Source:  delegator.UsageSourceCCEvent,
		Windows: []delegator.UsageReportWindow{w},
		Payload: payload,
	})
}

// fireSessionLimit invokes the session-limit hook if one is registered.
// Safe to call whether or not a hook is set.
func (b *Backend) fireSessionLimit(signal ratelimit.Signal) {
	if b.onSessionLimit != nil {
		b.onSessionLimit(signal)
	}
}

// rejectedWindowSignal is the gate signal for a rejected rate_limit_event:
// the window's resetsAt when it is still ahead, else no hint (the shared
// usage fallback applies).
func rejectedWindowSignal(info RateLimitInfo, now time.Time) ratelimit.Signal {
	sig := ratelimit.Signal{Kind: ratelimit.KindUsage,
		Detail: fmt.Sprintf("Claude Code %s limit rejected", rateLimitWindowLabel(info.RateLimitType))}
	if info.ResetsAt != nil {
		if reset := time.Unix(int64(*info.ResetsAt), 0); reset.After(now) {
			sig.ResetAt = reset
		}
	}
	return sig
}

// syntheticLimitRe matches CC's synthetic limit phrase for any limit kind:
// "hit your session limit", "hit your weekly limit", "hit your five-hour
// limit" (#2488), "hit your Fable limit · resets …" (CC 2.1.325 -p), and the
// "reached your Fable limit" wording CC also uses (#3102).
var syntheticLimitRe = regexp.MustCompile(`(?i)\b(?:hit|reached) your [a-z0-9-]+ limit\b`)

// ccLimitPrefixes is Claude Code's own list of the openings of its "you cannot
// use Claude now" messages, read from the CC 2.1.325 binary (the list its
// limit-message check reads, beside the "<model> requires usage credits"
// pattern, syntheticModelBlockRe) (#3262). CC builds these when the plan limit
// rejected a call and usage credits could not cover it: "You're out of usage
// credits · resets 3pm (Europe/London)", "Your org is out of usage · add funds
// to continue", "You've hit your team's shared budget", and so on. Only the
// first two openings were caught before, and only with a one-word limit name
// (syntheticLimitRe). The "·" is U+00B7, as CC writes it.
var ccLimitPrefixes = []string{
	"You've hit your",
	"You've reached your",
	"You're out of usage credits",
	"Your org is out of usage · add funds to continue",
	"Your org is out of usage · contact your admin",
	"Your seat type doesn't include usage credits",
	"Your seat type doesn't include usage",
	"Your usage allocation has been disabled by your admin",
	"Your group's usage limit is set to $0",
	"Fable 5 requires usage credits",
	"You're out of extra usage",
	"Your seat type doesn't include extra usage",
}

// isCCLimitText reports whether s is one of CC's limit messages, by CC's own
// rule: it starts with one of ccLimitPrefixes or matches the model-credits
// pattern.
func isCCLimitText(s string) bool {
	for _, p := range ccLimitPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return syntheticModelBlockRe.MatchString(s)
}

// sessionLimitResetDateRe matches the dated reset clause of CC's longer
// limits, e.g. "resets Oct 12, 12am (Europe/London)" (#2488), and the form
// with a year CC writes when the reset is not in the current year, e.g.
// "resets Jan 2, 2027, 3pm (Europe/London)" (#3106, F2 of #3102).
// Submatches: 1 month, 2 day, 3 year (optional), 4 hour, 5 minute
// (optional), 6 am/pm, 7 zone.
var sessionLimitResetDateRe = regexp.MustCompile(`(?i)resets\s+([a-z]{3})[a-z]*\s+(\d{1,2}),?\s+(?:(\d{4}),?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

// sessionLimitResetRe matches CC's session-limit reset clause, e.g.
// "resets 11:30pm (Europe/London)" or "resets 6pm (Europe/London)".
var sessionLimitResetRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

// parseSessionLimitReset extracts the reset instant from a CC session-limit
// message like "You've hit your session limit · resets 11:30pm (Europe/London)".
// It returns a neutral signal containing the next future occurrence of that
// wall-clock time in the named zone, and false if the clause is absent or the
// zone is unknown.
func parseSessionLimitReset(text string, now time.Time) (ratelimit.Signal, bool) {
	if d := sessionLimitResetDateRe.FindStringSubmatch(text); d != nil {
		return parseDatedLimitReset(text, d, now)
	}
	m := sessionLimitResetRe.FindStringSubmatch(text)
	if m == nil {
		return ratelimit.Signal{}, false
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch {
	case strings.EqualFold(m[3], "pm") && hour != 12:
		hour += 12
	case strings.EqualFold(m[3], "am") && hour == 12:
		hour = 0
	}
	loc, err := time.LoadLocation(strings.TrimSpace(m[4]))
	if err != nil {
		return ratelimit.Signal{}, false
	}
	n := now.In(loc)
	reset := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = reset.Add(24 * time.Hour)
	}
	return ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: reset, Detail: text}, true
}

// parseDatedLimitReset resolves a dated reset clause (sessionLimitResetDateRe
// submatches d) to an instant: the named month and day at the wall-clock time
// in the named zone. With a year in the clause, that year; without one, this
// year or, when that has passed, next year.
func parseDatedLimitReset(text string, d []string, now time.Time) (ratelimit.Signal, bool) {
	month, err := time.Parse("Jan", strings.ToUpper(d[1][:1])+strings.ToLower(d[1][1:3]))
	if err != nil {
		return ratelimit.Signal{}, false
	}
	day, _ := strconv.Atoi(d[2])
	hour, _ := strconv.Atoi(d[4])
	minute := 0
	if d[5] != "" {
		minute, _ = strconv.Atoi(d[5])
	}
	switch {
	case strings.EqualFold(d[6], "pm") && hour != 12:
		hour += 12
	case strings.EqualFold(d[6], "am") && hour == 12:
		hour = 0
	}
	loc, err := time.LoadLocation(strings.TrimSpace(d[7]))
	if err != nil {
		return ratelimit.Signal{}, false
	}
	if d[3] != "" {
		year, _ := strconv.Atoi(d[3])
		reset := time.Date(year, month.Month(), day, hour, minute, 0, 0, loc)
		return ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: reset, Detail: text}, true
	}
	n := now.In(loc)
	reset := time.Date(n.Year(), month.Month(), day, hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = time.Date(n.Year()+1, month.Month(), day, hour, minute, 0, 0, loc)
	}
	return ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: reset, Detail: text}, true
}

// rateLimitWarnState is the per-window high-water mark used to throttle
// rate-limit warnings. Keyed by "status|type"; resetsAt identifies the
// specific limit window so a new window re-arms warnings.
type rateLimitWarnState struct {
	resetsAt int64 // the window's reset instant; a change means a fresh window
	bucket   int   // highest utilization bucket already warned for this window
}

// RateLimitThrottle holds rate-limit warning throttle state. It is shared
// across all Backends for an agent (agentThrottle, bound at Start) so that each
// warning fires only once per utilization bucket regardless of how many
// concurrent sessions/backends are active — rate limits are account-wide.
type RateLimitThrottle struct {
	mu    sync.Mutex
	state map[string]rateLimitWarnState
	// windows is the highest usage level announced per window ("five_hour",
	// "seven_day") by EITHER source: the stream's allowed_warning notices and
	// the scheduled usage reads (#3144). Each source announces a level only
	// above it, so a level is told once whichever source saw it first.
	windows map[string]rateLimitWarnState
}

// NewRateLimitThrottle creates a rate-limit warning throttle.
func NewRateLimitThrottle() *RateLimitThrottle {
	return &RateLimitThrottle{state: make(map[string]rateLimitWarnState), windows: make(map[string]rateLimitWarnState)}
}

// sameWindowSlack is how far apart two reset instants of one window may be.
// The stream and get_usage report the same reset up to a second apart
// (usage-history.db, 2026-11-16: 1794880800 vs 1794880799); two different
// windows of one type are at least five hours apart.
const sameWindowSlack = 30 * 60 // seconds

// sameWindow reports whether two reset instants (Unix seconds, 0 = unknown)
// name the same window.
func sameWindow(a, b int64) bool {
	if a == 0 || b == 0 {
		return a == b
	}
	d := a - b
	return d >= -sameWindowSlack && d <= sameWindowSlack
}

// claimWindowLocked reports whether level is above the highest level
// announced in window (resetsAt), and records it when so. A new window
// starts with nothing announced. t.mu must be held.
func (t *RateLimitThrottle) claimWindowLocked(window string, resetsAt int64, level int) bool {
	prev, seen := t.windows[window]
	if seen && sameWindow(prev.resetsAt, resetsAt) && level <= prev.bucket {
		return false
	}
	t.windows[window] = rateLimitWarnState{resetsAt: resetsAt, bucket: level}
	return true
}

// raiseWindowLocked records level as announced in window without a check (a
// rejection, which always fires). t.mu must be held.
func (t *RateLimitThrottle) raiseWindowLocked(window string, resetsAt int64, level int) {
	if prev, seen := t.windows[window]; seen && sameWindow(prev.resetsAt, resetsAt) && level <= prev.bucket {
		return
	}
	t.windows[window] = rateLimitWarnState{resetsAt: resetsAt, bucket: level}
}

// usageReadNotices returns the notices one usage read raises: for each of the
// five-hour and weekly windows it reports, the highest of levels (the live
// [notify] usage_warning_levels, #3145) its use has reached, when that level
// is above every level already announced in that window by a read or by the
// stream. A window whose reset is at or before now is over (the reading
// predates it) and says nothing. A read that is not Claude Code's (a codex
// Allowance) raises none.
func (t *RateLimitThrottle) usageReadNotices(info *delegator.UsageInfo, levels []int, now time.Time) []string {
	if info == nil || info.Allowance != nil {
		return nil
	}
	var out []string
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range []struct {
		window string
		u      delegator.UsageWindow
	}{{"five_hour", info.FiveHour}, {"seven_day", info.SevenDay}} {
		if !w.u.Reported || w.u.ResetsAt.IsZero() || !w.u.ResetsAt.After(now) {
			continue
		}
		level := delegator.UsageLevelReached(levels, w.u.Percent)
		if level == 0 {
			continue
		}
		// #3242: use not ahead of the time gone in the window says nothing,
		// and claims nothing: a later read that is ahead announces it.
		if !delegator.UsageAheadOfWindow(float64(w.u.Percent), w.u.ResetsAt, delegator.UsageWindowLength(w.window), now) {
			continue
		}
		if !t.claimWindowLocked(w.window, w.u.ResetsAt.Unix(), level) {
			continue
		}
		util := float64(w.u.Percent) / 100
		resets := float64(w.u.ResetsAt.Unix())
		out = append(out, FormatRateLimitNotice(RateLimitInfo{
			Status: "allowed_warning", RateLimitType: w.window, Utilization: &util, ResetsAt: &resets,
		}))
	}
	return out
}

// AnnounceUsageRead returns the chat notices a usage read of agentID's
// account accountKey raises at levels (#3144, #3145): the same notices, and
// the same throttle, as the stream's rate_limit_event warnings of the
// agent's sessions on that account, so a level either source has announced
// is not announced again. The caller delivers them the way it delivers
// HostHooks.OnRateLimitNotice.
func AnnounceUsageRead(agentID, accountKey string, info *delegator.UsageInfo, levels []int, now time.Time) []string {
	return agentThrottle(agentID, accountKey).usageReadNotices(info, levels, now)
}

var (
	agentThrottlesMu sync.Mutex
	agentThrottles   = map[throttleKey]*RateLimitThrottle{}
)

// throttleKey names one throttle: an agent's sessions on one account.
type throttleKey struct{ agent, account string }

// agentThrottle is the throttle every Backend of agentID on the account
// accountKey shares (main, facet and batch sessions), so a limit of that
// login warns once per bucket however many sessions see it. An agent with
// several accounts (#3186) has one per account (#3242): two logins' windows
// are different windows even when their resets are close, and one login's
// level must not hide or re-arm another's. One per agent and account for the
// process lifetime.
func agentThrottle(agentID, accountKey string) *RateLimitThrottle {
	agentThrottlesMu.Lock()
	defer agentThrottlesMu.Unlock()
	k := throttleKey{agentID, accountKey}
	t, ok := agentThrottles[k]
	if !ok {
		t = NewRateLimitThrottle()
		agentThrottles[k] = t
	}
	return t
}

// rateLimitCheckResult holds the outcome of a throttle evaluation, for logging.
type rateLimitCheckResult struct {
	prev rateLimitWarnState
	seen bool
	fire bool
	// byRead: the stream event's level was already announced in its window
	// (by a usage read, #3144), so it does not fire.
	byRead bool
}

// evaluateStream atomically checks and updates the throttle for one stream
// rate_limit_event. It fires when: the key (status|type) is unseen, the limit
// window changed (resetsAt), or utilization climbed to a higher bucket. The
// event is also checked against the levels announced in its window by either
// source (#3144): an allowed_warning at a level a usage read already
// announced does not fire. A rejection always fires (the limit itself is
// news). Either way a firing event raises its window's announced level. An
// event with no window, no reset or no utilization skips the window check.
func (t *RateLimitThrottle) evaluateStream(key, window, status string, resetsAt int64, bucket int) rateLimitCheckResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, seen := t.state[key]
	fire := !seen || prev.resetsAt != resetsAt || bucket > prev.bucket
	res := rateLimitCheckResult{prev: prev, seen: seen, fire: fire}
	if !fire {
		return res
	}
	if window != "" && resetsAt != 0 && bucket >= 0 {
		if status == "rejected" {
			t.raiseWindowLocked(window, resetsAt, bucket)
		} else if !t.claimWindowLocked(window, resetsAt, bucket) {
			res.fire, res.byRead = false, true
		}
	}
	// The key's own mark advances even when a read had announced the level:
	// the level has been told either way.
	t.state[key] = rateLimitWarnState{resetsAt: resetsAt, bucket: bucket}
	return res
}

// rateLimitPercent is a utilization fraction (0..1) as a whole percent,
// rounded down (0.87 is 87, never 86 from float error), or -1 when it is
// unknown (nil): such an event fires once per window.
func rateLimitPercent(u *float64) int {
	if u == nil {
		return -1
	}
	pct := int(math.Floor(*u*100 + 1e-6))
	if pct < 0 {
		pct = 0
	}
	return pct
}

// rateLimitWindowLabel maps CC's rateLimitType to a human-friendly window name.
func rateLimitWindowLabel(t string) string {
	switch t {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "7-day"
	case "":
		return "usage"
	default:
		return t
	}
}

// humanUntil renders the time until reset compactly ("in 5d 1h", "in 42m").
func humanUntil(now, reset time.Time) string {
	d := reset.Sub(now)
	if d <= 0 {
		return "now"
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("in %dd %dh", days, hrs)
	case hrs > 0:
		return fmt.Sprintf("in %dh %dm", hrs, mins)
	default:
		return fmt.Sprintf("in %dm", mins)
	}
}

// describeRateLimitInfo renders every rate_limit_info field for the DEBUG log,
// "nil" for absent optional fields so absence is distinguishable from zero.
func describeRateLimitInfo(info RateLimitInfo) string {
	frac := func(f *float64) string {
		if f == nil {
			return "nil"
		}
		return fmt.Sprintf("%.4f", *f)
	}
	epoch := func(f *float64) string {
		if f == nil {
			return "nil"
		}
		return strconv.FormatInt(int64(*f), 10)
	}
	using := "nil"
	if info.IsUsingOverage != nil {
		using = strconv.FormatBool(*info.IsUsingOverage)
	}
	return fmt.Sprintf("status=%s type=%s util=%s surpassedThreshold=%s resetsAt=%s overageStatus=%s overageResetsAt=%s isUsingOverage=%s",
		info.Status, info.RateLimitType, frac(info.Utilization), frac(info.SurpassedThreshold),
		epoch(info.ResetsAt), info.OverageStatus, epoch(info.OverageResetsAt), using)
}

// FormatRateLimitNotice renders a CC structured rate_limit_event as a
// human-facing notice for delivery to the agent's chat (not a log line). It
// names the affected window, current utilization, and when the limit resets.
// CC emits the event on status transitions (allowed → allowed_warning →
// rejected); this is only called past the "allowed" threshold.
func FormatRateLimitNotice(info RateLimitInfo) string {
	window := rateLimitWindowLabel(info.RateLimitType)

	var b strings.Builder
	if info.Status == "rejected" {
		fmt.Fprintf(&b, "🛑 Anthropic rate limit reached — requests on the %s window are being rejected.", window)
	} else {
		fmt.Fprintf(&b, "⚠️ Approaching Anthropic %s rate limit.", window)
	}

	if info.Utilization != nil {
		fmt.Fprintf(&b, "\nUsage: %.0f%%", *info.Utilization*100)
	}
	if info.ResetsAt != nil {
		reset := time.Unix(int64(*info.ResetsAt), 0)
		fmt.Fprintf(&b, "\nResets: %s (%s)",
			reset.Local().Format("Mon 2 Jan 15:04"), humanUntil(timeutil.Now(), reset))
	}
	if info.OverageStatus != "" && info.OverageStatus != "allowed" {
		fmt.Fprintf(&b, "\nOverage: %s", info.OverageStatus)
	}
	return b.String()
}
