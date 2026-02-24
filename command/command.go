package command

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Command is a slash command that executes outside the agent pipeline.
type Command struct {
	Name           string
	Description    string
	Category       string // grouping for /help (e.g. "observability", "operations")
	Execute        func(ctx context.Context, args string) (string, error)
	SkipToolExport bool // if true, not exposed as an agent tool
	Hidden         bool // if true, excluded from /help and BotFather registration
}

// WizardHandler is implemented by interactive wizards that take over message routing.
// While a wizard is active, all messages are routed to Handle() instead of normal
// command dispatch or the agent queue.
type WizardHandler interface {
	Handle(text string) (response string, done bool)
}

// Registry holds registered slash commands and dispatches them.
type Registry struct {
	commands map[string]*Command
	wizard   WizardHandler
	wizardMu sync.Mutex
}

// NewRegistry creates an empty command registry.
func NewRegistry() *Registry {
	return &Registry{commands: make(map[string]*Command)}
}

// Register adds a command to the registry.
func (r *Registry) Register(cmd *Command) {
	r.commands[cmd.Name] = cmd
}

// Get returns a command by name, or nil.
func (r *Registry) Get(name string) *Command {
	return r.commands[name]
}

// All returns all commands sorted by name.
func (r *Registry) All() []*Command {
	cmds := make([]*Command, 0, len(r.commands))
	for _, c := range r.commands {
		cmds = append(cmds, c)
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name < cmds[j].Name
	})
	return cmds
}

// Dispatch parses a "/command args" string, looks up the command, and executes it.
// Returns (result, true) if the command was found and executed, or ("", false) if
// the text is not a recognized command.
func (r *Registry) Dispatch(ctx context.Context, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	// Parse "/command args"
	text = text[1:] // strip leading /
	name, args, _ := strings.Cut(text, " ")
	name = strings.ToLower(name)
	args = strings.TrimSpace(args)

	cmd := r.commands[name]
	if cmd == nil {
		return "", false
	}

	result, err := cmd.Execute(ctx, args)
	if err != nil {
		return "Error: " + err.Error(), true
	}
	return result, true
}

// SetWizard activates a wizard that intercepts all messages.
func (r *Registry) SetWizard(w WizardHandler) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	r.wizard = w
}

// ClearWizard removes the active wizard.
func (r *Registry) ClearWizard() {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	r.wizard = nil
}

// ChatIDKey is the context key for storing the Telegram chat ID.
// Used by commands that need to know which chat issued the command (e.g. /sessions info).
type ChatIDKey struct{}

// HandleMessage routes a message to the active wizard, if any.
// Returns (response, true) if the wizard handled the message, or ("", false)
// if no wizard is active. Handles /cancel and /stop to abort the wizard.
func (r *Registry) HandleMessage(text string) (string, bool) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()

	if r.wizard == nil {
		return "", false
	}

	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "/cancel" || lower == "/stop" {
		r.wizard = nil
		return "Wizard cancelled.", true
	}

	response, done := r.wizard.Handle(text)
	if done {
		r.wizard = nil
	}
	return response, true
}
