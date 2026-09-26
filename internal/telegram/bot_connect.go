// Package telegram — bot_connect.go: resilient bot construction.
//
// gotgbot.NewBot calls getMe synchronously to fetch the bot's identity (id +
// username). If DNS or the network isn't ready at startup, that call fails and
// the bot is permanently disabled — no retry, no recovery.
//
// connectBot wraps gotgbot.NewBot in netretry.Do, the startup-connect retry
// shared with Discord's gateway open (#1954): unbounded exponential backoff
// on transient errors, fail fast on auth/token errors. See the netretry
// package doc for why "unbounded" and for the 2026-05-20 incident (#796:
// foci restarted with DNS not yet up, all six bots failed getMe with "server
// misbehaving", 27.5h silence until manual restart).

package telegram

import (
	"context"
	"strings"

	"foci/internal/log"
	"foci/internal/netretry"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// botFactory is the function used to build the underlying gotgbot.Bot.
// It's a package-level var so tests can substitute a deterministic fake.
var botFactory = gotgbot.NewBot

// defaultConnectBackoff is the schedule connectBot uses in production.
var defaultConnectBackoff = netretry.StartupBackoff

// connectBot calls botFactory with exponential backoff. Transient errors
// are retried (forever, if MaxAttempts == 0); permanent (auth/token) errors
// fail fast. All log lines have the bot token redacted before emit.
func connectBot(token string, opts *gotgbot.BotOpts, lg *log.ComponentLogger, bo netretry.Backoff) (*gotgbot.Bot, error) {
	var bot *gotgbot.Bot
	err := netretry.Do(context.Background(), netretry.Policy{
		Name:      "create telegram bot",
		Backoff:   bo,
		Permanent: isPermanentTelegramErr,
		Redact:    func(s string) string { return redactToken(s, token) },
		Log:       lg,
	}, func() error {
		var err error
		bot, err = botFactory(token, opts)
		return err
	})
	if err != nil {
		return nil, err
	}
	return bot, nil
}

// isPermanentTelegramErr returns true for errors that won't get better by
// retrying — the shared auth markers plus Telegram's own.
var isPermanentTelegramErr = netretry.PermanentMarkers(
	"bot was blocked", // user-side, not relevant to startup but harmless
)

// redactToken replaces the bot token in a string with "[REDACTED]" so it
// doesn't leak into logs. Shared with Bot.sanitizeError.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}
