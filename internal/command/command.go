package command

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"foci/internal/platform"
	"foci/internal/session"
)

// KeyboardOption is an alias for platform.ButtonChoice.
// Command keyboards use the same ButtonChoice type that all button rendering uses.
type KeyboardOption = platform.ButtonChoice

// Subcommand declares a named subcommand within a parent command.
// When Command.Subcommands is populated, Execute and KeyboardOptions
// are auto-wired from the subcommand list by Register.
type Subcommand struct {
	Name        string
	Label       string   // keyboard button label (defaults to Name)
	Aliases     []string // accepted in dispatch, not shown in keyboard
	Description string   // one-line help text for auto-generated usage
	Hidden      bool     // dispatched but excluded from keyboard (e.g. needs args)
	// Immediate marks subcommands that must run inline in the polling
	// goroutine rather than being deferred to the worker (mirrors
	// Command.Immediate but at subcommand granularity). Set this on any
	// subcommand that cancels or interrupts an active agent turn (e.g.
	// /reset hard) — the worker is blocked while a turn is running and
	// cannot process deferred work.
	Immediate bool
	Visible   func(ctx context.Context, cc CommandContext) bool
	Execute   func(ctx context.Context, req Request, cc CommandContext) (Response, error)
}

// Requires declares what transport a command needs to function.
// Checked at dispatch time and help rendering time.
type Requires int

const (
	// RequiresNothing means the command works regardless of transport
	// (foci-internal, or the Agent routes internally).
	RequiresNothing Requires = iota

	// RequiresBackend means the command only works with a delegated backend
	// (e.g. ccstream). Dispatch rejects with a clear error if no backend.
	RequiresBackend

	// RequiresAPI means the command only works with the API transport.
	RequiresAPI
)

// Command is a slash command that executes outside the agent pipeline.
type Command struct {
	Name        string
	Aliases     []string // accepted in dispatch, not shown in keyboard
	Description string
	Category    string
	Requires    Requires // transport requirement — checked before Execute
	Hidden      bool
	// ExcludeApp hides the command from the app client's command palette.
	// Used for commands whose functionality is surfaced natively in the app
	// UI (e.g. /pause, /resume, /complete are Telegram/Discord alternatives
	// to the app's own ask buttons).
	ExcludeApp bool
	// Immediate marks commands that must run in the polling goroutine rather
	// than being deferred to the worker goroutine. Set this on any command
	// that cancels or interrupts an active agent turn (e.g. /stop), since the
	// worker is blocked while a turn is running and cannot process deferred work.
	Immediate bool
	Visible   func(ctx context.Context, req Request, cc CommandContext) bool // when non-nil and returns false, suppressed from listings/keyboards

	// Subcommands declares the command's subcommand set. When non-empty and
	// Execute is nil, Register auto-wires Execute and (if nil) KeyboardOptions
	// from this list.
	Subcommands []Subcommand

	Execute func(ctx context.Context, req Request, cc CommandContext) (Response, error)

	// DefaultExecute is called when args are non-empty but no subcommand matches.
	// When nil, auto-generated usage is shown.
	DefaultExecute func(ctx context.Context, req Request, cc CommandContext) (Response, error)

	KeyboardOptions func(ctx context.Context, cc CommandContext) []KeyboardOption
	KeyboardHeader  func(ctx context.Context, req Request, cc CommandContext) string // text shown above keyboard (e.g. current value)
	ChainKeyboard   func(ctx context.Context, subcommand string, cc CommandContext) []KeyboardOption
}

// Registry holds registered slash commands and dispatches them. The wizard
// fields back the per-session interactive-wizard machinery in wizard.go.
type Registry struct {
	commands map[string]*Command

	wizardMu    sync.Mutex
	wizards     map[string]*wizardEntry // scope (session key) → active wizard
	wizardGen   uint64                  // monotonic; mints wizardEntry.gen
	wizardStore *session.SessionIndex   // nil = no persistence
	wizardAgent string                  // agent_metadata owner for persistence
}

// NewRegistry creates an empty command registry.
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]*Command),
		wizards:  make(map[string]*wizardEntry),
	}
}

// Register adds a command to the registry. When cmd.Subcommands is non-empty
// and cmd.Execute is nil, it auto-wires Execute (and KeyboardOptions if nil)
// from the subcommand declarations.
func (r *Registry) Register(cmd *Command) {
	if len(cmd.Subcommands) > 0 && cmd.Execute == nil {
		cmd.buildSubcommandDispatch()
	}
	r.commands[cmd.Name] = cmd
	for _, alias := range cmd.Aliases {
		r.commands[alias] = cmd
	}
}

