package suggestion

import (
	"fmt"
	"sync"
)

// Action is one command button on a server-originated suggestion: Label is what
// the user sees, Command is the slash-command line the button runs through
// the agent's command registry when pressed (e.g. "/sr on"). The command is
// read only from the registered Spec at press time — never from the press
// data — so nothing a user or agent types can widen what a button runs.
type Action struct {
	Label   string
	Command string
}

// Spec is one registered suggestion: the message text and the up-to-four command
// buttons it carries, plus the two built-ins every suggestion appends (Not now,
// Stop showing me this). See Sender.
type Spec struct {
	ID      string
	Text    string
	Actions []Action
}

// Registry validation limits. maxSpecActions caps how many command buttons
// one suggestion may carry (the two built-ins are appended on top); maxSpecIDLen
// bounds the id that goes into interactive-message ids and press routing.
const (
	maxSpecActions = 4
	maxSpecIDLen   = 64
)

// Registry holds the fixed set of suggestion Specs gateway Go code registered at
// startup. Specs enter ONLY through Register — there is no config key, tool,
// file or message-text path that can add or alter one, by design: only
// server-originated system messages may carry command buttons, so a
// prompt-injected agent must not be able to mint a one-tap command (Dick's
// 2026-10-10 ruling, #1321).
type Registry struct {
	mu    sync.RWMutex
	specs map[string]Spec
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{specs: make(map[string]Spec)}
}

// Register validates s and stores it. It rejects an id that is empty, longer
// than 64 characters or outside [a-z0-9._-]; a duplicate id; an empty Text (a
// suggestion with no body is unusable as a suggestion — beyond the ticket's list, kept
// deliberately); more than 4 actions; an action with an empty Label or an
// empty Command; and a command that does not start with "/". The stored copy
// is a deep clone, so later mutation of the caller's Spec cannot alter what
// the registry serves.
func (r *Registry) Register(s Spec) error {
	if !validSpecID(s.ID) {
		return fmt.Errorf("suggestion: invalid spec id %q: want 1-%d characters of [a-z0-9._-]", s.ID, maxSpecIDLen)
	}
	if s.Text == "" {
		return fmt.Errorf("suggestion %q: empty text", s.ID)
	}
	if len(s.Actions) > maxSpecActions {
		return fmt.Errorf("suggestion %q: %d actions, max %d", s.ID, len(s.Actions), maxSpecActions)
	}
	for i, a := range s.Actions {
		if a.Label == "" {
			return fmt.Errorf("suggestion %q: action %d has an empty label", s.ID, i)
		}
		if a.Command == "" {
			return fmt.Errorf("suggestion %q: action %q has an empty command", s.ID, a.Label)
		}
		if a.Command[0] != '/' {
			return fmt.Errorf("suggestion %q: action %q command %q must start with '/'", s.ID, a.Label, a.Command)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.specs[s.ID]; dup {
		return fmt.Errorf("suggestion: duplicate spec id %q", s.ID)
	}
	r.specs[s.ID] = cloneSpec(s)
	return nil
}

// Get returns the Spec registered under id. The returned value is a deep
// clone, so no caller can mutate the registry's copy through a handle.
func (r *Registry) Get(id string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[id]
	if !ok {
		return Spec{}, false
	}
	return cloneSpec(s), true
}

// validSpecID reports whether id is 1–64 characters of [a-z0-9._-].
func validSpecID(id string) bool {
	if len(id) == 0 || len(id) > maxSpecIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// cloneSpec deep-copies a Spec, Actions included, so a stored or served Spec
// shares no slice with any caller's copy (F3: the one deep-copy helper, used
// by both Register and Get).
func cloneSpec(s Spec) Spec {
	out := Spec{ID: s.ID, Text: s.Text}
	if len(s.Actions) > 0 {
		out.Actions = make([]Action, len(s.Actions))
		copy(out.Actions, s.Actions)
	}
	return out
}
