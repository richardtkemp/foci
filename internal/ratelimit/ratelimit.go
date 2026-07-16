// Package ratelimit defines backend-neutral rate-limit signals and the shared
// policy for turning their reset hints into an absolute gate deadline.
package ratelimit

import "time"

// Kind distinguishes a short-lived request throttle from an account usage
// window. The distinction matters only when the source supplies no trustworthy
// reset hint.
type Kind string

const (
	KindRequest Kind = "request"
	KindUsage   Kind = "usage"
)

const (
	requestFallbackInitial = time.Minute
	requestFallbackMax     = time.Hour
	usageFallback          = time.Hour
)

// Signal is the neutral rate-limit report emitted by API and delegated
// backends. ResetAt and RetryAfter are optional trustworthy hints; Detail is
// diagnostic source text and is never interpreted by the shared policy.
type Signal struct {
	Kind       Kind
	ResetAt    time.Time
	RetryAfter time.Duration
	Detail     string
}

// Resolution is the result of applying shared fallback policy to a Signal.
// MissingHintStreak is carried by the endpoint gate and only advances for
// request throttles without a reset hint.
type Resolution struct {
	Until             time.Time
	MissingHintStreak int
}

// Resolve converts a backend-neutral signal into an absolute deadline.
// Trustworthy absolute/duration hints always win and reset request backoff.
// Missing usage-window hints use a conservative one-hour gate. Missing request
// hints back off 1m, 2m, 4m, ... capped at 1h.
func Resolve(now time.Time, signal Signal, missingHintStreak int) Resolution {
	if signal.ResetAt.After(now) {
		return Resolution{Until: signal.ResetAt}
	}
	if signal.RetryAfter > 0 {
		return Resolution{Until: now.Add(signal.RetryAfter)}
	}
	if signal.Kind == KindUsage {
		return Resolution{Until: now.Add(usageFallback)}
	}

	delay := requestFallbackInitial
	for range missingHintStreak {
		delay *= 2
		if delay >= requestFallbackMax {
			delay = requestFallbackMax
			break
		}
	}
	return Resolution{
		Until:             now.Add(delay),
		MissingHintStreak: missingHintStreak + 1,
	}
}
