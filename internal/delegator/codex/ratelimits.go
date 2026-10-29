package codex

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/timeutil"
)

// ratelimits.go — the account's Codex usage allowance.
//
// codex app-server reports usage per metered bucket ("limitId"). Verified live
// against codex 0.164.0 on a ChatGPT team account (2026-10-29):
//
//   - account/rateLimits/read returns {rateLimits, rateLimitsByLimitId, …}.
//     The "codex" bucket carries the windows: primary {usedPercent,
//     windowDurationMins: 300, resetsAt} and secondary {…, 10080, …}.
//   - account/rateLimits/updated arrives with no threadId, one bucket per
//     notification. A "premium" bucket arrives with primary and secondary null
//     and only credits + rateLimitReachedType ("workspace_member_credits_depleted"
//     when the workspace is out of credits).
//   - credits.hasCredits is false on that account even while included usage
//     works, so it is NOT an out-of-credits signal; rateLimitReachedType is.
//
// The state is per foci agent, shared by every app-server process and facade
// of that agent: limits are account-wide, so a warning must fire once, not
// once per session.

// rlWarnLevels are the used-% thresholds that produce one chat warning each
// per window. 100 is "window exhausted".
var rlWarnLevels = []int{80, 95, 100}

type rateLimitState struct {
	mu    sync.Mutex
	snaps map[string]rateLimitSnapshot // by limitId
	// marks is the last level seen per warning key. The FIRST observation of a
	// key only seeds its mark and never warns: a foci restart (or a fresh
	// app-server) must not re-announce a level the user was already told
	// about. Warnings come from transitions seen while running.
	marks map[string]rlMark
}

// rlNewWindowSlack is how far resetsAt must move before it is a new window.
const rlNewWindowSlack = 300 // seconds

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

type rlMark struct {
	resetsAt int64  // window identity: a new value is a new window
	level    int    // highest rlWarnLevels entry reached (0 = none)
	reached  string // rateLimitReachedType last seen ("" = none)
}

var (
	agentRateLimitsMu sync.Mutex
	agentRateLimits   = map[string]*rateLimitState{}
)

// rateLimitsFor returns the agent's shared rate-limit state.
func rateLimitsFor(agentID string) *rateLimitState {
	agentRateLimitsMu.Lock()
	defer agentRateLimitsMu.Unlock()
	s, ok := agentRateLimits[agentID]
	if !ok {
		s = &rateLimitState{snaps: map[string]rateLimitSnapshot{}, marks: map[string]rlMark{}}
		agentRateLimits[agentID] = s
	}
	return s
}

