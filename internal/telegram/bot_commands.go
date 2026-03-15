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
	// Normalize dot-commands to slash form for keyboard lookups.
	lookupText := text
	if len(text) > 1 && text[0] == '.' && text[1] >= 'a' && text[1] <= 'z' {
		lookupText = "/" + text[1:]
	}

	// Check for keyboard display before dispatch so commands with keyboards
	// don't execute their bare form (which is typically just usage text).
	if name, opts, ok := b.dispatcher.LookupKeyboard(ctx, lookupText); ok {
		b.sendCommandKeyboard(msg.Chat.Id, name, opts)
		return true
	}

	// Check for chain keyboard (e.g. /config set → section buttons).
	if name, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, lookupText); ok {
		label := text + ":"
		_, _ = b.client.SendMessage(msg.Chat.Id, label, &gotgbot.SendMessageOpts{
			ReplyMarkup: buildCommandKeyboard(name, opts),
		})
		return true
	}

	result := b.dispatcher.Dispatch(ctx, msg)
	if !result.Handled {
		return false
	}

	// If the response includes a keyboard, send with keyboard markup.
	if len(result.Response.Keyboard) > 0 {
		text := result.Response.Text
		if len(result.Response.Parts) > 0 {
			text = strings.Join(result.Response.Parts, "\n\n")
		}
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(lookupText), "/"), " ")
		_, _ = b.client.SendMessage(msg.Chat.Id, text, &gotgbot.SendMessageOpts{
			ReplyMarkup: buildCommandKeyboard(cmdName, result.Response.Keyboard),
		})
	} else if len(result.Response.Parts) > 0 {
		for _, part := range result.Response.Parts {
			b.sendReply(msg, part)
		}
	} else if result.Response.Text != "" {
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
