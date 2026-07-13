package ccstream

import (
	"fmt"
)

// fireRateLimited invokes the rate-limit warning hook if one is registered.
// Safe to call whether or not a hook is set.
func (b *Backend) fireRateLimited(detail string) {
	if b.onRateLimited != nil {
		b.onRateLimited(detail)
	}
}

// rateLimitEventSummary renders a CC structured rate_limit_event for a warning.
// CC emits the event on status transitions with the API's utilization; the
// summary carries the fields worth surfacing to a human.
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
