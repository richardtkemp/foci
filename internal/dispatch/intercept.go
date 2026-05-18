package dispatch

import (
	"context"
	"strings"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
)

// StaleCommandAge is the maximum age of a slash command before it is dropped.
// Commands older than this are treated as replays (e.g. from Telegram's update
// queue after a foci restart) and silently discarded.
const StaleCommandAge = 30 * time.Second

// IsCommandText reports whether text begins with a command prefix (/ or .).
// This is a syntactic check only — it does not consult the registry. Use
// IsRoutableCommand when deciding whether to route to the command channel.
func IsCommandText(text string) bool {
	return len(text) > 0 && (text[0] == '/' || text[0] == '.')
}

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
		return true
	}
	if text[0] == '.' && r != nil {
		return r.IsKnownCommand(text)
	}
	return false
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
	SessionKeyFn func() string     // returns current session key; empty for idle secondary bots
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
	WizardReply string          // wizard handled it, send this reply
	Outcome     *CommandOutcome // command dispatched, render this
	// Consumed && WizardReply=="" && Outcome==nil → silently consumed (stale/idle drop)

	// Text is the final message text after any transforms have been applied.
	// Always set — either the original text or the transformed version.
	// When Consumed is false, callers should use this for downstream processing.
	Text string
}

// TryIntercept runs the shared interception pipeline.
// Returns an InterceptResult describing what happened. The caller is
// responsible for platform-specific rendering based on the result.
func (i *Interceptor) TryIntercept(ctx context.Context, msg *InterceptMessage) InterceptResult {
	// Wizard intercept — route all messages to active wizard before normal dispatch.
	if msg.Text != "" {
		if result, ok := i.Commands.HandleMessage(msg.Text); ok {
			return InterceptResult{Consumed: true, WizardReply: result, Text: msg.Text}
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
