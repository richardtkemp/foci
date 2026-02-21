package command

import (
	"context"
	"sort"
	"strings"
)

// Command is a slash command that executes outside the agent pipeline.
type Command struct {
	Name           string
	Description    string
	Execute        func(ctx context.Context, args string) (string, error)
	SkipToolExport bool // if true, not exposed as an agent tool
}

// Registry holds registered slash commands and dispatches them.
type Registry struct {
	commands map[string]*Command
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
