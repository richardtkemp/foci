package discord

import (
	"context"
	"strconv"
	"strings"

	"foci/internal/command"
	"foci/internal/dispatch"

	"github.com/bwmarrin/discordgo"
)

// Dispatcher is a thin Discord wrapper around the shared dispatch.Dispatcher.
// It extracts text, chatID, and userID from discordgo.Message and delegates to
// the platform-agnostic dispatch logic.
type Dispatcher struct {
	inner *dispatch.Dispatcher
}

// NewDispatcher creates a new command dispatcher.
func NewDispatcher(registry *command.Registry, cc command.CommandContext, agentID string) *Dispatcher {
	return &Dispatcher{inner: dispatch.NewDispatcher(registry, cc, agentID)}
}

// SetSessionKeyFunc sets the function used to resolve stable session keys for a chat ID.
func (d *Dispatcher) SetSessionKeyFunc(fn func(chatID int64) string) {
	d.inner.SetSessionKeyFunc(fn)
}

// Dispatch routes a Discord message to the appropriate command handler.
func (d *Dispatcher) Dispatch(ctx context.Context, msg *discordgo.Message) dispatch.Result {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return dispatch.Result{}
	}

	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	return d.inner.DispatchText(ctx, text, chatID, msg.Author.ID)
}

// DispatchCallback dispatches a command from a button interaction.
func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText string) dispatch.Result {
	return d.inner.DispatchCallback(ctx, chatID, cmdText)
}

// LookupKeyboard checks if a command has a keyboard to display.
func (d *Dispatcher) LookupKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.inner.LookupKeyboard(ctx, text)
}

// LookupChainKeyboard checks if a command has a chained keyboard to display.
func (d *Dispatcher) LookupChainKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.inner.LookupChainKeyboard(ctx, text)
}
