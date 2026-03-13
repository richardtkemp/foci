package telegram

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/command"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

type Dispatcher struct {
	registry     *command.Registry
	deps         command.Deps
	agentID      string
	sessionKeyFn func(chatID int64) string // stable session key resolver; falls back to NewChatSessionKey
}

func NewDispatcher(registry *command.Registry, deps command.Deps, agentID string) *Dispatcher {
	return &Dispatcher{
		registry: registry,
		deps:     deps,
		agentID:  agentID,
	}
}

// SetSessionKeyFunc sets the function used to resolve stable session keys for a chat ID.
func (d *Dispatcher) SetSessionKeyFunc(fn func(chatID int64) string) {
	d.sessionKeyFn = fn
}

type DispatchResult struct {
	Handled   bool
	Response  command.Response
	SessionKey string
	UserID    string
}

func (d *Dispatcher) Dispatch(ctx context.Context, msg *gotgbot.Message) DispatchResult {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	if text == "" {
		return DispatchResult{}
	}

	sessionKey := d.sessionKeyForChat(msg.Chat.Id)
	userID := fmt.Sprintf("%d", msg.From.Id)

	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		if result := d.dispatchDotCommand(ctx, msg, text, sessionKey, userID); result.Handled {
			return result
		}
	}

	if strings.HasPrefix(text, "/") {
		return d.dispatchSlashCommand(ctx, msg, text, sessionKey, userID)
	}

	return DispatchResult{}
}

func (d *Dispatcher) dispatchDotCommand(ctx context.Context, _ *gotgbot.Message, text, sessionKey, userID string) DispatchResult {
	dotText := strings.TrimSpace(text)[1:]
	cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")

	if d.registry.Get(cmdName) == nil {
		return DispatchResult{}
	}

	req := command.Request{
		Name:      cmdName,
		Args:      extractArgs(dotText),
		SessionKey: sessionKey,
		UserID:    userID,
	}

	resp, handled, err := d.registry.DispatchRich(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionKey: sessionKey, UserID: userID}
}

func (d *Dispatcher) dispatchSlashCommand(ctx context.Context, _ *gotgbot.Message, text, sessionKey, userID string) DispatchResult {
	cmd := strings.ToLower(strings.TrimSpace(text))

	if cmd == "/stop" || cmd == "/done" {
		return DispatchResult{}
	}

	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	req := command.Request{
		Name:      name,
		Args:      args,
		SessionKey: sessionKey,
		UserID:    userID,
	}

	resp, handled, err := d.registry.DispatchRich(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionKey: sessionKey, UserID: userID}
}

func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText string) DispatchResult {
	stripped := strings.TrimPrefix(cmdText, "/")
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	sessionKey := d.sessionKeyForChat(chatID)

	req := command.Request{
		Name:      name,
		Args:      args,
		SessionKey: sessionKey,
		UserID:    "",
	}

	resp, handled, err := d.registry.DispatchRich(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionKey: sessionKey}
}

func (d *Dispatcher) sessionKeyForChat(chatID int64) string {
	if d.sessionKeyFn != nil {
		return d.sessionKeyFn(chatID)
	}
	return session.NewChatSessionKey(d.agentID, chatID)
}

func (d *Dispatcher) LookupKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupKeyboard(ctx, text)
}

func (d *Dispatcher) LookupChainKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupChainKeyboard(ctx, text)
}

func extractArgs(text string) string {
	_, args, _ := strings.Cut(text, " ")
	return strings.TrimSpace(args)
}
