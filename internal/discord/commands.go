package discord

import (
	"context"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// tryDispatchCommand tries to dispatch text as a slash or dot-command.
// Returns true if the message was handled (caller should return).
func (b *Bot) tryDispatchCommand(ctx context.Context, msg *discordgo.Message, text string) bool {
	if text == "" {
		return false
	}
	if b.dispatcher == nil {
		return false
	}
	return b.tryDispatchViaDispatcher(ctx, msg, text)
}

// tryDispatchViaDispatcher uses the platform-aware Dispatcher.
func (b *Bot) tryDispatchViaDispatcher(ctx context.Context, msg *discordgo.Message, text string) bool {
	// Normalize dot-commands to slash form for keyboard lookups.
	lookupText := text
	if len(text) > 1 && text[0] == '.' && text[1] >= 'a' && text[1] <= 'z' {
		lookupText = "/" + text[1:]
	}

	// Check for keyboard display before dispatch so commands with keyboards
	// don't execute their bare form (which is typically just usage text).
	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	if name, header, opts, ok := b.dispatcher.LookupKeyboard(ctx, lookupText, chatID); ok {
		b.sendCommandKeyboard(msg.ChannelID, name, header, opts)
		return true
	}

	// Check for chain keyboard.
	if name, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, lookupText, chatID); ok {
		label := text + ":"
		_, _ = b.SendTextWithButtons(label, cmdButtons(name, opts), "cmd:")
		return true
	}

	result := b.dispatcher.Dispatch(ctx, msg)
	if !result.Handled {
		return false
	}

	// Typing lifecycle owner for command dispatch. Commands run outside
	// processAgentMessage, so there's no defer to clean up. Commands that
	// trigger backend work (e.g. /reset memory formation) start typing via
	// TypingFunc → SetTyping(true); this is the only place that cancels it.
	// Safe to call even if no typing is active (SetTyping(false) is a no-op
	// when the ticker isn't running).
	// See SetTyping doc comment in send.go for the full ownership model.
	b.SetTyping(false)

	// If the response includes a keyboard, send with buttons.
	if len(result.Response.Keyboard) > 0 {
		responseText := result.Response.Text
		if len(result.Response.Parts) > 0 {
			responseText = strings.Join(result.Response.Parts, "\n\n")
		}
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(lookupText), "/"), " ")
		_, _ = b.SendTextWithButtons(responseText, cmdButtons(cmdName, result.Response.Keyboard), "cmd:")
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