// buildSubcommandDispatch wires Execute and KeyboardOptions from Subcommands.
func (cmd *Command) buildSubcommandDispatch() {
	// Build name → subcommand lookup (including aliases).
	lookup := make(map[string]*Subcommand, len(cmd.Subcommands)*2)
	for i := range cmd.Subcommands {
		sub := &cmd.Subcommands[i]
		lookup[sub.Name] = sub
		for _, alias := range sub.Aliases {
			lookup[alias] = sub
		}
	}

	usage := cmd.buildSubcommandUsage()

	cmd.Execute = func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
		parts := strings.Fields(req.Args)
		if len(parts) == 0 {
			if cmd.DefaultExecute != nil {
				return cmd.DefaultExecute(ctx, req, cc)
			}
			return Response{Text: usage}, nil
		}
		name := strings.ToLower(parts[0])
		sub, ok := lookup[name]
		if !ok {
			if cmd.DefaultExecute != nil {
				return cmd.DefaultExecute(ctx, req, cc)
			}
			return Response{Text: usage}, nil
		}
		subReq := req
		subReq.Args = strings.TrimSpace(strings.TrimPrefix(req.Args, parts[0]))
		return sub.Execute(ctx, subReq, cc)
	}

	if cmd.KeyboardOptions == nil {
		cmd.KeyboardOptions = func(ctx context.Context, cc CommandContext) []KeyboardOption {
			var opts []KeyboardOption
			for _, sub := range cmd.Subcommands {
				if sub.Hidden {
					continue
				}
				if sub.Visible != nil && !sub.Visible(ctx, cc) {
					continue
				}
				label := sub.Label
				if label == "" {
					label = sub.Name
				}
				opts = append(opts, KeyboardOption{Label: label, Data: sub.Name})
			}
			return opts
		}
	}
}

// buildSubcommandUsage generates a usage string from the subcommand list.
func (cmd *Command) buildSubcommandUsage() string {
	var names []string
	for _, sub := range cmd.Subcommands {
		names = append(names, sub.Name)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Usage: /%s [%s]", cmd.Name, strings.Join(names, "|"))

	hasDesc := false
	for _, sub := range cmd.Subcommands {
		if sub.Description != "" {
			hasDesc = true
			break
		}
	}
	if hasDesc {
		sb.WriteString("\n")
		maxLen := 0
		for _, sub := range cmd.Subcommands {
			if len(sub.Name) > maxLen {
				maxLen = len(sub.Name)
			}
		}
		for _, sub := range cmd.Subcommands {
			if sub.Description != "" {
				fmt.Fprintf(&sb, "\n  %-*s  %s", maxLen, sub.Name, sub.Description)
			}
		}
	}

	return sb.String()
}

// Get returns a command by name, or nil.
func (r *Registry) Get(name string) *Command {
	return r.commands[name]
}

// IsImmediateText reports whether the command named in text has Immediate set.
// Used by platform receive loops to decide whether to dispatch a command in the
// polling goroutine (immediate) or defer it to the worker goroutine (non-immediate).
//
// Checks the parent command's Immediate flag first; if false, also checks
// whether the first arg matches an Immediate subcommand. This lets a parent
// command stay non-Immediate (so its default path doesn't tie up the polling
// goroutine) while still dispatching specific subcommands (e.g. /reset hard)
// inline.
func (r *Registry) IsImmediateText(text string) bool {
	name, args := commandNameAndArgsFromText(text)
	if name == "" {
		return false
	}
	cmd, ok := r.commands[name]
	if !ok {
		return false
	}
	if cmd.Immediate {
		return true
	}
	if args == "" || len(cmd.Subcommands) == 0 {
		return false
	}
	first := strings.ToLower(strings.Fields(args)[0])
	for i := range cmd.Subcommands {
		sub := &cmd.Subcommands[i]
		if sub.Name == first {
			return sub.Immediate
		}
		for _, alias := range sub.Aliases {
			if alias == first {
				return sub.Immediate
			}
		}
	}
	return false
}

// IsKnownCommand reports whether text is a slash- or dot-prefix command
// that maps to a command in this registry. Used by routing code to decide
// whether to send a message through the command channel.
func (r *Registry) IsKnownCommand(text string) bool {
	name, _ := commandNameAndArgsFromText(text)
	if name == "" {
		return false
	}
	_, ok := r.commands[name]
	return ok
}

// commandNameAndArgsFromText extracts both the lower-cased command name and the
// trimmed args. Returns ("", "") if text is not a command.
func commandNameAndArgsFromText(text string) (string, string) {
	text = strings.TrimSpace(text)
	if len(text) == 0 || (text[0] != '/' && text[0] != '.') {
		return "", ""
	}
	name, args, _ := strings.Cut(text[1:], " ")
	return strings.ToLower(name), strings.TrimSpace(args)
}

// All returns every distinct registered command, sorted by name. r.commands
// maps EVERY dispatch key (a command's Name and each of its Aliases) to the
// same *Command pointer, so a naive range over the map yields that pointer
// once per key — a command with two aliases would otherwise appear three
// times in /help and the app command palette (both of which enumerate via
// All()). Deduplicated by pointer identity so an aliased command surfaces
// exactly once, under its canonical Name.
func (r *Registry) All() []*Command {
	seen := make(map[*Command]bool, len(r.commands))
	cmds := make([]*Command, 0, len(r.commands))
	for _, c := range r.commands {
		if seen[c] {
			continue
		}
		seen[c] = true
		cmds = append(cmds, c)
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name < cmds[j].Name
	})
	return cmds
}

