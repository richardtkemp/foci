package telegram

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/command"
	"foci/internal/dispatch"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Dispatcher is a thin Telegram wrapper around the shared dispatch.Dispatcher.
// It extracts text, chatID, and userID from gotgbot.Message and delegates to
// the platform-agnostic dispatch logic.
type Dispatcher struct {
	inner *dispatch.Dispatcher
}

func NewDispatcher(registry *command.Registry, cc command.CommandContext, agentID string) *Dispatcher {
	return &Dispatcher{inner: dispatch.NewDispatcher(registry, cc, agentID)}
}

// SetSessionKeyFunc sets the function used to resolve stable session keys for a chat ID.
func (d *Dispatcher) SetSessionKeyFunc(fn func(chatID int64) string) {
	d.inner.SetSessionKeyFunc(fn)
}

// Dispatch routes a Telegram message to the appropriate command handler.
func (d *Dispatcher) Dispatch(ctx context.Context, msg *gotgbot.Message) dispatch.Result {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	if text == "" {
		return dispatch.Result{}
	}
	return d.inner.DispatchText(ctx, text, msg.Chat.Id, fmt.Sprintf("%d", msg.From.Id))
}

// DispatchCallback dispatches a command from a button callback.
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
