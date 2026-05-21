// Package telegram — bot_connect.go: resilient bot construction.
//
// gotgbot.NewBot calls getMe synchronously to fetch the bot's identity (id +
// username). If DNS or the network isn't ready at startup, that call fails and
// the bot is permanently disabled — no retry, no recovery.
//
// This file wraps gotgbot.NewBot in an unbounded exponential-backoff retry
// loop so a transient blip at boot doesn't take an agent offline for hours
// (see TODO #796 / the 2026-05-20 incident: foci restarted at 06:34 with DNS
// not yet up, all six bots failed getMe with "server misbehaving", 27.5h
// silence until manual restart).
//
// "Unbounded" means: keep retrying as long as the error looks transient. If
// Telegram is genuinely unreachable, foci sits on startup retrying — that's
// loud (operator can see foci hasn't come up) and recoverable (DNS returns →
// next retry succeeds → startup completes). The "agent runs without
// platform" failure mode that caused the outage is eliminated.
//
// Permanent errors (auth/token problems) still bail immediately — no point
// retrying a 401 forever.

package telegram

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/log"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// botFactory is the function used to build the underlying gotgbot.Bot.
// It's a package-level var so tests can substitute a deterministic fake.
var botFactory = gotgbot.NewBot

// connectBackoff describes the retry schedule used by connectBot.
//
// Schedule: InitialDelay grows by Multiplier each attempt, capped at MaxDelay.
// MaxAttempts == 0 means unbounded (retry until success or permanent error).
type connectBackoff struct {
	MaxAttempts  int           // 0 = unbounded
	InitialDelay time.Duration // delay before attempt 2
	MaxDelay     time.Duration // cap on per-attempt delay
	Multiplier   float64       // growth factor per attempt
}

// defaultConnectBackoff retries forever with exponential backoff, capping
// the inter-attempt delay at 5 minutes.
//
// Schedule: 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 300s, 300s, …
// After ~9 attempts (~8 minutes) we settle to a 5-minute heartbeat. In
// practice a startup DNS hiccup resolves in the first few attempts; the
// long tail exists so an extended outage eventually recovers without
// requiring a manual restart.
var defaultConnectBackoff = connectBackoff{
	MaxAttempts:  0, // unbounded
	InitialDelay: 2 * time.Second,
	MaxDelay:     5 * time.Minute,
	Multiplier:   2.0,
}

// nextDelay returns the delay to apply BEFORE the given (1-based) attempt.
// Attempt 1 has no delay (it's the first try). Attempt N's delay is the
// initial doubled (N-2) times, capped at MaxDelay.
func (bo connectBackoff) nextDelay(attempt int) time.Duration {
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

// connectBot calls botFactory with exponential backoff. Transient errors
// are retried (forever, if MaxAttempts == 0); permanent (auth/token) errors
// fail fast. All log lines have the bot token redacted before emit.
func connectBot(token string, opts *gotgbot.BotOpts, lg *log.ComponentLogger, bo connectBackoff) (*gotgbot.Bot, error) {
	var lastErr error
	for attempt := 1; ; attempt++ {
		if delay := bo.nextDelay(attempt); delay > 0 {
			time.Sleep(delay)
		}

		bot, err := botFactory(token, opts)
		if err == nil {
			if attempt > 1 && lg != nil {
				lg.Infof("connected on attempt %d", attempt)
			}
			return bot, nil
		}
		lastErr = err

		if isPermanentTelegramErr(err) {
			// Don't burn cycles on a wrong token / disabled bot.
			return nil, fmt.Errorf("create telegram bot: %s", redactToken(err.Error(), token))
		}

		if lg != nil {
			lg.Warnf("attempt %d failed (transient, will retry): %s", attempt, redactToken(err.Error(), token))
		}

		if bo.MaxAttempts > 0 && attempt >= bo.MaxAttempts {
			break
		}
	}
	final := "unknown error"
	if lastErr != nil {
		final = redactToken(lastErr.Error(), token)
	}
	return nil, fmt.Errorf("create telegram bot: gave up after %d attempts: %s", bo.MaxAttempts, final)
}

// isPermanentTelegramErr returns true for errors that won't get better by
// retrying — primarily auth failures (wrong token, bot deleted/disabled).
// Everything else (DNS, timeouts, transport, 5xx) is treated as transient.
//
// We match on the error message because gotgbot wraps transport errors in
// *url.Error / *net.OpError, and an auth failure can surface either as a
// bare error or wrapped inside a url.Error — the marker string is the
// reliable signal regardless of how the transport layered the error.
func isPermanentTelegramErr(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(err.Error(), permanentMarkers)
}

// permanentMarkers are substrings of error messages that indicate the
// problem isn't going to resolve via retry. We match conservatively — when
// in doubt, retry.
var permanentMarkers = []string{
	"Unauthorized",     // gotgbot returns this for 401
	"unauthorized",     // case variant from some transports
	"401",              // raw status
	"bot was blocked",  // user-side, not relevant to startup but harmless
	"Forbidden",        // 403 — token revoked / bot kicked
	"invalid token",    // generic
	"token is invalid", // generic
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// redactToken replaces the bot token in a string with "[REDACTED]" so it
// doesn't leak into logs. Shared with Bot.sanitizeError.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}
