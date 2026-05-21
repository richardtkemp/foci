// Package telegram — bot_connect.go: resilient bot construction.
//
// gotgbot.NewBot calls getMe synchronously to fetch the bot's identity (id +
// username). If DNS or the network isn't ready at startup, that call fails and
// the bot is permanently disabled — no retry, no recovery.
//
// This file wraps gotgbot.NewBot in a bounded retry loop with exponential
// backoff so a transient blip at boot doesn't take an agent offline for hours
// (see TODO #796 / the 2026-05-20 incident: foci restarted at 06:34 with DNS
// not yet up, all six bots failed getMe with "server misbehaving", 27.5h
// silence until manual restart).
//
// The retry classifier is intentionally permissive: only definitively
// permanent errors (auth/token problems) bail immediately; everything else
// (DNS, i/o timeout, connection refused, generic context deadlines) is
// treated as transient.

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

// connectBackoff describes the bounded retry schedule used by connectBot.
// MaxAttempts includes the initial try (attempt 1 has zero delay).
type connectBackoff struct {
	MaxAttempts int
	Delays      []time.Duration // index 0 unused; Delays[i] is the wait BEFORE attempt i+1
}

// defaultConnectBackoff is ~112s total over 6 attempts.
//
// Rationale: a typical DNS-not-ready window at boot resolves in <30s once
// network-online.target fires; ~2 minutes per agent is plenty without
// blocking startup intolerably. Six telegram agents serialised at worst case
// adds ~12 minutes to startup, which is acceptable for the once-in-a-blue-
// moon "DNS truly broken" scenario; the common case is that attempt 1 or 2
// succeeds and startup feels normal.
var defaultConnectBackoff = connectBackoff{
	MaxAttempts: 6,
	Delays: []time.Duration{
		0,               // attempt 1 (unused)
		2 * time.Second, // before attempt 2
		5 * time.Second, // before attempt 3
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
	},
}

// connectBot calls botFactory with bounded exponential backoff. Transient
// errors are retried; permanent (auth/token) errors fail fast. All log lines
// have the bot token redacted before emit.
func connectBot(token string, opts *gotgbot.BotOpts, lg *log.ComponentLogger, bo connectBackoff) (*gotgbot.Bot, error) {
	if bo.MaxAttempts <= 0 {
		bo.MaxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= bo.MaxAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(0)
			if attempt-1 < len(bo.Delays) {
				delay = bo.Delays[attempt-1]
			} else if len(bo.Delays) > 0 {
				delay = bo.Delays[len(bo.Delays)-1]
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}

		bot, err := botFactory(token, opts)
		if err == nil {
			if attempt > 1 && lg != nil {
				lg.Infof("connected on attempt %d/%d", attempt, bo.MaxAttempts)
			}
			return bot, nil
		}
		lastErr = err

		if isPermanentTelegramErr(err) {
			// Don't burn budget on a wrong token / disabled bot.
			return nil, fmt.Errorf("create telegram bot: %s", redactToken(err.Error(), token))
		}

		if lg != nil {
			lg.Warnf("attempt %d/%d failed (transient): %s", attempt, bo.MaxAttempts, redactToken(err.Error(), token))
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
	"Unauthorized",       // gotgbot returns this for 401
	"unauthorized",       // case variant from some transports
	"401",                // raw status
	"bot was blocked",    // user-side, not relevant to startup but harmless
	"Forbidden",          // 403 — token revoked / bot kicked
	"invalid token",      // generic
	"token is invalid",   // generic
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
