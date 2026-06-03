package telegram

import (
	"context"
	"errors"
	"runtime/debug"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// RegisterCommands registers the bot's slash commands with Telegram via setMyCommands.
// This makes commands appear as autocomplete suggestions when the user types "/" in chat.
// Logs a warning on failure but does not return an error.
func (b *Bot) RegisterCommands() {
	var cmds []gotgbot.BotCommand

	// Add all registered commands from the registry (skip hidden)
	for _, cmd := range b.commands.All() {
		if cmd.Hidden {
			continue
		}
		desc := cmd.Description
		if desc == "" {
			desc = cmd.Name
		}
		cmds = append(cmds, gotgbot.BotCommand{
			Command:     cmd.Name,
			Description: desc,
		})
	}

	if _, err := b.client.SetMyCommands(cmds, nil); err != nil {
		b.logger().Warnf("setMyCommands: %s", b.sanitizeError(err))
		return
	}
	b.logger().Infof("registered %d commands with BotFather", len(cmds))
}

// Start spawns the bot's Run loop as a goroutine.
// Non-blocking; use Stop() to wait for shutdown.
func (b *Bot) Start(ctx context.Context) error {
	go b.Run(ctx)
	return nil
}

// Stop is a no-op for telegram.Bot (shutdown is ctx-cancellation based).
func (b *Bot) Stop() error {
	return nil
}

// Run starts the receiver and agent worker goroutines. Blocks until ctx is cancelled.
// If polling fails, it recovers and retries with backoff.
func (b *Bot) Run(ctx context.Context) {
	b.logger().Infof("bot started as @%s", b.api.Username)

	b.RegisterCommands()

	// Agent message pump — drains the platform queue and hands each message
	// to the agent's per-session inbox, where per-session workers handle
	// batching, in-flight tracking, and turn execution via Bot.Drive.
	go b.agentMessagePump(ctx)

	// Command worker — processes slash commands concurrently with agent turns,
	// so /status etc. respond immediately instead of queueing behind tool calls.
	go b.commandWorker(ctx)

	for ctx.Err() == nil {
		b.pollUpdates(ctx)

		if ctx.Err() != nil {
			return
		}

		b.logger().Warnf("polling interrupted, restarting in 5s...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// pollUpdates runs the telegram update polling loop. Returns if a panic
// occurs or ctx is cancelled. Caller should retry on return.
func (b *Bot) pollUpdates(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger().Errorf("panic in polling: %v\n%s", r, debug.Stack())
		}
	}()

	type updateResult struct {
		updates []gotgbot.Update
		err     error
	}

	var offset int64
	var consecutiveErrors int
	const errorEscalateThreshold = 5 // log one ERROR when failures reach this count; DEBUG before and after
	// Error backoff: exponential from baseBackoff, doubling per consecutive
	// failure, capped at maxBackoff, reset on first success. This stops a
	// fast-failing error (e.g. a 502 that returns immediately instead of
	// holding the long-poll open) from collapsing the poll interval into a
	// tight retry loop that hammers an already-struggling server.
	const baseBackoff = 1 * time.Second
	const maxBackoff = 30 * time.Second

	// On exit, acknowledge processed updates so they aren't replayed on restart.
	// Telegram acknowledges updates implicitly when the next getUpdates has a
	// higher offset, so we must fire one final short-poll before returning.
	defer func() {
		if offset > 0 {
			_, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:         offset,
				Timeout:        0,
				AllowedUpdates: []string{"message", "callback_query"},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 5 * time.Second,
				},
			})
			if err != nil {
				b.logger().Errorf("failed to ack updates on shutdown: %s", b.sanitizeError(err))
			} else {
				b.logger().Infof("acknowledged updates up to offset %d", offset)
			}
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}

		// Resolve poll timeouts: HTTP client timeout from config (default 30s);
		// Telegram-side long-poll is derived as client-5s (floored at 0),
		// preserving the network roundtrip buffer. A tg-side timeout of 0
		// is a short-poll (Telegram returns immediately), which is what
		// integration tests want against the httptest stub.
		clientTimeout := b.longPollTimeout
		if clientTimeout <= 0 {
			clientTimeout = 30 * time.Second
		}
		tgTimeoutSeconds := int64((clientTimeout - 5*time.Second) / time.Second)
		if tgTimeoutSeconds < 0 {
			tgTimeoutSeconds = 0
		}

		// Poll in a goroutine so we can select on ctx.Done()
		ch := make(chan updateResult, 1)
		go func() {
			updates, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:         offset,
				Timeout:        tgTimeoutSeconds,
				AllowedUpdates: []string{"message", "callback_query"},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: clientTimeout, // must exceed Telegram's long-poll timeout
				},
			})
			ch <- updateResult{updates, err}
		}()

		select {
		case <-ctx.Done():
			return
		case res := <-ch:
			if res.err != nil {
				consecutiveErrors++
				sanitized := b.sanitizeError(res.err)
				// Log ERROR exactly once — on the poll that crosses the
				// escalation threshold. Every failure before and after that
				// stays at DEBUG, so a sustained outage produces a single
				// ERROR per bot rather than one per poll (a 14-min outage was
				// emitting ~16 ERROR lines per bot). Recovery is logged once
				// on the next success (see below).
				if consecutiveErrors == errorEscalateThreshold {
					b.logger().Errorf("get updates failing (%d consecutive failures; further failures logged at debug until recovery): %s", consecutiveErrors, sanitized)
				} else {
					b.logger().Debugf("get updates (failure #%d): %s", consecutiveErrors, sanitized)
				}

				// Exponential backoff, capped.
				wait := baseBackoff
				for i := 1; i < consecutiveErrors && wait < maxBackoff; i++ {
					wait *= 2
				}
				if wait > maxBackoff {
					wait = maxBackoff
				}
				// Honour Telegram's flood-control Retry-After (429): never retry
				// sooner than instructed. Pad by 1s for clock/roundtrip slack.
				var tgErr *gotgbot.TelegramError
				if errors.As(res.err, &tgErr) && tgErr.ResponseParams != nil && tgErr.ResponseParams.RetryAfter > 0 {
					if ra := time.Duration(tgErr.ResponseParams.RetryAfter)*time.Second + time.Second; ra > wait {
						wait = ra
					}
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				continue
			}
			// Recovery: if we had escalated to ERROR, log a single INFO line
			// so the log shows the outage ended (paired with the one ERROR).
			if consecutiveErrors >= errorEscalateThreshold {
				b.logger().Infof("get updates recovered after %d consecutive failures", consecutiveErrors)
			}
			consecutiveErrors = 0

			for _, update := range res.updates {
				if update.UpdateId >= offset {
					offset = update.UpdateId + 1
				}
				if update.CallbackQuery != nil {
					b.handleCallbackQuery(ctx, update.CallbackQuery)
					continue
				}
				if update.Message == nil {
					continue
				}
				b.receiveMessage(ctx, update.Message)
			}
		}
	}
}