// apply merges one bucket's snapshot and returns the chat notices it
// triggers. A null window or credits field leaves the stored one unchanged.
func (s *rateLimitState) apply(snap rateLimitSnapshot, now time.Time) []string {
	id := snap.LimitID
	if id == "" {
		id = "codex"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snaps[id]
	cur.LimitID = id
	if snap.LimitName != nil {
		cur.LimitName = snap.LimitName
	}
	if snap.Primary != nil {
		cur.Primary = snap.Primary
	}
	if snap.Secondary != nil {
		cur.Secondary = snap.Secondary
	}
	if snap.Credits != nil {
		cur.Credits = snap.Credits
	}
	cur.RateLimitReachedType = snap.RateLimitReachedType
	s.snaps[id] = cur

	var notices []string
	for _, w := range []struct {
		slot string
		win  *rateLimitWindow
	}{{"primary", snap.Primary}, {"secondary", snap.Secondary}} {
		if w.win == nil {
			continue
		}
		if n := s.windowNoticeLocked(id+"|"+w.slot, id, *w.win, now); n != "" {
			notices = append(notices, n)
		}
	}
	if n := s.reachedNoticeLocked(id, cur, now); n != "" {
		notices = append(notices, n)
	}
	return notices
}

func (s *rateLimitState) windowNoticeLocked(key, limitID string, w rateLimitWindow, now time.Time) string {
	level := 0
	for _, l := range rlWarnLevels {
		if w.usedPercent() >= l {
			level = l
		}
	}
	resetsAt := int64(0)
	if w.ResetsAt != nil {
		resetsAt = *w.ResetsAt
	}
	prev, seen := s.marks[key]
	// codex's resetsAt for the same window wobbles by a second between
	// reports (seen live: 1793786822 vs 1793786823), so only a real move
	// counts as a new window. A new window starts from no level.
	base := prev.level
	if seen && absDiff(resetsAt, prev.resetsAt) > rlNewWindowSlack {
		base = 0
	}
	store := level
	if base > store {
		store = base // same window: usage never goes down, keep the high-water mark
	}
	s.marks[key] = rlMark{resetsAt: resetsAt, level: store}
	if !seen || level <= base {
		return ""
	}
	icon := "⚠️"
	what := fmt.Sprintf("at %d%%", w.usedPercent())
	if level >= 100 {
		icon = "🛑"
		what = "used up (100%)"
	}
	msg := fmt.Sprintf("%s Codex %s usage %s.", icon, windowLabel(limitID, w), what)
	if r := resetPhrase(w.ResetsAt, now); r != "" {
		msg += " Resets " + r + "."
	}
	return msg
}

func (s *rateLimitState) reachedNoticeLocked(limitID string, cur rateLimitSnapshot, now time.Time) string {
	key := limitID + "|reached"
	reached := ""
	if cur.RateLimitReachedType != nil {
		reached = *cur.RateLimitReachedType
	}
	prev, seen := s.marks[key]
	s.marks[key] = rlMark{reached: reached}
	if !seen || reached == "" || reached == prev.reached {
		return ""
	}
	msg := "🛑 Codex limit reached: " + reachedLabel(reached) + ". Turns will fail until it is lifted."
	if r := s.earliestFullResetLocked(now); r != "" {
		msg += " Included usage resets " + r + "."
	}
	return msg
}

// earliestFullResetLocked is the reset of the soonest window that is at
// 100%, which is when included usage comes back.
func (s *rateLimitState) earliestFullResetLocked(now time.Time) string {
	var best *int64
	for _, sn := range s.snaps {
		for _, w := range []*rateLimitWindow{sn.Primary, sn.Secondary} {
			if w == nil || w.ResetsAt == nil || w.usedPercent() < 100 {
				continue
			}
			if best == nil || *w.ResetsAt < *best {
				best = w.ResetsAt
			}
		}
	}
	return resetPhrase(best, now)
}

// statusLine renders the current allowance for the status block, e.g.
// "usage: 5h 100% (resets 02:09), 7d 34% (resets Wed 4 Nov 10:07) | limit
// reached: workspace member credits depleted". Empty when nothing is known.
func (s *rateLimitState) statusLine(now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.snaps))
	for id := range s.snaps {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		// "codex" (the main bucket) first, then alphabetical.
		if (ids[i] == "codex") != (ids[j] == "codex") {
			return ids[i] == "codex"
		}
		return ids[i] < ids[j]
	})
	var windows, credits, reached []string
	for _, id := range ids {
		sn := s.snaps[id]
		for _, w := range []*rateLimitWindow{sn.Primary, sn.Secondary} {
			if w == nil {
				continue
			}
			part := fmt.Sprintf("%s %d%%", windowLabel(id, *w), w.usedPercent())
			if w.ResetsAt != nil {
				part += " (resets " + formatReset(*w.ResetsAt, now) + ")"
			}
			windows = append(windows, part)
		}
		if c := sn.Credits; c != nil {
			switch {
			case c.Unlimited:
				credits = append(credits, "unlimited")
			case c.Balance != nil && *c.Balance != "":
				credits = append(credits, *c.Balance)
			}
		}
		if sn.RateLimitReachedType != nil && *sn.RateLimitReachedType != "" {
			reached = append(reached, reachedLabel(*sn.RateLimitReachedType))
		}
	}
	var parts []string
	if len(windows) > 0 {
		parts = append(parts, "usage: "+strings.Join(windows, ", "))
	}
	if len(credits) > 0 {
		parts = append(parts, "credits: "+strings.Join(dedupe(credits), ", "))
	}
	if len(reached) > 0 {
		parts = append(parts, "limit reached: "+strings.Join(dedupe(reached), ", "))
	}
	return strings.Join(parts, " | ")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// windowLabel names a window by its length ("5h", "7d"), prefixed with the
