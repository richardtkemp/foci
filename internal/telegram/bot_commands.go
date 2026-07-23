package telegram

import (
	"strings"

	"foci/internal/dispatch"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// renderCommandOutcome renders a CommandOutcome using Telegram-native sends.
// NotHandled outcomes are silently ignored.
func (b *Bot) renderCommandOutcome(msg *gotgbot.Message, outcome *dispatch.CommandOutcome) {
	if outcome.NotHandled {
		return
	}

	if outcome.Keyboard != nil {
		b.sendCommandKeyboard(outcome.Keyboard.CommandName, outcome.Keyboard.Header, outcome.Keyboard.Options)
		return
	}

	if outcome.Chain != nil {
		_, _ = b.SendTextWithButtons(outcome.Chain.Label, dispatch.CmdButtons(outcome.Chain.CommandName, outcome.Chain.Options), "cmd:")
		return
	}

	if outcome.Response != nil {
		// Stop typing indicator — commands run outside processAgentMessage
		// so there's no defer to clean up.
		b.SetTyping(false)

		result := outcome.Response.Result
		if len(result.Response.Keyboard) > 0 {
			text := result.Response.Text
			if len(result.Response.Parts) > 0 {
				text = strings.Join(result.Response.Parts, "\n\n")
			}
			cmdName, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(outcome.Response.LookupText), "/"), " ")
			_, _ = b.SendTextWithButtons(text, dispatch.CmdButtons(cmdName, result.Response.Keyboard), "cmd:")
		} else if len(result.Response.Parts) > 0 {
			for _, part := range result.Response.Parts {
				b.sendReply(msg, part)
			}
		} else if result.Response.Text != "" {
			b.sendReply(msg, result.Response.Text)
		}
		_ = platform.SendDocAndRemove(b, msg.Chat.Id, result.Response.DocPath, "")
	}
}
