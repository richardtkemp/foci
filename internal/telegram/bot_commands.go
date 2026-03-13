package telegram

import (
	"context"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// tryDispatchCommand tries to dispatch text as a slash or dot-command.
// Returns true if the message was handled (caller should return).
func (b *Bot) tryDispatchCommand(ctx context.Context, msg *gotgbot.Message, text string) bool {
	if text == "" {
		return false
	}

	// /stop and /done are handled locally (not dispatched to command registry)
	if strings.HasPrefix(text, "/") {
		cmd := strings.ToLower(strings.TrimSpace(text))
		if b.isStopCommand(cmd) {
			b.cancelTurn()
			b.sendReply(msg, "Stopped.")
			return true
		}
		if cmd == "/done" {
			return b.handleDoneCommand(msg)
		}
	}

	if b.dispatcher == nil {
		return false
	}

	return b.tryDispatchViaDispatcher(ctx, msg, text)
}

// tryDispatchViaDispatcher uses the platform-aware Dispatcher.
func (b *Bot) tryDispatchViaDispatcher(ctx context.Context, msg *gotgbot.Message, text string) bool {
	result := b.dispatcher.Dispatch(ctx, msg)
	if !result.Handled {
		return false
	}

	// Check for keyboard display
	if name, opts, ok := b.dispatcher.LookupKeyboard(ctx, text); ok {
		b.sendCommandKeyboard(msg.Chat.Id, name, opts)
		return true
	}

	if result.Response.Text != "" {
		b.sendReply(msg, result.Response.Text)
	}
	if result.Response.DocPath != "" {
		_ = b.SendDocument(result.Response.DocPath)
	}
	return true
}

// handleDoneCommand handles the /done command for secondary bots.
func (b *Bot) handleDoneCommand(msg *gotgbot.Message) bool {
	if !b.isSecondary {
		b.sendReply(msg, "Nothing to detach — this is the main session.")
		return true
	}
	sk := b.SessionKey()
	if sk == "" {
		b.sendReply(msg, "Already idle.")
		return true
	}
	b.cancelTurn()
	if b.pool != nil {
		b.pool.Release(b)
	}
	b.sendReply(msg, "Session ended.")
	b.logger().Infof("secondary bot detached from %s", sk)
	return true
}
