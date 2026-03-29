package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"foci/internal/command"
	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// cmdButtons converts command keyboard options to platform.ButtonChoice with
// callback data formatted as "/cmdName optData".
func cmdButtons(cmdName string, opts []command.KeyboardOption) []platform.ButtonChoice {
	btns := make([]platform.ButtonChoice, len(opts))
	for i, opt := range opts {
		btns[i] = platform.ButtonChoice{
			Label: opt.Label,
			Data:  fmt.Sprintf("/%s %s", cmdName, opt.Data),
			Row:   opt.Row,
		}
	}
	return btns
}

// sendCommandKeyboard sends a message with command keyboard buttons.
func (b *Bot) sendCommandKeyboard(channelID, cmdName string, header string, opts []command.KeyboardOption) {
	_, _ = b.SendTextWithButtons(header, cmdButtons(cmdName, opts), "cmd:")
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
	_ = b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})

	// Command button callbacks: "cmd:/name args"
	if strings.HasPrefix(data.CustomID, "cmd:") {
		cmdText := data.CustomID[4:] // strip "cmd:" prefix
		b.handleCommandCallback(ctx, channelID, i.Message.ID, cmdText, chatID)
		return
	}

	// Interactive message callbacks: "im:<promptID>:<buttonIndex>"
	if strings.HasPrefix(data.CustomID, "im:") {
		editText, _, ok := platform.HandleInteractiveCallback(data.CustomID[3:])
		if ok && editText != "" {
			noComponents := []discordgo.MessageComponent{}
			_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel:    channelID,
				ID:         i.Message.ID,
				Content:    &editText,
				Components: &noComponents,
			})
		}
		return
	}

	parts := strings.SplitN(data.CustomID, ":", 2)
	if len(parts) != 2 {
		return
	}
	action := parts[1] // "show" or "hide"
	msgID := i.Message.ID

	switch parts[0] {
	case "tc":
		b.handleToolCallCallback(channelID, action, msgID)
	case "th":
		b.handleThinkingCallback(channelID, action, msgID)
	}
}

// handleCommandCallback executes a command from a button press
// and edits the original message to show the result.
func (b *Bot) handleCommandCallback(ctx context.Context, channelID, msgID, cmdText string, chatID int64) {
	if b.dispatcher == nil {
		return
	}

	// Check for chained keyboard
	if parentName, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, cmdText, chatID); ok {
		display := "/" + parentName + " " + strings.TrimPrefix(cmdText, "/"+parentName+" ") + ":"
		buttons := buildButtonComponents(cmdButtons(parentName, opts), "cmd:")
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &display,
			Components: &buttons,
		})
		return
	}

	dr := b.dispatcher.DispatchCallback(ctx, chatID, cmdText)
	var result string
	if len(dr.Response.Parts) > 0 {
		result = strings.Join(dr.Response.Parts, "\n\n")
	} else {
		result = dr.Response.Text
	}
	if !dr.Handled {
		result = "Unknown command: " + cmdText
	}

	if len(result) > discordMaxChars {
		result = result[:discordMaxChars-4] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	// If the response includes a keyboard, edit with both text and buttons.
	if len(dr.Response.Keyboard) > 0 {
		cmdName, _, _ := strings.Cut(strings.TrimPrefix(cmdText, "/"), " ")
		buttons := buildButtonComponents(cmdButtons(cmdName, dr.Response.Keyboard), "cmd:")
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &result,
			Components: &buttons,
		})
		return
	}

	noComponents := []discordgo.MessageComponent{}
	_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
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
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgIDInt, stored)
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show full", Data: "show"}}, "tc:")
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
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
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		buttons := buildButtonComponents([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
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

