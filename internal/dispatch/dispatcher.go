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
	"foci/internal/tools"
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
	trimmed := strings.TrimSpace(text)

	isDot := len(trimmed) > 1 && trimmed[0] == '.' &&
		(trimmed[1] >= 'a' && trimmed[1] <= 'z' || trimmed[1] >= 'A' && trimmed[1] <= 'Z')
	isSlash := strings.HasPrefix(trimmed, "/")

	if !isDot && !isSlash {
		return Result{}
	}

	body := trimmed[1:]
	name, _, _ := strings.Cut(body, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args := extractArgs(body)

	// A leading-slash token that contains a further slash is a filesystem path
	// (e.g. "/home/foci/x", "/etc/hosts"), not a command — no command name
	// contains "/". Fall through as normal text so paths aren't swallowed as
	// "unknown command". (#770)
	if isSlash && strings.Contains(name, "/") {
		return Result{}
	}

	// Dot commands must match a registered command — otherwise fall through
	// as normal text (e.g. ".something" in a sentence).
	if isDot && d.registry.Get(name) == nil {
		return Result{}
	}

	return d.dispatchRequest(ctx, name, args, sessionKey, userID, chatID)
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
func (d *Dispatcher) LookupKeyboard(ctx context.Context, text string, chatID int64) (string, string, []command.KeyboardOption, bool) {
	ctx = tools.WithSessionKey(ctx, d.sessionKeyForChat(chatID))
	return d.registry.LookupKeyboard(ctx, text, d.cc)
}

// LookupChainKeyboard checks if a command has a chained keyboard to display.
func (d *Dispatcher) LookupChainKeyboard(ctx context.Context, text string, chatID int64) (string, []command.KeyboardOption, bool) {
	sessionKey := d.sessionKeyForChat(chatID)
	ctx = tools.WithSessionKey(ctx, sessionKey)
	return d.registry.LookupChainKeyboard(ctx, text, d.cc)
}

// dispatchRequest dispatches a command request and wraps the result.
func (d *Dispatcher) dispatchRequest(ctx context.Context, name, args, sessionKey, userID string, chatID int64) Result {
	ctx = tools.WithSessionKey(ctx, sessionKey)
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

// DispatchCommand runs the full command dispatch pipeline: normalize dot-commands
// to slash form, check for keyboard display, check for chain keyboard, then dispatch.
// Returns a CommandOutcome describing what the platform should render.
func (d *Dispatcher) DispatchCommand(ctx context.Context, text string, chatID int64, userID string) CommandOutcome {
	// Normalize dot-commands to slash form for keyboard lookups.
	lookupText := text
	if len(text) > 1 && text[0] == '.' && (text[1] >= 'a' && text[1] <= 'z' || text[1] >= 'A' && text[1] <= 'Z') {
		lookupText = "/" + text[1:]
	}

	sessionKey := d.sessionKeyForChat(chatID)
	ctx = tools.WithSessionKey(ctx, sessionKey)

	// Check for keyboard display before dispatch so commands with keyboards
	// don't execute their bare form (which is typically just usage text).
	if name, header, opts, ok := d.registry.LookupKeyboard(ctx, lookupText, d.cc); ok {
		return CommandOutcome{Keyboard: &KeyboardOutcome{CommandName: name, Header: header, Options: opts}}
	}

	// Check for chain keyboard (e.g. /config set → section buttons).
	if name, opts, ok := d.registry.LookupChainKeyboard(ctx, lookupText, d.cc); ok {
		return CommandOutcome{Chain: &ChainOutcome{CommandName: name, Label: text + ":", Options: opts}}
	}

	result := d.DispatchText(ctx, text, chatID, userID)
	if !result.Handled {
		return CommandOutcome{NotHandled: true}
	}
	return CommandOutcome{Response: &ResponseOutcome{Result: result, LookupText: lookupText}}
}

// DispatchCommandCallback runs the callback dispatch pipeline: check for chain
// keyboard, then dispatch. Returns a CommandOutcome for the platform to render.
func (d *Dispatcher) DispatchCommandCallback(ctx context.Context, chatID int64, cmdText string) CommandOutcome {
	sessionKey := d.sessionKeyForChat(chatID)
	ctx = tools.WithSessionKey(ctx, sessionKey)

	// Check for chain keyboard before dispatch (e.g. /config set → section picker).
	stripped := strings.TrimPrefix(cmdText, "/")
	name, _, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(strings.TrimSpace(name))

	if chainName, opts, ok := d.registry.LookupChainKeyboard(ctx, cmdText, d.cc); ok {
		return CommandOutcome{Chain: &ChainOutcome{CommandName: chainName, Label: cmdText + ":", Options: opts}}
	}

	result := d.DispatchCallback(ctx, chatID, cmdText)
	if !result.Handled {
		return CommandOutcome{NotHandled: true}
	}
	_ = name // used only for chain keyboard lookup above
	return CommandOutcome{Response: &ResponseOutcome{Result: result, LookupText: cmdText}}
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
