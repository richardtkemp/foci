package telegram

import (
	"context"
	"strings"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// tryDispatchCommand tries to dispatch text as a slash or dot-command.
// Returns true if the message was handled (caller should return).
func (b *Bot) tryDispatchCommand(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
	if text == "" {
		return false
	}

	// /stop and /done are handled locally (not dispatched to command registry)
	if strings.HasPrefix(text, "/") {
		cmd := strings.ToLower(strings.TrimSpace(text))
		if b.isStopCommand(cmd) {
			b.cancelTurn()
			b.sendReply(msg, userID, "Stopped.")
			return true
		}
		if cmd == "/done" {
			return b.handleDoneCommand(msg, userID)
		}
	}

	// Try platform-aware dispatcher first
	if b.dispatcher != nil {
		return b.tryDispatchViaDispatcher(ctx, msg, userID, text)
	}

	// Direct dispatch (no Dispatcher configured)
	return b.tryDispatchDirect(ctx, msg, userID, text)
}

// tryDispatchViaDispatcher uses the platform-aware Dispatcher.
func (b *Bot) tryDispatchViaDispatcher(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
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
		b.sendReply(msg, userID, result.Response.Text)
	}
	if result.Response.DocPath != "" {
		_ = b.SendDocument(result.Response.DocPath)
	}
	return true
}

// tryDispatchDirect dispatches commands directly to the command registry.
func (b *Bot) tryDispatchDirect(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
	// Dot-command alias (.xxx → /xxx) — easier to type on phone keyboards.
	// Only treated as a command if it matches a registered command.
	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		dotText := strings.TrimSpace(text)[1:] // strip leading dot, preserve case
		cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")
		if b.commands.Get(cmdName) != nil || b.isStopCommand("/"+cmdName) {
			dotCmd := "/" + dotText
			cmdCtx := b.commandContext(ctx, userID, msg.Chat.Id)
			if _, opts, ok := b.commands.LookupKeyboard(cmdCtx, dotCmd); ok {
				b.sendCommandKeyboard(msg.Chat.Id, cmdName, opts)
				return true
			}
			if result, ok := b.commands.Dispatch(cmdCtx, dotCmd); ok {
				b.logger().Debugf("command %s → %s dispatched", text, dotCmd)
				b.sendReply(msg, userID, result)
				return true
			}
		}
	}

	// Slash commands
	if strings.HasPrefix(text, "/") {
		cmdCtx := b.commandContext(ctx, userID, msg.Chat.Id)

		// Check for inline keyboard (bare command, no args)
		if name, opts, ok := b.commands.LookupKeyboard(cmdCtx, text); ok {
			b.logger().Debugf("command /%s showing keyboard (%d options)", name, len(opts))
			b.sendCommandKeyboard(msg.Chat.Id, name, opts)
			return true
		}

		if result, ok := b.commands.Dispatch(cmdCtx, text); ok {
			b.logger().Debugf("command %s dispatched", text)
			b.sendReply(msg, userID, result)
			return true
		}
	}

	return false
}

// handleDoneCommand handles the /done command for secondary bots.
func (b *Bot) handleDoneCommand(msg *gotgbot.Message, userID string) bool {
	if !b.isSecondary {
		b.sendReply(msg, userID, "Nothing to detach — this is the main session.")
		return true
	}
	sk := b.SessionKey()
	if sk == "" {
		b.sendReply(msg, userID, "Already idle.")
		return true
	}
	b.cancelTurn()
	if b.pool != nil {
		b.pool.Release(b)
	}
	b.sendReply(msg, userID, "Session ended.")
	b.logger().Infof("secondary bot detached from %s", sk)
	return true
}

// commandContext creates a context with metadata for command dispatch.
func (b *Bot) commandContext(ctx context.Context, userID string, chatID int64) context.Context {
	ctx = context.WithValue(ctx, command.LastMessageUserKey{}, userID)
	ctx = context.WithValue(ctx, command.ChatIDKey{}, chatID)
	return ctx
}
