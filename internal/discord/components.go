package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"foci/internal/command"

	"github.com/bwmarrin/discordgo"
)

// buildCommandButtons creates Discord message component action rows from keyboard options.
// Buttons use custom IDs prefixed with "cmd:/cmdName " for routing.
func buildCommandButtons(cmdName string, opts []command.KeyboardOption) []discordgo.MessageComponent {
	// Group by row
	rowMap := make(map[int][]discordgo.MessageComponent)
	maxRow := 0
	for _, opt := range opts {
		btn := discordgo.Button{
			Label:    opt.Label,
			Style:    discordgo.PrimaryButton,
			CustomID: fmt.Sprintf("cmd:/%s %s", cmdName, opt.Data),
		}
		rowMap[opt.Row] = append(rowMap[opt.Row], btn)
		if opt.Row > maxRow {
			maxRow = opt.Row
		}
	}

	var components []discordgo.MessageComponent
	for i := 0; i <= maxRow; i++ {
		buttons, ok := rowMap[i]
		if !ok {
			continue
		}
		// Discord allows at most 5 buttons per action row
		for len(buttons) > 0 {
			end := 5
			if end > len(buttons) {
				end = len(buttons)
			}
			components = append(components, discordgo.ActionsRow{
				Components: buttons[:end],
			})
			buttons = buttons[end:]
		}
	}
	return components
}

// singleButton returns a slice of message components with one action row and one button.
func singleButton(label, customID string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    label,
					Style:    discordgo.SecondaryButton,
					CustomID: customID,
				},
			},
		},
	}
}

// sendCommandKeyboard sends a message with command keyboard buttons.
func (b *Bot) sendCommandKeyboard(channelID, cmdName string, opts []command.KeyboardOption) {
	label := fmt.Sprintf("/%s:", cmdName)
	buttons := buildCommandButtons(cmdName, opts)
	_, _ = b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    label,
		Components: buttons,
	})
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
	if parentName, opts, ok := b.dispatcher.LookupChainKeyboard(ctx, cmdText); ok {
		display := "/" + parentName + " " + strings.TrimPrefix(cmdText, "/"+parentName+" ") + ":"
		buttons := buildCommandButtons(parentName, opts)
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
		buttons := buildCommandButtons(cmdName, dr.Response.Keyboard)
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
		buttons := singleButton("Hide", "tc:hide")
		if len(expanded) > discordMaxChars {
			expanded = expanded[:discordMaxChars-4] + "\n..."
		}
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgIDInt, stored)
		buttons := singleButton("Show full", "tc:show")
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
		buttons := singleButton("Hide thinking", "th:hide")
		if len(expanded) > discordMaxChars {
			expanded = expanded[:discordMaxChars-4] + "\n..."
		}
		_, _ = b.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    channelID,
			ID:         msgID,
			Content:    &expanded,
			Components: &buttons,
		})
	case "hide":
		buttons := singleButton("Show thinking", "th:show")
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

