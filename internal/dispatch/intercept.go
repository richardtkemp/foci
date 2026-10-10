package dispatch

import (
	"context"
	"strings"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/question"
)

// StaleCommandAge is the maximum age of a slash command before it is dropped.
// Commands older than this are treated as replays (e.g. from Telegram's update
// queue after a foci restart) and silently discarded.
const StaleCommandAge = 30 * time.Second

// IsRoutableCommand reports whether text should be routed to the command
// channel for dispatch. Slash-prefixed text always routes (unknown commands
// produce a "Did you mean?" reply, which is intentional). Dot-prefixed text
// routes only if the command is registered — unknown ".something" must fall
// through to the agent as normal text so the dot-prefix alias doesn't eat
// phone-typed messages like ".sigh" or sentence fragments.
func IsRoutableCommand(text string, r *command.Registry) bool {
	if len(text) == 0 {
		return false
	}
	if text[0] == '/' {
		// A leading-slash filesystem path is not a command — don't divert it to
		// the command worker; let it fall through to the agent as normal text.
		// Shares isSlashPath with DispatchText so the routing gate and the
		// dispatcher can't drift apart (#770): if only one applied the guard, a
		// path would pass this gate, get declined by DispatchText as NotHandled,
		// and be silently dropped instead of reaching the agent.
		return !isSlashPath(text)
	}
	if text[0] == '.' && r != nil {
		return r.IsKnownCommand(text)
	}
	return false
}

// isSlashPath reports whether text is a leading-slash filesystem path rather
// than a slash command. A real command is a single token with no embedded
// slash ("/status"); a path has a further slash in its first token
// ("/home/foci/x:12 - error", "/etc/hosts"). Both the routing gate
// (IsRoutableCommand) and the dispatcher (DispatchText) call this so they
// agree — a path must reach the agent as normal text, never be swallowed as an
// unknown command or dropped in the gap between the two predicates (#770).
func isSlashPath(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, _, _ := strings.Cut(text[1:], " ")
	return strings.Contains(name, "/")
}

// Interceptor implements the shared message interception pipeline used by
// both Telegram and Discord bots. It handles wizard intercept, last-message
// recording, stale command drops, command dispatch, message transforms,
// and secondary bot idle drops.
type Interceptor struct {
	Commands     *command.Registry
	LastMsgStore *command.LastMessageStore
	Handler      platform.MessageHandler // for TransformMessage; may be nil
	Dispatcher   *Dispatcher
	IsSecondary  bool
	SessionKeyFn func() string // reports the bot's current session key; used ONLY by the idle-secondary drop below (empty = idle). Wizard scoping comes from Dispatcher.SessionKeyForChat.
	LogWarnf     func(string, ...any)
	LogDebugf    func(string, ...any)
}

// InterceptMessage holds the platform-neutral fields needed for interception.
type InterceptMessage struct {
	Text      string
	UserID    string
	ChatID    int64
	Timestamp time.Time // message creation time; zero skips staleness check
}

// InterceptResult describes what happened during message interception.
type InterceptResult struct {
	Consumed bool
	// If Consumed, at most one of these is set:
	WizardReply   string          // wizard handled it, send this reply
	WizardDocPath string          // optional file to send after WizardReply (e.g. a QR image), then remove
	Outcome       *CommandOutcome // command dispatched, render this
	// Consumed && WizardReply=="" && Outcome==nil → silently consumed (stale/idle drop)

	// WizardStep is the wizard's CURRENT structured step when the wizard is
	// still active after handling the message and the step has at least one
	// option. Platforms send WizardReply with wizard buttons (WizardButtons,
	// "wz:" callbacks) when it is set; nil means a plain-text reply.
	WizardStep *question.Question

	// Text is the final message text after any transforms have been applied.
	// Always set — either the original text or the transformed version.
	// When Consumed is false, callers should use this for downstream processing.
	Text string
}