// VisibleList returns the app-facing command palette filtered for a specific
// session: non-hidden, non-ExcludeApp commands whose Visible func (if set)
// evaluates true against the session's context. Each result carries name,
// description and category — the same fields as CommandInfo.
func (r *Registry) VisibleList(ctx context.Context, req Request, cc CommandContext) []CommandInfo {
	var out []CommandInfo
	for _, c := range r.All() {
		if c.Hidden || c.ExcludeApp {
			continue
		}
		if c.Visible != nil && !c.Visible(ctx, req, cc) {
			continue
		}
		out = append(out, CommandInfo{
			Name:        c.Name,
			Description: c.Description,
			Category:    c.Category,
		})
	}
	return out
}

// CommandInfo is the app-facing representation of a slash command.
type CommandInfo struct {
	Name        string
	Description string
	Category    string
}

// LookupKeyboard checks if a bare command (no args) has inline keyboard options.
// Returns (command_name, header, options, true) if a keyboard should be shown,
// or ("", "", nil, false) otherwise. The header is contextual text to display above
// the keyboard (e.g. current value); it defaults to "/<name>:" when KeyboardHeader is nil.
func (r *Registry) LookupKeyboard(ctx context.Context, text string, cc CommandContext) (string, string, []KeyboardOption, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", nil, false
	}

	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(name)
	args = strings.TrimSpace(args)

	if args != "" {
		return "", "", nil, false
	}

	cmd := r.commands[name]
	if cmd == nil || cmd.KeyboardOptions == nil {
		return "", "", nil, false
	}
	req := Request{Name: name}
	if cmd.Visible != nil && !cmd.Visible(ctx, req, cc) {
		return "", "", nil, false
	}

	opts := cmd.KeyboardOptions(ctx, cc)
	if len(opts) == 0 {
		return "", "", nil, false
	}

	header := fmt.Sprintf("/%s:", name)
	if cmd.KeyboardHeader != nil {
		if h := cmd.KeyboardHeader(ctx, req, cc); h != "" {
			header = h
		}
	}

	return name, header, opts, true
}

// LookupChainKeyboard checks if a command callback text (e.g. "/tmux kill") needs
// a follow-up keyboard to select a parameter. The full args string is passed to
// ChainKeyboard, which decides whether to chain at any depth. Returns
// (command_name, options, true) if chaining should occur, or ("", nil, false) otherwise.
func (r *Registry) LookupChainKeyboard(ctx context.Context, text string, cc CommandContext) (string, []KeyboardOption, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}

	stripped := text[1:]
	name, args, _ := strings.Cut(stripped, " ")
	name = strings.ToLower(name)
	args = strings.TrimSpace(args)

	if args == "" {
		return "", nil, false
	}

	cmd := r.commands[name]
	if cmd == nil || cmd.ChainKeyboard == nil {
		return "", nil, false
	}

	opts := cmd.ChainKeyboard(ctx, args, cc)
	if len(opts) == 0 {
		return "", nil, false
	}

	return name, opts, true
}

// Dispatch executes a command using the platform-agnostic Request/Response pattern.
// Returns (Response, true) if the command was found, or (Response{}, false) if not.
func (r *Registry) Dispatch(ctx context.Context, req Request, cc CommandContext) (Response, bool, error) {
	cmd := r.commands[req.Name]
	if cmd == nil {
		return Response{Text: r.suggestCommand(req.Name)}, true, nil
	}

	// Gate on transport requirements before executing.
	if msg := checkRequires(cmd, cc); msg != "" {
		return Response{Text: msg}, true, nil
	}

	if cmd.Execute != nil {
		resp, err := cmd.Execute(ctx, req, cc)
		if err != nil {
			return Response{Text: "Error: " + err.Error()}, true, nil
		}
		return resp, true, nil
	}

	return Response{}, false, fmt.Errorf("command /%s has no Execute function", req.Name)
}

// checkRequires validates a command's transport requirement against the
// current agent configuration. Returns an error message if the requirement
// is not met, or empty string if OK.
func checkRequires(cmd *Command, cc CommandContext) string {
	switch cmd.Requires {
	case RequiresBackend:
		if cc.Agent == nil || cc.Agent.DelegatedManager == nil {
			return fmt.Sprintf("/%s requires a Claude Code backend", cmd.Name)
		}
	case RequiresAPI:
		if cc.Agent != nil && cc.Agent.DelegatedManager != nil {
			return fmt.Sprintf("/%s requires API transport (not available in delegated mode)", cmd.Name)
		}
	}
	return ""
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
