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
	registry *command.Registry
	deps     command.Deps
	agentID  string
}

func NewDispatcher(registry *command.Registry, deps command.Deps, agentID string) *Dispatcher {
	return &Dispatcher{
		registry: registry,
		deps:     deps,
		agentID:  agentID,
	}
}

type DispatchResult struct {
	Handled   bool
	Response  command.Response
	SessionID string
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

	sessionID := d.sessionKeyForChat(msg.Chat.Id)
	userID := fmt.Sprintf("%d", msg.From.Id)

	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		if result := d.dispatchDotCommand(ctx, msg, text, sessionID, userID); result.Handled {
			return result
		}
	}

	if strings.HasPrefix(text, "/") {
		return d.dispatchSlashCommand(ctx, msg, text, sessionID, userID)
	}

	return DispatchResult{}
}

func (d *Dispatcher) dispatchDotCommand(ctx context.Context, _ *gotgbot.Message, text, sessionID, userID string) DispatchResult {
	dotText := strings.TrimSpace(text)[1:]
	cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")

	if d.registry.Get(cmdName) == nil {
		return DispatchResult{}
	}

	req := command.Request{
		Name:      cmdName,
		Args:      extractArgs(dotText),
		SessionID: sessionID,
		UserID:    userID,
	}

	resp, handled, err := d.registry.DispatchV2(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionID: sessionID, UserID: userID}
}

func (d *Dispatcher) dispatchSlashCommand(ctx context.Context, _ *gotgbot.Message, text, sessionID, userID string) DispatchResult {
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
		SessionID: sessionID,
		UserID:    userID,
	}

	resp, handled, err := d.registry.DispatchV2(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionID: sessionID, UserID: userID}
}

func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText string) DispatchResult {
	stripped := strings.TrimPrefix(cmdText, "/")
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	sessionID := d.sessionKeyForChat(chatID)

	req := command.Request{
		Name:      name,
		Args:      args,
		SessionID: sessionID,
		UserID:    "",
	}

	resp, handled, err := d.registry.DispatchV2(ctx, req, d.deps)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionID: sessionID}
}

func (d *Dispatcher) sessionKeyForChat(chatID int64) string {
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
