package discord

import (
	"context"
	"strconv"
	"strings"

	"foci/internal/dispatch"

	"github.com/bwmarrin/discordgo"
)

// tryDispatchCommand tries to dispatch text as a slash or dot-command.
// Returns true if the message was handled (caller should return).
func (b *Bot) tryDispatchCommand(ctx context.Context, msg *discordgo.Message, text string) bool {
	if text == "" || b.dispatcher == nil {
		return false
	}
	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	outcome := b.dispatcher.DispatchCommand(ctx, text, chatID, msg.Author.ID)
	return b.renderCommandOutcome(msg, &outcome)
}

// renderCommandOutcome renders a CommandOutcome using Discord-native sends.
// Returns true if the outcome was handled (i.e. not NotHandled).
func (b *Bot) renderCommandOutcome(msg *discordgo.Message, outcome *dispatch.CommandOutcome) bool {
	if outcome.NotHandled {
		return false
	}

	if outcome.Keyboard != nil {
		b.sendCommandKeyboard(msg.ChannelID, outcome.Keyboard.CommandName, outcome.Keyboard.Header, outcome.Keyboard.Options)
		return true
	}

	if outcome.Chain != nil {
		_, _ = b.SendTextWithButtons(outcome.Chain.Label, dispatch.CmdButtons(outcome.Chain.CommandName, outcome.Chain.Options), "cmd:")
		return true
	}

	if outcome.Response != nil {
		// Stop typing indicator — commands run outside processAgentMessage
		// so there's no defer to clean up.
		b.SetTyping(false)

		result := outcome.Response.Result
		if len(result.Response.Keyboard) > 0 {
			responseText := result.Response.Text
			if len(result.Response.Parts) > 0 {
				responseText = strings.Join(result.Response.Parts, "\n\n")
			}
			cmdName, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(outcome.Response.LookupText), "/"), " ")
			_, _ = b.SendTextWithButtons(responseText, dispatch.CmdButtons(cmdName, result.Response.Keyboard), "cmd:")
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

	return false
}
