package nudge

import (
	"context"
	"fmt"
	"strconv"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

// Result is what one Send call did.
type Result string

const (
	// ResultSent: the nudge was posted (a text-only connection counts — it
	// got the numbered fallback).
	ResultSent Result = "sent"
	// ResultMuted: the chat has muted this nudge id; nothing was posted.
	ResultMuted Result = "muted"
	// ResultNoConnection: no live connection for the session's chat.
	ResultNoConnection Result = "no_connection"
)

// Built-in button labels. Every nudge appends these two after its own
// actions: Not now just closes the message, Stop showing me this mutes the
// nudge for the chat.
const (
	NotNowLabel = "Not now"
	MuteLabel   = "Stop showing me this"
)

// Press outcome texts (what the nudge message is edited to).
const (
	notNowText          = "OK."
	mutedText           = "Muted. You will not see this again."
	doneText            = "Done"
	commandUnavailable  = "That command is not available."
	muteStoreFailedText = "Could not save that preference — please try again."
)

// CommandDispatcher runs one slash-command line through the agent's command
// registry on a session key, returning the command's reply rendered for a
// single message and whether it executed. It is injected because nudge
// cannot import the command package (command → agent → nudge would cycle);
// the gateway wires it to the same dispatch core /command and the deferred
// sweep use (runAgentCommand in cmd/foci-gw), so a button press runs
// exactly what typing the command runs. The reply text must be the response
// rendered as one message (command.Response.DisplayText: Parts joined by a
// blank line, else Text) — the same rendering every platform command-reply
// path shows, so a command that answers in Parts is not flattened away.
type CommandDispatcher func(ctx context.Context, sessionKey, commandLine string) (text string, ok bool)

// SenderDeps are the collaborators a nudge Sender needs.
type SenderDeps struct {
	// Registry supplies the Specs (server-originated only; see Registry).
	Registry *Registry
	// Index persists per-chat mutes.
	Index *session.SessionIndex
	// ConnFor resolves the live connection for a nudge's session at send
	// time; the returned resolver is re-invoked on later proactive edits.
	ConnFor func(agentID, sessionKey string) platform.ConnResolver
	// Dispatch runs an action's command (see CommandDispatcher).
	Dispatch CommandDispatcher
}

// Sender posts server-originated nudges: one interactive message carrying the
// Spec's text and one button per action plus the two built-ins, and handles
// the presses. A nudge is a message to the session's own user, so the
// interactive message goes through the session-chat resolver the caller
// supplies (the same rule as the ask tool, #2275).
type Sender struct {
	deps  SenderDeps
	mutes *muteStore
}

// NewSender builds a Sender. Every dep is required: a nil Registry, Index,
// ConnFor or Dispatch is rejected here, at construction — the wiring is
// gateway startup code, and a missing collaborator must fail the startup
// rather than surface as a nil dereference inside a platform button-press
// callback.
func NewSender(deps SenderDeps) (*Sender, error) {
	for _, dep := range []struct {
		name string
		bad  bool
	}{
		{"Registry", deps.Registry == nil},
		{"Index", deps.Index == nil},
		{"ConnFor", deps.ConnFor == nil},
		{"Dispatch", deps.Dispatch == nil},
	} {
		if dep.bad {
			return nil, fmt.Errorf("nudge: NewSender requires a non-nil %s", dep.name)
		}
	}
return &Sender{deps: deps, mutes: newMuteStore(deps.Index)}, nil
}

// Send posts the nudge nudgeID to the chat owning sessionKey.
//
// Unknown nudge id, a session key that does not name agentID's own numeric
// chat (another agent's key, a non-chat type, or a chat id that is not a
// non-zero number — the per-chat mute unit must exist and be unambiguous
// before anything is keyed on it), an unreadable mute store or a failed
// send return an error. ResultMuted returns without posting;
// ResultNoConnection returns without posting when no live connection
// resolves. A connection without buttons (platform.ButtonSender
// unimplemented) still counts as Sent: the interactive layer falls back to
// numbered plain text, which registers no callback and therefore offers no
// execution path — a typed reply cannot run a command.
func (s *Sender) Send(ctx context.Context, agentID, sessionKey, nudgeID string) (Result, error) {
	spec, ok := s.deps.Registry.Get(nudgeID)
	if !ok {
		return "", fmt.Errorf("nudge: unknown id %q", nudgeID)
	}
	sk, err := session.ParseSessionKey(sessionKey)
	if err != nil {
		return "", fmt.Errorf("nudge %q: bad session key %q: %w", nudgeID, sessionKey, err)
	}
	if sk.AgentID != agentID {
		return "", fmt.Errorf("nudge %q: session key %q belongs to agent %q, not %q", nudgeID, sessionKey, sk.AgentID, agentID)
	}
	if sk.Type != 'c' {
		return "", fmt.Errorf("nudge %q requires a chat session, got %q", nudgeID, sessionKey)
	}
	// Fail closed on the chat id itself: SessionKey.ChatID silently maps a
	// non-numeric id to 0, and no platform mints chat 0 (the app remaps a 0
	// hash to 1), so either would key the mute on the shared fallback
	// bucket every malformed key lands in.
	chatID, err := strconv.ParseInt(sk.ID, 10, 64)
	if err != nil || chatID == 0 {
		return "", fmt.Errorf("nudge %q: session key %q has no usable chat id", nudgeID, sessionKey)
	}
	muted, err := s.mutes.muted(agentID, chatID, nudgeID)
	if err != nil {
		return "", fmt.Errorf("nudge %q: read mute for chat %d: %w", nudgeID, chatID, err)
	}
	if muted {
		return ResultMuted, nil
	}
	resolve := s.deps.ConnFor(agentID, sessionKey)
	if resolve == nil || resolve() == nil {
		return ResultNoConnection, nil
	}
	id := "nudge-" + nudgeID + "-" + fap.NewULID()
	_, err = platform.SendInteractiveMessageWithID(resolve, id, "", spec.Text,
		nudgeButtons(spec), s.pressCallback(spec, agentID, sessionKey, chatID), nil)
	if err != nil {
		return "", err
	}
	return ResultSent, nil
}

// nudgeButtons builds the button row: one per action, then the two built-ins.
// Each button's Data is ONLY its index — the command never travels in the
// press data; the callback resolves the index back through the registered
// Spec (see pressCallback).
func nudgeButtons(spec Spec) []platform.ButtonChoice {
	buttons := make([]platform.ButtonChoice, 0, len(spec.Actions)+2)
	for i, a := range spec.Actions {
		buttons = append(buttons, platform.ButtonChoice{Label: a.Label, Data: strconv.Itoa(i)})
	}
	buttons = append(buttons,
		platform.ButtonChoice{Label: NotNowLabel, Data: strconv.Itoa(len(spec.Actions))},
		platform.ButtonChoice{Label: MuteLabel, Data: strconv.Itoa(len(spec.Actions) + 1)},
	)
	return buttons
}

// pressCallback returns the interactive-layer callback for one sent nudge.
// choice.Data is the pressed button's index; anything unparseable edits
// nothing and runs nothing.
func (s *Sender) pressCallback(spec Spec, agentID, sessionKey string, chatID int64) platform.ButtonCallback {
	return func(choice platform.ButtonChoice) string {
		idx, err := strconv.Atoi(choice.Data)
		if err != nil {
			return ""
		}
		return s.handlePress(spec, agentID, sessionKey, chatID, idx)
	}
}

// handlePress resolves one button press (by index) to the message's edit
// text. Action buttons dispatch their Spec command through the injected
// dispatcher on the nudge's session key; Not now closes without side
// effects; the mute button stores the per-chat mute. Any other index —
// unreachable through the real router, which rejects out-of-range presses —
// runs nothing.
func (s *Sender) handlePress(spec Spec, agentID, sessionKey string, chatID int64, buttonIdx int) string {
	switch {
	case buttonIdx >= 0 && buttonIdx < len(spec.Actions):
		text, ok := s.deps.Dispatch(context.Background(), sessionKey, spec.Actions[buttonIdx].Command)
		if !ok {
			return commandUnavailable
		}
		if text == "" {
			return doneText
		}
		return text
	case buttonIdx == len(spec.Actions):
		return notNowText
	case buttonIdx == len(spec.Actions)+1:
		if err := s.mutes.mute(agentID, chatID, spec.ID); err != nil {
			nudgeLog.Warnf("nudge %s: store mute for chat %d: %v", spec.ID, chatID, err)
			return muteStoreFailedText
		}
		return mutedText
	default:
		return ""
	}
}
