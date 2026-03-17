package discord

import (
	"context"
	"strconv"
	"strings"

	"foci/internal/command"
	"foci/internal/session"

	"github.com/bwmarrin/discordgo"
)

// Dispatcher routes commands from Discord messages to the command registry.
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

// DispatchResult holds the outcome of a command dispatch.
type DispatchResult struct {
	Handled    bool
	Response   command.Response
	SessionKey string
	UserID     string
}

// Dispatch routes a message to the appropriate command handler.
func (d *Dispatcher) Dispatch(ctx context.Context, msg *discordgo.Message) DispatchResult {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return DispatchResult{}
	}

	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	sessionKey := d.sessionKeyForChat(chatID)
	userID := msg.Author.ID

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

// dispatchRequest dispatches a command request and wraps the result.
func (d *Dispatcher) dispatchRequest(ctx context.Context, name, args, sessionKey, userID string, chatID int64) DispatchResult {
	req := command.Request{
		Name:       name,
		Args:       args,
		SessionKey: sessionKey,
		UserID:     userID,
		ChatID:     chatID,
	}
	resp, handled, err := d.registry.Dispatch(ctx, req, d.cc)
	if err != nil {
		return DispatchResult{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return DispatchResult{Handled: handled, Response: resp, SessionKey: sessionKey, UserID: userID}
}

func (d *Dispatcher) dispatchDotCommand(ctx context.Context, msg *discordgo.Message, text, sessionKey, userID string) DispatchResult {
	dotText := strings.TrimSpace(text)[1:]
	cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")

	if d.registry.Get(cmdName) == nil {
		return DispatchResult{}
	}

	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	return d.dispatchRequest(ctx, cmdName, extractArgs(dotText), sessionKey, userID, chatID)
}

func (d *Dispatcher) dispatchSlashCommand(ctx context.Context, msg *discordgo.Message, text, sessionKey, userID string) DispatchResult {
	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	chatID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	return d.dispatchRequest(ctx, name, args, sessionKey, userID, chatID)
}

// DispatchCallback dispatches a command from a button interaction.
func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText string) DispatchResult {
	stripped := strings.TrimPrefix(cmdText, "/")
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)

	return d.dispatchRequest(ctx, name, args, d.sessionKeyForChat(chatID), "", chatID)
}

func (d *Dispatcher) sessionKeyForChat(chatID int64) string {
	if d.sessionKeyFn != nil {
		return d.sessionKeyFn(chatID)
	}
	return session.NewChatSessionKey(d.agentID, chatID)
}

// LookupKeyboard checks if a command has a keyboard to display.
func (d *Dispatcher) LookupKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupKeyboard(ctx, text, d.cc)
}

// LookupChainKeyboard checks if a command has a chained keyboard to display.
func (d *Dispatcher) LookupChainKeyboard(ctx context.Context, text string) (string, []command.KeyboardOption, bool) {
	return d.registry.LookupChainKeyboard(ctx, text, d.cc)
}

func extractArgs(text string) string {
	_, args, _ := strings.Cut(text, " ")
	return strings.TrimSpace(args)
}

