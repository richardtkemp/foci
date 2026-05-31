package telegram

import (
	"context"
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
	const errorEscalateThreshold = 5 // escalate to ERROR after this many consecutive failures

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

		// Poll in a goroutine so we can select on ctx.Done()
		ch := make(chan updateResult, 1)
		go func() {
			updates, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:         offset,
				Timeout:        25,
				AllowedUpdates: []string{"message", "callback_query"},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 30 * time.Second, // must exceed Telegram's long-poll timeout
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
				if consecutiveErrors >= errorEscalateThreshold {
					b.logger().Errorf("get updates (%d consecutive failures): %s", consecutiveErrors, sanitized)
				} else {
					b.logger().Debugf("get updates (transient): %s", sanitized)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
				continue
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
