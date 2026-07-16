package opencode

import (
	"context"
	"regexp"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/timeutil"
)

const rateLimitTurnEnd = "rate limit"

var rateLimitResetRe = regexp.MustCompile(`(?i)\breset(?:s)?(?:\s+at)?\s+(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})(Z|[+-]\d{2}:?\d{2})?`)

// parseRateLimitRetry identifies OpenCode retry statuses caused by an account
// usage/rate limit and extracts the advertised reset time. OpenCode currently
// reports a timezone-less wall clock (for example, "reset at
// 2026-07-16 19:13:59"). That value cannot safely be interpreted in foci's
// timezone, so only timestamps carrying an explicit UTC offset are trusted.
// A recognised limit without an unambiguous reset uses the rate-limit system's
// conservative one-hour fallback rather than allowing OpenCode to retry
// indefinitely or claiming a false local reset time.
func parseRateLimitRetry(message string, now time.Time) (time.Time, bool) {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "usage limit") &&
		!strings.Contains(lower, "rate limit") &&
		!strings.Contains(lower, "rate-limit") &&
		!strings.Contains(lower, "rate limited") {
		return time.Time{}, false
	}

	if match := rateLimitResetRe.FindStringSubmatch(message); match != nil && match[2] != "" {
		zone := match[2]
		if len(zone) == 5 && zone != "Z" {
			zone = zone[:3] + ":" + zone[3:]
		}
		if reset, err := time.Parse("2006-01-02 15:04:05Z07:00", match[1]+zone); err == nil && reset.After(now) {
			return reset, true
		}
	}
	return now.Add(time.Hour), true
}

// handleRateLimitRetry engages foci's shared rate-limit system, asks OpenCode
// to stop its exponential retry loop, and then completes the waiting turn.
// Abort is sent before completion so a new user turn cannot begin and consume
// the old turn's delayed MessageAbortedError/session.idle events.
func (b *Backend) handleRateLimitRetry(status SessionStatus) bool {
	until, limited := parseRateLimitRetry(status.Message, timeutil.Now())
	if !limited {
		return false
	}
	if !b.IsTurnInFlight() {
		log.NewComponentLogger(b.logComponent()).Debugf("rate limit retry ignored without an active turn: %s", status.Message)
		return true
	}

	log.NewComponentLogger(b.logComponent()).Warnf("rate limited until %s; aborting OpenCode turn", until.Format(time.RFC3339))
	if b.onRateLimited != nil {
		b.onRateLimited(until)
	}
	if err := b.Interrupt(context.Background()); err != nil {
		log.NewComponentLogger(b.logComponent()).Warnf("rate limit abort failed: %v", err)
	}
	b.failInFlightTurn(rateLimitTurnEnd)
	return true
}
