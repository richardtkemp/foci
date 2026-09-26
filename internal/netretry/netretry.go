// Package netretry retries a network connect made at startup with exponential
// backoff, splitting transient failures (retry) from permanent ones (fail fast).
//
// It exists because a platform connect that fails once at boot otherwise
// disables that platform until the next restart. foci routinely comes up before
// systemd-resolved is answering, so the first attempt fails with
// "lookup ... server misbehaving" and nothing tries again: #796 (Telegram,
// 27.5h of silence) and #1954 (Discord, the same boot window on the same host).
// Every platform's startup connect goes through Do so the next one inherits
// the fix instead of rediscovering the bug.
package netretry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/log"
)

// Backoff describes a retry schedule.
//
// The delay before attempt 2 is InitialDelay; each later attempt multiplies it
// by Multiplier, capped at MaxDelay. MaxAttempts == 0 means unbounded (retry
// until success, a permanent error, or ctx cancellation).
type Backoff struct {
	MaxAttempts  int           // 0 = unbounded
	InitialDelay time.Duration // delay before attempt 2
	MaxDelay     time.Duration // cap on per-attempt delay (0 = no cap)
	Multiplier   float64       // growth factor per attempt
}

// StartupBackoff is the schedule for a platform's boot-time connect: retry
// forever, 2s, 4s, 8s … capped at a 5-minute heartbeat after ~8 minutes.
//
// Unbounded on purpose: a startup DNS hiccup clears in the first few attempts,
// and the long tail means an extended outage still recovers without a manual
// restart. "Agent runs without platform" is the failure mode being removed,
// so giving up after N attempts would just reintroduce it on a longer fuse.
var StartupBackoff = Backoff{
	MaxAttempts:  0,
	InitialDelay: 2 * time.Second,
	MaxDelay:     5 * time.Minute,
	Multiplier:   2.0,
}

// NextDelay returns the delay to apply BEFORE the given (1-based) attempt.
// Attempt 1 has no delay. Attempt N's delay is InitialDelay grown (N-2)
// times, capped at MaxDelay.
func (bo Backoff) NextDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := bo.InitialDelay
	if d <= 0 {
		d = time.Second
	}
	mult := bo.Multiplier
	if mult < 1 {
		mult = 1
	}
	for i := 2; i < attempt; i++ {
		d = time.Duration(float64(d) * mult)
		if bo.MaxDelay > 0 && d >= bo.MaxDelay {
			return bo.MaxDelay
		}
	}
	if bo.MaxDelay > 0 && d > bo.MaxDelay {
		return bo.MaxDelay
	}
	return d
}

// Policy configures one Do call.
type Policy struct {
	// Name prefixes every returned error, e.g. "create telegram bot".
	Name string
	// Backoff is the retry schedule.
	Backoff Backoff
	// Permanent reports errors that retrying cannot fix (bad token, revoked
	// bot). nil treats every error as transient.
	Permanent func(error) bool
	// Redact scrubs secrets from error text before it is logged or returned.
	// nil leaves the text unchanged.
	Redact func(string) string
	// Log receives the per-attempt warnings and the recovery line. May be nil.
	Log *log.ComponentLogger
}

// Do calls fn until it succeeds, returns a permanent error, exhausts
// p.Backoff.MaxAttempts, or ctx is cancelled while waiting between attempts.
// Returned errors carry redacted text only (never the wrapped original), so a
// secret embedded in a transport error cannot leak through %w.
func Do(ctx context.Context, p Policy, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	redact := p.Redact
	if redact == nil {
		redact = func(s string) string { return s }
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		if delay := p.Backoff.NextDelay(attempt); delay > 0 {
			if err := sleep(ctx, delay); err != nil {
				return fmt.Errorf("%s: %w after %d attempt(s): %s", p.Name, err, attempt-1, redact(lastErr.Error()))
			}
		}
		// A timer that fired in the same instant as a cancel can win sleep's
		// select; re-check so no attempt ever starts after cancellation.
		if attempt > 1 && ctx.Err() != nil {
			return fmt.Errorf("%s: %w after %d attempt(s): %s", p.Name, ctx.Err(), attempt-1, redact(lastErr.Error()))
		}

		err := fn()
		if err == nil {
			if attempt > 1 && p.Log != nil {
				p.Log.Infof("connected on attempt %d", attempt)
			}
			return nil
		}
		lastErr = err

		if p.Permanent != nil && p.Permanent(err) {
			return fmt.Errorf("%s: %s", p.Name, redact(err.Error()))
		}

		if p.Log != nil {
			p.Log.Warnf("attempt %d failed (transient, will retry): %s", attempt, redact(err.Error()))
		}

		if p.Backoff.MaxAttempts > 0 && attempt >= p.Backoff.MaxAttempts {
			break
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %s", p.Name, p.Backoff.MaxAttempts, redact(lastErr.Error()))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AuthMarkers are substrings of HTTP auth failures shared by every bot API
// foci talks to: the token is wrong or the bot was revoked, and no amount of
// retrying fixes that. Matched on the message because transports wrap the
// status in *url.Error / *net.OpError inconsistently; the marker text is the
// reliable signal however the error was layered.
var AuthMarkers = []string{
	"Unauthorized",     // 401
	"unauthorized",     // case variant from some transports
	"401",              // raw status
	"Forbidden",        // 403 — token revoked / bot kicked
	"invalid token",    // generic
	"token is invalid", // generic
}

// PermanentMarkers returns a Policy.Permanent classifier that treats an error
// as permanent when its message contains any of AuthMarkers or extra.
// Everything else (DNS, timeouts, refused connections, 5xx) is transient —
// the match is deliberately conservative: when in doubt, retry.
func PermanentMarkers(extra ...string) func(error) bool {
	markers := append(append([]string{}, AuthMarkers...), extra...)
	return func(err error) bool {
		if err == nil {
			return false
		}
		msg := err.Error()
		for _, m := range markers {
			if strings.Contains(msg, m) {
				return true
			}
		}
		return false
	}
}
