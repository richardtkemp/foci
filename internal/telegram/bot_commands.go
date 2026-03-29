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
	if name, header, opts, ok := b.dispatcher.LookupKeyboard(ctx, lookupText, msg.Chat.Id); ok {
		b.sendCommandKeyboard(msg.Chat.Id, name, header, opts)
		return true
	}

	// Check for chain keyboard (e.g. /config set → section buttons).
	if name, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, lookupText, msg.Chat.Id); ok {
		label := text + ":"
		_, _ = b.SendTextWithButtons(label, cmdButtons(name, opts), "cmd:")
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
		_, _ = b.SendTextWithButtons(text, cmdButtons(cmdName, result.Response.Keyboard), "cmd:")
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

