// Package dispatch provides platform-agnostic command dispatch logic shared by
// Telegram, Discord, and any future chat frontends. Each platform creates a thin
// wrapper that extracts text, chatID, and userID from the native message type,
// then delegates to Dispatcher.DispatchText.
package dispatch

import (
	"context"
	"strings"

	"foci/internal/command"
	"foci/internal/session"
)

// Dispatcher routes pre-extracted command text to the command registry.
type Dispatcher struct {
	registry     *command.Registry
	cc           command.CommandContext
	agentID      string
	sessionKeyFn func(chatID int64) string // stable session key resolver; falls back to NewChatSessionKey
}

// NewDispatcher creates a new command dispatcher.
func NewDispatcher(registry *command.Registry, cc command.CommandContext, agentID string) *Dispatcher {
	return &Dispatcher{
		registry: registry,
		cc:       cc,
		agentID:  agentID,
	}
}

// SetSessionKeyFunc sets the function used to resolve stable session keys for a chat ID.
func (d *Dispatcher) SetSessionKeyFunc(fn func(chatID int64) string) {
	d.sessionKeyFn = fn
}

// Result holds the outcome of a command dispatch.
type Result struct {
	Handled    bool
	Response   command.Response
	SessionKey string
	UserID     string
}

// DispatchText routes pre-extracted command text to the appropriate handler.
// The caller is responsible for extracting text, chatID, and userID from the
// platform-specific message type.
func (d *Dispatcher) DispatchText(ctx context.Context, text string, chatID int64, userID string) Result {
	sessionKey := d.sessionKeyForChat(chatID)

	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		if result := d.dispatchDotCommand(ctx, text, sessionKey, userID, chatID); result.Handled {
			return result
		}
	}

	if strings.HasPrefix(text, "/") {
		return d.dispatchSlashCommand(ctx, text, sessionKey, userID, chatID)
	}

	return Result{}
}

// DispatchCallback dispatches a command from a button/callback interaction.
func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText string) Result {
	stripped := strings.TrimPrefix(cmdText, "/")
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	return d.dispatchRequest(ctx, name, args, d.sessionKeyForChat(chatID), "", chatID)
}

// LookupKeyboard checks if a command has a keyboard to display.
func (d *Dispatcher) LookupKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupKeyboard(ctx, text, d.cc)
}

// LookupChainKeyboard checks if a command has a chained keyboard to display.
func (d *Dispatcher) LookupChainKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupChainKeyboard(ctx, text, d.cc)
}

// dispatchRequest dispatches a command request and wraps the result.
func (d *Dispatcher) dispatchRequest(ctx context.Context, name, args, sessionKey, userID string, chatID int64) Result {
	req := command.Request{
		Name:       name,
		Args:       args,
		SessionKey: sessionKey,
		UserID:     userID,
		ChatID:     chatID,
	}
	resp, handled, err := d.registry.Dispatch(ctx, req, d.cc)
	if err != nil {
		return Result{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return Result{Handled: handled, Response: resp, SessionKey: sessionKey, UserID: userID}
}

func (d *Dispatcher) dispatchDotCommand(ctx context.Context, text, sessionKey, userID string, chatID int64) Result {
	dotText := strings.TrimSpace(text)[1:]
	cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")

	if d.registry.Get(cmdName) == nil {
		return Result{}
	}

	return d.dispatchRequest(ctx, cmdName, extractArgs(dotText), sessionKey, userID, chatID)
}

func (d *Dispatcher) dispatchSlashCommand(ctx context.Context, text, sessionKey, userID string, chatID int64) Result {
	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	return d.dispatchRequest(ctx, name, args, sessionKey, userID, chatID)
}

func (d *Dispatcher) sessionKeyForChat(chatID int64) string {
	if d.sessionKeyFn != nil {
		return d.sessionKeyFn(chatID)
	}
	return session.NewChatSessionKey(d.agentID, chatID)
}

func extractArgs(text string) string {
	_, args, _ := strings.Cut(text, " ")
	return strings.TrimSpace(args)
}
