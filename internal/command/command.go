package command

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// KeyboardOption represents a button in an inline keyboard for a command.
type KeyboardOption struct {
	Label string // Button text shown to user
	Data  string // Callback data suffix (appended to "cmd:/name ")
	Row   int    // Which row this button goes in (0-indexed)
}

// Command is a slash command that executes outside the agent pipeline.
type Command struct {
	Name        string
	Description string
	Category    string
	Hidden      bool

	Execute func(ctx context.Context, args string) (string, error)

	ExecuteV2 func(ctx context.Context, req Request, deps Deps) (Response, error)

	KeyboardOptions func(ctx context.Context) []KeyboardOption
	ChainKeyboard   func(ctx context.Context, subcommand string) []KeyboardOption
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

// LookupKeyboard checks if a bare command (no args) has inline keyboard options.
// Returns (command_name, options, true) if a keyboard should be shown, or ("", nil, false) otherwise.
func (r *Registry) LookupKeyboard(ctx context.Context, text string) (string, []KeyboardOption, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}

	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(name)
	args = strings.TrimSpace(args)

	if args != "" {
		return "", nil, false
	}

	cmd := r.commands[name]
	if cmd == nil || cmd.KeyboardOptions == nil {
		return "", nil, false
	}

	opts := cmd.KeyboardOptions(ctx)
	if len(opts) == 0 {
		return "", nil, false
	}

	return name, opts, true
}

// LookupChainKeyboard checks if a command callback text (e.g. "/tmux kill") needs
// a second keyboard to select a parameter. Returns (command_name, options, true)
// if chaining should occur, or ("", nil, false) otherwise.
func (r *Registry) LookupChainKeyboard(ctx context.Context, text string) (string, []KeyboardOption, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}

	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(name)
	args = strings.TrimSpace(args)

	// Chain only fires for a bare subcommand (exactly one word, no further args)
	if args == "" {
		return "", nil, false
	}
	sub, extra, _ := strings.Cut(args, " ")
	if strings.TrimSpace(extra) != "" {
		return "", nil, false // already has full args
	}

	cmd := r.commands[name]
	if cmd == nil || cmd.ChainKeyboard == nil {
		return "", nil, false
	}

	opts := cmd.ChainKeyboard(ctx, sub)
	if len(opts) == 0 {
		return "", nil, false
	}

	return name, opts, true
}

// Dispatch parses a "/command args" string, looks up the command, and executes it.
// Returns (result, true) if the command was found and executed, or ("", false) if
// the text is not a recognized command.
func (r *Registry) Dispatch(ctx context.Context, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	text = text[1:]
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

// DispatchV2 executes a command using the platform-agnostic Request/Response pattern.
// Returns (Response, true) if the command was found, or (Response{}, false) if not.
func (r *Registry) DispatchV2(ctx context.Context, req Request, deps Deps) (Response, bool, error) {
	cmd := r.commands[req.Name]
	if cmd == nil {
		return Response{Text: r.suggestCommand(req.Name)}, true, nil
	}

	if cmd.ExecuteV2 != nil {
		resp, err := cmd.ExecuteV2(ctx, req, deps)
		if err != nil {
			return Response{Text: "Error: " + err.Error()}, true, nil
		}
		return resp, true, nil
	}

	if cmd.Execute != nil {
		result, err := cmd.Execute(ctx, req.Args)
		if err != nil {
			return Response{Text: "Error: " + err.Error()}, true, nil
		}
		return Response{Text: result}, true, nil
	}

	return Response{}, false, fmt.Errorf("command /%s has no Execute or ExecuteV2 function", req.Name)
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

// ChatIDKey is the context key for storing the platform chat ID.
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