// TryIntercept runs the shared interception pipeline.
// Returns an InterceptResult describing what happened. The caller is
// responsible for platform-specific rendering based on the result.
func (i *Interceptor) TryIntercept(ctx context.Context, msg *InterceptMessage) InterceptResult {
	// Wizard intercept — route the message to this session's active wizard (if
	// any) before normal dispatch. Wizards are scoped by session key: the scope
	// is the Dispatcher's SessionKeyForChat(msg.ChatID) for the INBOUND chat —
	// the same key command activations pass as req.SessionKey — so a chat only
	// ever advances the wizard it started, never another conversation's. With
	// no Dispatcher, no command can have started a wizard here, so the scope
	// stays "".
	if msg.Text != "" {
		scope := ""
		if i.Dispatcher != nil {
			scope = i.Dispatcher.SessionKeyForChat(msg.ChatID)
		}
		if result, docPath, ok := i.Commands.HandleMessage(scope, msg.Text); ok {
			return InterceptResult{Consumed: true, WizardReply: result, WizardDocPath: docPath, WizardStep: wizardStepIfAny(i.Commands, scope), Text: msg.Text}
		}
	}

	// Record non-command messages for /repeat command.
	if msg.Text != "" && !strings.HasPrefix(msg.Text, "/") {
		i.LastMsgStore.Record(msg.UserID, msg.Text)
	}

	// Drop stale slash commands (e.g. replayed from the event queue after a
	// restart). Agent messages are still delivered since the agent can reason
	// about timeliness, but slash commands execute unconditionally.
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") && !msg.Timestamp.IsZero() {
		if age := time.Since(msg.Timestamp); age > StaleCommandAge {
			i.LogWarnf("dropping stale command %q (age=%s)", strings.ToLower(msg.Text), age.Truncate(time.Second))
			return InterceptResult{Consumed: true, Text: msg.Text}
		}
	}

	// Try dispatching the original message as a command (slash or dot-prefix).
	if outcome := i.tryDispatch(ctx, msg); outcome != nil {
		return InterceptResult{Consumed: true, Outcome: outcome, Text: msg.Text}
	}

	// Apply message transforms to non-command messages.
	// Transforms rewrite the text unconditionally; if the result is itself
	// a command, dispatch it. Either way, the transformed text is carried
	// in the result for downstream processing.
	if i.Handler != nil {
		if transformed := i.Handler.TransformMessage(msg.Text); transformed != msg.Text {
			msg.Text = transformed
			if outcome := i.tryDispatch(ctx, msg); outcome != nil {
				return InterceptResult{Consumed: true, Outcome: outcome, Text: msg.Text}
			}
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	if i.IsSecondary && i.SessionKeyFn != nil && i.SessionKeyFn() == "" {
		i.LogDebugf("dropping message to idle secondary bot")
		return InterceptResult{Consumed: true, Text: msg.Text}
	}

	return InterceptResult{Text: msg.Text}
}

// tryDispatch attempts to dispatch text as a command via the Dispatcher.
// Returns a non-nil CommandOutcome if handled.
func (i *Interceptor) tryDispatch(ctx context.Context, msg *InterceptMessage) *CommandOutcome {
	if msg.Text == "" || i.Dispatcher == nil {
		return nil
	}
	outcome := i.Dispatcher.DispatchCommand(ctx, msg.Text, msg.ChatID, msg.UserID)
	if outcome.NotHandled {
		return nil
	}
	return &outcome
}

// wizardStepIfAny returns the scope's wizard's current structured step when it
// has at least one option (a button step), else nil. Shared by the intercept
// result and the dispatch outcome so both surfaces report the same rule —
// free-text steps and finished wizards never carry buttons.
func wizardStepIfAny(reg *command.Registry, scope string) *question.Question {
	if reg == nil {
		return nil
	}
	if q := reg.WizardPendingStep(scope); q != nil && len(q.Options) > 0 {
		return q
	}
	return nil
}

// IsWizardCancel reports whether text is a bare /cancel (any case, surrounding
// spaces ignored) from a chat whose scope has an active wizard. This is the
// one slash command platforms must route to the wizard intercept instead of
// the command channel: there is no /cancel command, so the command channel
// would answer "unknown command" and leave the wizard running. Every other
// command keeps today's routing. Nil-receiver safe.
func (d *Dispatcher) IsWizardCancel(chatID int64, text string) bool {
	if d == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(text), "/cancel") {
		return false
	}
	return d.registry.WizardActive(d.SessionKeyForChat(chatID))
}
