package telegram

import (
	"context"
	"errors"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// ipv4SwitchThreshold is how many consecutive getUpdates *timeouts* (while
// still dialing dual-stack) trigger the one-way switch to IPv4-only. Kept
// small so we self-heal the IPv6 blackhole (TODO #809) within ~a minute or two
// rather than the 34-minute outage observed on 2026-06-07, but >1 so a single
// stray timeout (which would recover on its own) doesn't permanently abandon
// IPv6. Each timeout costs roughly one long-poll interval plus backoff.
const ipv4SwitchThreshold = 3

// errorEscalateThreshold is the consecutive-failure count at which the poll
// loop logs a single ERROR; failures before and after it stay at DEBUG until
// recovery (so a sustained outage is one ERROR per bot, not one per poll).
const errorEscalateThreshold = 5

// maxIPv4Reverts bounds how many times the bot will revert IPv4→dual-stack to
// retry IPv6 after a switch. The 2026-06-07 blackhole (#809) self-healed in
// ~34 min, so the v6 path is worth retrying once polling recovers — but a
// genuinely persistent blackhole would otherwise flap forever (revert → re-
// blackhole → re-switch). After this many flap cycles with no healthy
// dual-stack poll in between, we latch IPv4 for the process. A successful
// dual-stack poll restores the full budget, so unrelated future incidents
// aren't starved by earlier flapping. Per-pollUpdates-invocation.
const maxIPv4Reverts = 3

// isTimeoutErr reports whether a getUpdates error is a timeout — the signature
// of the IPv6 read-stall (TODO #809). Covers both the http.Client deadline
// ("context deadline exceeded") and the underlying socket read timeout
// ("read: connection timed out" / "i/o timeout"), however the transport
// layered them. Non-timeout failures (4xx/5xx, conn refused, DNS) are NOT
// treated as the blackhole signature.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "timed out") ||
		strings.Contains(s, "i/o timeout")
}

// pollErrorAction is how the poll loop should react to a getUpdates failure.
type pollErrorAction struct {
	switchToIPv4 bool // latch the transport to IPv4-only (the silent self-heal)
	escalate     bool // log this failure at ERROR (crossed escalation threshold)
}

// classifyPollError decides the reaction to a getUpdates failure.
//
//   - onIPv4: whether the bot has already latched to IPv4-only.
//   - isTimeout: whether the error is a timeout (the IPv6-blackhole signature).
//   - consecutiveTimeouts: run of consecutive *timeout* failures (reset by any
//     non-timeout error) — the actual blackhole signature.
//   - consecutiveErrors: run of consecutive failures of any kind, for escalation.
//
// The switch gates on consecutiveTimeouts, NOT consecutiveErrors: the
// 2026-06-07 blackhole (#809) was an unbroken run of read timeouts, whereas a
// mix of 429/502/timeout is ordinary Telegram-side noise that IPv4 can't fix.
// While still dual-stack, a sustained run of timeouts is the suspected IPv6
// blackhole: we switch to IPv4 and stay quiet — the switch IS the recovery
// (Dick's rule: "the switch-to-4 process does not error or warn"). Once on
// IPv4 — or for any non-timeout-dominated failure run — normal escalation
// applies, so an ERROR fires only if failures persist ("only error or warn if
// we still get failures on 4").
func classifyPollError(onIPv4, isTimeout bool, consecutiveTimeouts, consecutiveErrors int) pollErrorAction {
	if !onIPv4 && isTimeout && consecutiveTimeouts >= ipv4SwitchThreshold {
		return pollErrorAction{switchToIPv4: true}
	}
	return pollErrorAction{escalate: consecutiveErrors == errorEscalateThreshold}
}

// switchToIPv4 latches the bot's transport to IPv4-only and drops pooled IPv6
// sockets so the next dial takes the IPv4 path. Idempotent and safe on
// test-constructed bots (nil forceIPv4). Logs at INFO — not WARN/ERROR —
// because the switch is a successful self-heal, not a fault. The poll loop may
// later revert to dual-stack on recovery (see revertToDualStack).
func (b *Bot) switchToIPv4(afterTimeouts int) {
	if b.forceIPv4 == nil {
		return // test-constructed bot without transport wiring
	}
	if b.forceIPv4.Swap(true) {
		return // already latched
	}
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	b.logger().Infof("switched Telegram API to IPv4-only after %d consecutive IPv6 read timeouts (suspected IPv6 blackhole to api.telegram.org — TODO #809); will retry dual-stack on recovery", afterTimeouts)
}

