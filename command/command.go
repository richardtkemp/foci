package command

import (
	"context"
	"fmt"
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
		return r.suggestCommand(name), true
	}

	result, err := cmd.Execute(ctx, args)
	if err != nil {
		return "Error: " + err.Error(), true
	}
	return result, true
}

// suggestCommand returns a helpful message when a command isn't found,
// with "did you mean?" suggestions based on edit distance.
func (r *Registry) suggestCommand(name string) string {
	type match struct {
		name string
		dist int
	}
	var matches []match
	for cmdName := range r.commands {
		d := levenshtein(name, cmdName)
		if d <= 2 || (len(name) >= 3 && strings.HasPrefix(cmdName, name[:3])) {
			matches = append(matches, match{cmdName, d})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].dist < matches[j].dist })

	if len(matches) > 0 {
		// Show up to 3 suggestions
		limit := len(matches)
		if limit > 3 {
			limit = 3
		}
		var suggestions []string
		for _, m := range matches[:limit] {
			suggestions = append(suggestions, "/"+m.name)
		}
		return fmt.Sprintf("Unknown command /%s. Did you mean %s?", name, strings.Join(suggestions, ", "))
	}
	return fmt.Sprintf("Unknown command /%s. Type /help to see available commands.", name)
}

// levenshtein returns the edit distance between two strings.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
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