// bucket when it is not the main "codex" one.
func windowLabel(limitID string, w rateLimitWindow) string {
	label := "window"
	if w.WindowDurationMins != nil && *w.WindowDurationMins > 0 {
		m := *w.WindowDurationMins
		switch {
		case m%1440 == 0:
			label = fmt.Sprintf("%dd", m/1440)
		case m%60 == 0:
			label = fmt.Sprintf("%dh", m/60)
		default:
			label = fmt.Sprintf("%dm", m)
		}
	}
	if limitID != "" && limitID != "codex" {
		label = limitID + " " + label
	}
	return label
}

// reachedLabel turns codex's rateLimitReachedType into words.
func reachedLabel(t string) string {
	switch t {
	case "rate_limit_reached":
		return "rate limit reached"
	case "workspace_owner_credits_depleted", "workspace_member_credits_depleted":
		return "workspace out of credits"
	case "workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached":
		return "workspace usage limit reached"
	default:
		return strings.ReplaceAll(t, "_", " ")
	}
}

// formatReset renders a reset instant in foci's timezone: "02:09" when it
// is today, "Wed 4 Nov 10:07" otherwise.
func formatReset(unix int64, now time.Time) string {
	loc := timeutil.Location()
	t := time.Unix(unix, 0).In(loc)
	n := now.In(loc)
	if t.Year() == n.Year() && t.YearDay() == n.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

// resetPhrase is "at 02:09 (in 1h 40m)", or "" when unknown.
func resetPhrase(unix *int64, now time.Time) string {
	if unix == nil || *unix == 0 {
		return ""
	}
	return "at " + formatReset(*unix, now) + " (" + untilPhrase(now, time.Unix(*unix, 0)) + ")"
}

func untilPhrase(now, reset time.Time) string {
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

// --- backend wiring ---

// rateLimits is this backend's agent-wide rate-limit state.
func (b *Backend) rateLimits() *rateLimitState {
	return rateLimitsFor(b.startOpts.AgentID)
}

// onRateLimitsUpdated handles account/rateLimits/updated. It has no threadId,
// so it reaches the process owner; the notice goes to the owner's session
// (the gateway's rate-limit notice hook falls back to the configured
// [notify] target).
func (b *Backend) onRateLimitsUpdated(params json.RawMessage) {
	var p struct {
		RateLimits rateLimitSnapshot `json:"rateLimits"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		b.logWarnf("dropping malformed account/rateLimits/updated: %v", err)
		return
	}
	b.logDebugf("account/rateLimits/updated: %s", string(params))
	b.deliverRateLimitNotices(b.rateLimits().apply(p.RateLimits, timeutil.Now()))
}

// seedRateLimits reads the current allowance once at startup so the status
// block shows it before the first turn. Seeding never warns (see marks).
// Best-effort: a failure is logged, not fatal.
func (b *Backend) seedRateLimits() {
	raw, err := b.sendAndWait("account/rateLimits/read", nil)
	if err != nil {
		b.logDebugf("account/rateLimits/read failed: %v", err)
		return
	}
	var r struct {
		RateLimits          *rateLimitSnapshot           `json:"rateLimits"`
		RateLimitsByLimitID map[string]rateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		b.logWarnf("malformed account/rateLimits/read response: %v", err)
		return
	}
	st := b.rateLimits()
	now := timeutil.Now()
	var notices []string
	if len(r.RateLimitsByLimitID) > 0 {
		for id, sn := range r.RateLimitsByLimitID {
			if sn.LimitID == "" {
				sn.LimitID = id
			}
			notices = append(notices, st.apply(sn, now)...)
		}
	} else if r.RateLimits != nil {
		notices = st.apply(*r.RateLimits, now)
	}
	// Only a second process of an agent that already saw these buckets can
	// get notices here, and only for a real change since then.
	b.deliverRateLimitNotices(notices)
}

func (b *Backend) deliverRateLimitNotices(notices []string) {
	if len(notices) == 0 {
		return
	}
	b.mu.Lock()
	fn := b.onRateLimitNotice
	b.mu.Unlock()
	for _, n := range notices {
		b.logInfof("rate-limit notice: %s", n)
		if fn != nil && !b.startOpts.BatchOnly {
			fn(b.startOpts.SessionKey, n)
		}
	}
}
