package ccstream

import (
	"fmt"
	"strings"
)

// looksLikeRateLimit reports whether a CC result text is a rate / session /
// usage limit notice. CC serves these as a *synthetic* result (an assistant
// message minted without an API call), e.g.:
//
//	"You've hit your session limit · resets 10:30pm (Europe/London)"
//
// We key on the "…limit … reset…" phrasing rather than the model field:
// <synthetic> is CC's generic no-API-call sentinel used for many benign
// no-ops, so it does not discriminate. The other synthetic CC emits ("There's
// an issue with the selected model (<synthetic>)…") has neither "limit" nor
// "reset", so it does not match; ordinary replies do not either. See #1211.
func looksLikeRateLimit(s string) bool {
	ls := strings.ToLower(s)
	if !strings.Contains(ls, "reset") {
		return false
	}
	return strings.Contains(ls, "session limit") ||
		strings.Contains(ls, "usage limit") ||
		strings.Contains(ls, "rate limit")
}

// fireRateLimited invokes the rate-limit hook if one is registered. Safe to
// call whether or not a hook is set.
func (b *Backend) fireRateLimited(detail string) {
	if b.onRateLimited != nil {
		b.onRateLimited(detail)
	}
}

// rateLimitEventSummary renders a CC structured rate_limit_event for logging.
// We have never observed one of these in the wild (rate/session limits have
// only ever surfaced as a synthetic result — see looksLikeRateLimit), so
// OnRateLimit logs this at WARN purely so we learn IF the event fires and can
// wire it into the gate properly later. See #1211.
func rateLimitEventSummary(ev *RateLimitEvent) string {
	if ev == nil {
		return "rate_limit_event=<nil>"
	}
	info := ev.RateLimitInfo
	resetsAt := "nil"
	if info.ResetsAt != nil {
		resetsAt = fmt.Sprintf("%.0f", *info.ResetsAt)
	}
	util := "nil"
	if info.Utilization != nil {
		util = fmt.Sprintf("%.2f", *info.Utilization)
	}
	return fmt.Sprintf("status=%q type=%q resetsAt=%s util=%s overage=%q",
		info.Status, info.RateLimitType, resetsAt, util, info.OverageStatus)
}
