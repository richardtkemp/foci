package dispatch

import (
	"context"
	"strings"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
)

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
}

// TryIntercept runs the shared interception pipeline.
// Returns an InterceptResult describing what happened. The caller is
// responsible for platform-specific rendering based on the result.
func (i *Interceptor) TryIntercept(ctx context.Context, msg *InterceptMessage) InterceptResult {
	// Wizard intercept — route all messages to active wizard before normal dispatch.
	if msg.Text != "" {
		if result, ok := i.Commands.HandleMessage(msg.Text); ok {
			return InterceptResult{Consumed: true, WizardReply: result}
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
		if age := time.Since(msg.Timestamp); age > 30*time.Second {
			i.LogWarnf("dropping stale command %q (age=%s)", strings.ToLower(msg.Text), age.Truncate(time.Second))
			return InterceptResult{Consumed: true}
		}
	}

	// Try dispatching the original message as a command (slash or dot-prefix).
	if outcome := i.tryDispatch(ctx, msg); outcome != nil {
		return InterceptResult{Consumed: true, Outcome: outcome}
	}

	// Apply message transforms to non-command messages.
	// Transforms may produce a command (e.g. "m" → "/mana").
	if i.Handler != nil {
		if transformed := i.Handler.TransformMessage(msg.Text); transformed != msg.Text {
			msg.Text = transformed
			if outcome := i.tryDispatch(ctx, msg); outcome != nil {
				return InterceptResult{Consumed: true, Outcome: outcome}
			}
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	if i.IsSecondary && i.SessionKeyFn != nil && i.SessionKeyFn() == "" {
		i.LogDebugf("dropping message to idle secondary bot")
		return InterceptResult{Consumed: true}
	}

	return InterceptResult{}
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