// revertToDualStack clears the IPv4-only latch so the next dial re-races
// dual-stack (v6-preferred), and drops pooled IPv4 sockets so the next dial
// actually re-resolves. The counterpart to switchToIPv4: the 2026-06-07
// blackhole (#809) self-healed in ~34 min, so the v6 path is worth retrying
// once polling recovers on IPv4. Bounded by maxIPv4Reverts in the caller so a
// persistent blackhole eventually latches IPv4 for good rather than flapping.
// Idempotent and nil-safe on test-constructed bots.
func (b *Bot) revertToDualStack(revertCount int) {
	if b.forceIPv4 == nil {
		return // test-constructed bot without transport wiring
	}
	if !b.forceIPv4.Swap(false) {
		return // already dual-stack
	}
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	b.logger().Infof("reverted Telegram API to dual-stack after recovery (revert %d/%d; retrying IPv6 — TODO #809)", revertCount, maxIPv4Reverts)
}

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
	// consecutiveTimeouts tracks the run of *timeout* failures (reset by any
	// non-timeout error). This — not consecutiveErrors — gates the IPv4 switch,
	// so a mix of 429/502/timeout (ordinary Telegram-side noise) can't trip it.
	var consecutiveTimeouts int
	// IPv4 revert accounting (see maxIPv4Reverts). ipv4Reverts counts flap
	// cycles since the last healthy dual-stack poll; ipv4Permanent latches once
	// the budget is spent so we stop retrying IPv6 for this process.
	var ipv4Reverts int
	var ipv4Permanent bool
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
				timeout := isTimeoutErr(res.err)
				if timeout {
					consecutiveTimeouts++
				} else {
					consecutiveTimeouts = 0
				}
				sanitized := b.sanitizeError(res.err)
				// Decide how to react. While dual-stack, a run of *timeouts* is
				// the suspected IPv6 blackhole (TODO #809): switch to IPv4-only
				// silently — the switch is the self-heal. Otherwise (already on
				// IPv4, or a non-timeout-dominated run) escalate to ERROR exactly
				// once, on the poll that crosses the threshold; every failure
				// before and after stays at DEBUG so a sustained outage produces
				// a single ERROR per bot rather than one per poll. Recovery is
				// logged once on the next success (see below).
				action := classifyPollError(b.ipv4Latched(), timeout, consecutiveTimeouts, consecutiveErrors)
				switch {
				case action.switchToIPv4:
					b.switchToIPv4(consecutiveTimeouts)
					// Restart escalation accounting on the new IPv4 path, so an
					// ERROR fires only if IPv4 *also* racks up failures.
					consecutiveErrors = 0
					consecutiveTimeouts = 0
				case action.escalate:
					b.logger().Errorf("get updates failing (%d consecutive failures; further failures logged at debug until recovery): %s", consecutiveErrors, sanitized)
				default:
					b.logger().Debugf("get updates (failure #%d): %s", consecutiveErrors, sanitized)
				}
				b.logger().Extra("poll_error consecutive=%d timeouts=%d on_ipv4=%v is_timeout=%v action_switch=%v action_escalate=%v err=%s",
					consecutiveErrors, consecutiveTimeouts, b.ipv4Latched(), timeout, action.switchToIPv4, action.escalate, sanitized)

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
			// IPv4 latch management. A successful poll while latched means the
			// v6 blackhole that triggered the switch may have healed (it did on
			// 2026-06-07, #809) — so retry dual-stack rather than abandon IPv6
			// for the whole process. Bounded by maxIPv4Reverts: in a flap the
			// post-revert poll fails on the still-dead v6 path (no success
			// between reverts), so the budget is spent and we eventually latch
			// IPv4 permanently. A genuine recovery yields a *dual-stack* success
			// (the else branch), which restores the full budget.
			if b.ipv4Latched() {
				if !ipv4Permanent {
					if ipv4Reverts < maxIPv4Reverts {
						ipv4Reverts++
						b.revertToDualStack(ipv4Reverts)
					} else {
						ipv4Permanent = true
						b.logger().Infof("keeping Telegram API on IPv4-only for this process: %d dual-stack reverts kept re-hitting the IPv6 blackhole (#809)", ipv4Reverts)
					}
				}
			} else if ipv4Reverts > 0 {
				ipv4Reverts = 0
			}
			consecutiveErrors = 0
			consecutiveTimeouts = 0

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
