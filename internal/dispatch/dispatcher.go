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
	source       string                    // originating transport (a command.Source* constant); stamped on every dispatched Request
	sessionKeyFn func(chatID int64) string // stable session key resolver; falls back to NewChatSessionKey
}

// NewDispatcher creates a new command dispatcher. source is the calling
// platform (command.SourceTelegram / command.SourceDiscord / …) and travels on
// every dispatched command.Request so surface-aware commands and help
// rendering know where a command came from.
func NewDispatcher(registry *command.Registry, cc command.CommandContext, agentID, source string) *Dispatcher {
	return &Dispatcher{
		registry: registry,
		cc:       cc,
		agentID:  agentID,
		source:   source,
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

// isDotCommand reports whether trimmed text starts a dot-command: a leading
// '.' followed by an ASCII letter (".status", not ".sigh" or a lone ".").
// DispatchText and DispatchCommand share this one predicate, and both
// classify TRIMMED text, so leading whitespace can never make one take a
// different path than the other.
func isDotCommand(trimmed string) bool {
	return len(trimmed) > 1 && trimmed[0] == '.' &&
		(trimmed[1] >= 'a' && trimmed[1] <= 'z' || trimmed[1] >= 'A' && trimmed[1] <= 'Z')
}

// DispatchText routes pre-extracted command text to the appropriate handler.
// The caller is responsible for extracting text, chatID, and userID from the
// platform-specific message type.
func (d *Dispatcher) DispatchText(ctx context.Context, text string, chatID int64, userID string) Result {
	sessionKey := d.SessionKeyForChat(chatID)
	trimmed := strings.TrimSpace(text)

	isDot := isDotCommand(trimmed)
	isSlash := strings.HasPrefix(trimmed, "/")

	if !isDot && !isSlash {
		return Result{}
	}

	body := trimmed[1:]
	name, _, _ := strings.Cut(body, " ")
	name = command.CommandKey(name)
	args := extractArgs(body)

	// A leading-slash filesystem path is not a command — fall through as normal
	// text so paths aren't swallowed as "unknown command". Shares isSlashPath
	// with IsRoutableCommand so the two predicates can't drift (#770).
	if isSlash && isSlashPath(trimmed) {
		return Result{}
	}

	// Dot commands must match a registered command — otherwise fall through
	// as normal text (e.g. ".something" in a sentence).
	if isDot && d.registry.Get(name) == nil {
		return Result{}
	}

	return d.dispatchRequest(ctx, name, args, sessionKey, userID, chatID, "")
}

// DispatchCallback dispatches a command from a button/callback interaction.
// pressedBy is who pressed the button, stamped as Request.PressedBy: the
// identity the operator gate, /help and every other Caller() reader judge
// (#2279). The Request carries no UserID: the sender of the message the button
// rode on is unknown on this path, so user-keyed commands (/repeat) answer for
// nobody, exactly as before the gate existed.
func (d *Dispatcher) DispatchCallback(ctx context.Context, chatID int64, cmdText, pressedBy string) Result {
	name, args, _ := strings.Cut(cmdText, " ")
	args = strings.TrimSpace(args)

	return d.dispatchRequest(ctx, command.CommandKey(name), args, d.SessionKeyForChat(chatID), "", chatID, pressedBy)
}

// dispatchRequest dispatches a command request and wraps the result. userID
// is the sender (empty on the callback path); pressedBy is the presser of a
// cmd: button (empty for a typed command). Request.Caller() picks between them.
func (d *Dispatcher) dispatchRequest(ctx context.Context, name, args, sessionKey, userID string, chatID int64, pressedBy string) Result {
	ctx = tools.WithSessionKey(ctx, sessionKey)
	req := command.Request{
		Name:       name,
		Args:       args,
		SessionKey: sessionKey,
		UserID:     userID,
		ChatID:     chatID,
		Source:     d.source,
		PressedBy:  pressedBy,
	}
	resp, handled, err := d.registry.Dispatch(ctx, req, d.cc)
	if err != nil {
		return Result{Handled: true, Response: command.Response{Text: "Error: " + err.Error()}}
	}
	return Result{Handled: handled, Response: resp, SessionKey: sessionKey, UserID: userID}
}

// caller stamps userID with this dispatcher's transport source — the identity
// the operator gate checks (#2279).
func (d *Dispatcher) caller(userID string) command.Caller {
	return command.Caller{Source: d.source, UserID: userID}
}

// DispatchCommand runs the full command dispatch pipeline: normalize dot-commands
// to slash form, check for keyboard display, check for chain keyboard, then dispatch.
// Returns a CommandOutcome describing what the platform should render.
func (d *Dispatcher) DispatchCommand(ctx context.Context, text string, chatID int64, userID string) CommandOutcome {
	// Classify trimmed text, exactly as DispatchText does, so a dot-command
	// behind whitespace (" .tmux") reaches the keyboard lookups like its
	// trimmed form instead of falling through to the bare command.
	text = strings.TrimSpace(text)
	lookupText := text
	if isDotCommand(text) {
		lookupText = "/" + text[1:]
	}

	sessionKey := d.SessionKeyForChat(chatID)
	ctx = tools.WithSessionKey(ctx, sessionKey)
	caller := d.caller(userID)

	// Check for keyboard display before dispatch so commands with keyboards
	// don't execute their bare form (which is typically just usage text).
	if name, header, opts, ok := d.registry.LookupKeyboard(ctx, lookupText, caller, d.cc); ok {
		return CommandOutcome{Keyboard: &KeyboardOutcome{CommandName: name, Header: header, Options: opts}}
	}

	// Check for chain keyboard (e.g. /config set → section buttons).
	if name, opts, ok := d.registry.LookupChainKeyboard(ctx, lookupText, caller, d.cc); ok {
		return CommandOutcome{Chain: &ChainOutcome{CommandName: name, Label: text + ":", Options: opts}}
	}

	genBefore := d.registry.WizardGen(sessionKey)
	result := d.DispatchText(ctx, text, chatID, userID)
	if !result.Handled {
		return CommandOutcome{NotHandled: true}
	}
	ro := &ResponseOutcome{Result: result, LookupText: lookupText}
	d.setActivatedWizardStep(ro, sessionKey, genBefore)
	return CommandOutcome{Response: ro}
}

// DispatchCommandCallback runs the callback dispatch pipeline: check for chain
// keyboard, then dispatch. userID is the presser, who may differ from the
// sender of the message the button rode on; the operator gate checks the
// presser (#2279). Returns a CommandOutcome for the platform to render.
func (d *Dispatcher) DispatchCommandCallback(ctx context.Context, chatID int64, cmdText, userID string) CommandOutcome {
	sessionKey := d.SessionKeyForChat(chatID)
	ctx = tools.WithSessionKey(ctx, sessionKey)
	caller := d.caller(userID)

	// Check for chain keyboard before dispatch (e.g. /config set → section picker).
	if chainName, opts, ok := d.registry.LookupChainKeyboard(ctx, cmdText, caller, d.cc); ok {
		return CommandOutcome{Chain: &ChainOutcome{CommandName: chainName, Label: cmdText + ":", Options: opts}}
	}

	genBefore := d.registry.WizardGen(sessionKey)
	result := d.DispatchCallback(ctx, chatID, cmdText, userID)
	if !result.Handled {
		return CommandOutcome{NotHandled: true}
	}
	ro := &ResponseOutcome{Result: result, LookupText: cmdText}
	d.setActivatedWizardStep(ro, sessionKey, genBefore)
	return CommandOutcome{Response: ro}
}

// setActivatedWizardStep records the wizard step a just-completed dispatch
// ACTIVATED for sessionKey: the scope's wizard generation changed across the
// dispatch and a wizard is still active (the same activation test the app's
// maybeStartWizard uses). A command that only ran while an older wizard was
// already active leaves the generation unchanged and gets no buttons.
func (d *Dispatcher) setActivatedWizardStep(ro *ResponseOutcome, sessionKey string, genBefore uint64) {
	if d.registry.WizardGen(sessionKey) == genBefore || !d.registry.WizardActive(sessionKey) {
		return
	}
	ro.WizardStep = wizardStepIfAny(d.registry, sessionKey)
}

// SessionKeyForChat resolves the stable session key for a chat ID — the ONE
// resolver shared by command activation (command.Request.SessionKey) and the
// wizard intercept (Interceptor.TryIntercept), so a wizard is always looked
// up under the key it was started with (#2277). Falls back to
// session.NewChatSessionKey when no resolver is wired.
func (d *Dispatcher) SessionKeyForChat(chatID int64) string {
	if d.sessionKeyFn != nil {
		return d.sessionKeyFn(chatID)
	}
	return session.NewChatSessionKey(d.agentID, chatID)
}

func extractArgs(text string) string {
	_, args, _ := strings.Cut(text, " ")
	return strings.TrimSpace(args)
}
