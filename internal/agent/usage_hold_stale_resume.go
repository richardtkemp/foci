package agent

import (
	"time"

	"foci/internal/display"
)

// A stale resume at lift (#3155).
//
// Dick 2026-11-17: "Todo high probably shouldn't tell agents to resume what
// they were doing after usage limit reset if they've been inactive more then
// 12 hours", and the ruling: "Configurable time threshold, hot-reloadable.
// Instead agent should ask what to do".
//
// At lift, a chat session the hold interrupted runs its held input headed by
// the hidden resume line (releaseUsageHeld). When the session has been idle
// longer than usage_hold.resume_max_idle (usagehold.Registry.ResumeMaxIdle,
// default 12h, 0 = no limit), that hidden line becomes the ask line instead:
// the agent tells the user what it was doing and asks what to do. The idle
// clock is measured at lift and starts at the session's last activity: the
// interruption itself (the resume line's ReceivedAt, kept across a restart)
// or a user message held during the hold, whichever is later.

// usageResumeAskPrefix heads the ask line (tests match on it).
const usageResumeAskPrefix = "The usage hold interrupted your last turn in this conversation"

// usageResumeAskLine is the hidden input that replaces the resume line after
// idle of the given length.
func usageResumeAskLine(idle time.Duration) string {
	return usageResumeAskPrefix + ", and it lifted only now, after " + display.FormatDuration(idle) +
		" without activity. Do not resume that work on your own: tell the user briefly what you were doing when it stopped, and ask them what to do."
}

// usageResumeIdle reports how long the session of held (headed by its resume
// line) has been idle at now: since the latest of the interruption and the
// user messages held behind it. Automated input and injections are not
// activity.
func usageResumeIdle(held []Envelope, now time.Time) time.Duration {
	last := held[0].ReceivedAt
	for _, env := range held[1:] {
		if env.Automated || env.Inject != nil || env.ReceivedAt.IsZero() {
			continue
		}
		if env.ReceivedAt.After(last) {
			last = env.ReceivedAt
		}
	}
	if last.IsZero() {
		return 0
	}
	return now.Sub(last)
}
