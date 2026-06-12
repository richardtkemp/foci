package discord

import (
	"context"
	"strconv"
	"strings"

	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// sendCommandKeyboard sends a message with command keyboard buttons.
func (b *Bot) sendCommandKeyboard(cmdName string, header string, opts []command.KeyboardOption) {
	_, _ = b.SendTextWithButtons(header, dispatch.CmdButtons(cmdName, opts), "cmd:")
}

// handleComponentInteraction dispatches button interactions.
func (b *Bot) handleComponentInteraction(ctx context.Context, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	if data.CustomID == "" {
		return
	}

	channelID := i.ChannelID
	chatID, _ := strconv.ParseInt(channelID, 10, 64)

	// Always acknowledge the interaction to prevent the "interaction failed" error.
	_ = b.api.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})

	msgID := i.Message.ID
	cbAction, cbData := dispatch.ParseCallback(data.CustomID)
	switch cbAction {
	case dispatch.CallbackCommand:
		b.handleCommandCallback(ctx, channelID, msgID, cbData, chatID)
	case dispatch.CallbackInteractive:
		editText, _, ok := platform.HandleInteractiveCallback(cbData)
		if ok && editText != "" {
			noComponents := []discordgo.MessageComponent{}
			_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel:    channelID,
				ID:         msgID,
				Content:    &editText,
				Components: &noComponents,
			})
		}
	case dispatch.CallbackToolCall:
		b.handleToolCallCallback(channelID, cbData, msgID)
	case dispatch.CallbackThinking:
		b.handleThinkingCallback(channelID, cbData, msgID)
	}
}

// handleCommandCallback executes a command from a button press
// and edits the original message to show the result.
func (b *Bot) handleCommandCallback(ctx context.Context, channelID, msgID, cmdText string, chatID int64) {
	if b.dispatcher == nil {
		return
	}

	outcome := b.dispatcher.DispatchCommandCallback(ctx, chatID, cmdText)

	if outcome.Chain != nil {
		display := "/" + outcome.Chain.CommandName + " " + strings.TrimPrefix(cmdText, "/"+outcome.Chain.CommandName+" ") + ":"
		buttons := buildButtonComponents(dispatch.CmdButtons(outcome.Chain.CommandName, outcome.Chain.Options), "cmd:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &display,
			Components: &buttons,
		})
		return
	}

	var result string
	var resp command.Response
	if outcome.Response != nil {
		resp = outcome.Response.Result.Response
		if len(resp.Parts) > 0 {
			result = strings.Join(resp.Parts, "\n\n")
		} else {
			result = resp.Text
		}
		if !outcome.Response.Result.Handled {
			result = "Unknown command: " + cmdText
		}
	} else {
		result = "Unknown command: " + cmdText
	}

	if len(result) > discordMaxChars {
		result = result[:discordMaxChars-4] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	// If the response includes a keyboard, edit with both text and buttons.
	if len(resp.Keyboard) > 0 {
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(cmdText, "/"), " ")
		buttons := buildButtonComponents(dispatch.CmdButtons(cmdName, resp.Keyboard), "cmd:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &result,
			Components: &buttons,
		})
		return
	}

	noComponents := []discordgo.MessageComponent{}
	_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         msgID,
		Content:    &result,
		Components: &noComponents,
	})
}

// handleToolCallCallback handles tool call expand/collapse button presses.
func (b *Bot) handleToolCallCallback(channelID, action, msgID string) {
	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	toolTextVal, ok := b.toolResults.Load(msgIDInt)
	if !ok {
		return
	}
	stored := toolTextVal.(toolResultEntry)

	switch action {
	case "show":
		var expanded string
		if stored.result == "" {
			expanded = stored.fullInput + "\n\n**Result:** Running..."
		} else {
			expanded = formatToolCallWithResult(stored.fullInput, stored.result)
		}
		stored.expanded = true
		stored.channelID = channelID
		b.toolResults.Store(msgIDInt, stored)
		if len(expanded) > discordMaxChars {
			expanded = expanded[:discordMaxChars-4] + "\n..."
		}
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Hide", Data: "hide"}}, "tc:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgIDInt, stored)
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show full", Data: "show"}}, "tc:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &stored.compactText,
			Components: &buttons,
		})
	}
}

// handleThinkingCallback handles thinking block expand/collapse button presses.
func (b *Bot) handleThinkingCallback(channelID, action, msgID string) {
	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	val, ok := b.thinkingStore.Load(msgIDInt)
	if !ok {
		return
	}
	entry := val.(thinkingEntry)
	dw := b.resolveDisplay(b.sessionKeyForMsg(0)).DisplayWidth

	switch action {
	case "show":
		expanded := formatThinkingExpanded(entry.thinkingText, entry.responseText, dw)
		if len(expanded) > discordMaxChars {
			expanded = expanded[:discordMaxChars-4] + "\n..."
		}
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Hide thinking", Data: "hide"}}, "th:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
		_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &entry.responseText,
			Components: &buttons,
		})
	}
}

// formatThinkingExpanded prepends thinking text above a separator, with the response below.
func formatThinkingExpanded(thinkingText, responseText string, displayWidth int) string {
	divider := "\n" + strings.Repeat("-", displayWidth) + "\n\n"
	return "*" + thinkingText + "*" + divider + responseText
}
