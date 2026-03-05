package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/internal/table"
	"foci/internal/tools"
)
// displayWidth extracts the display width from context, returning 0 if unset.
func displayWidth(ctx context.Context) int {
	w, _ := ctx.Value(DisplayWidthKey{}).(int)
	return w
}

// ChildSysProcAttr is called to get the SysProcAttr for child processes.
// Set this from main to drop supplementary groups (foci-secrets).
// If nil, defaults to {Setpgid: true}.
var ChildSysProcAttr func() *syscall.SysProcAttr

func childSysProcAttr() *syscall.SysProcAttr {
	if ChildSysProcAttr != nil {
		return ChildSysProcAttr()
	}
	return &syscall.SysProcAttr{Setpgid: true}
}


type LastMessageStore struct {
	mu       sync.RWMutex
	messages map[string]string // userID → last message text
}

// NewLastMessageStore creates a new store for tracking last messages.
func NewLastMessageStore() *LastMessageStore {
	return &LastMessageStore{
		messages: make(map[string]string),
	}
}

// Record stores the last message from a user.
func (s *LastMessageStore) Record(userID string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[userID] = message
}

// Get retrieves the last message from a user, or "" if not found.
func (s *LastMessageStore) Get(userID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.messages[userID]
}

// LastMessageUserKey is the context key for storing the user ID.
type LastMessageUserKey struct{}

// NewRepeatCommand creates the // command that repeats the last message.
// Expects userID to be stored in context via context.WithValue(ctx, LastMessageUserKey{}, userID).
func NewRepeatCommand(store *LastMessageStore) *Command {
	return &Command{
		Name:        "repeat",
		Description: "Repeat your last message (command: //)",
		Hidden:      true,
		Execute: func(ctx context.Context, args string) (string, error) {
			userID, ok := ctx.Value(LastMessageUserKey{}).(string)
			if !ok || userID == "" {
				return "", fmt.Errorf("unable to determine user")
			}

			lastMsg := store.Get(userID)
			if lastMsg == "" {
				return "", fmt.Errorf("no previous message to repeat")
			}

			return lastMsg, nil
		},
	}
}

func NewPingCommand() *Command {
	return &Command{
		Name:        "ping",
		Description: "Liveness check",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf("pong %s", time.Now().UTC().Format(time.RFC3339)), nil
		},
	}
}


// NewMultiballCommand returns a /multiball command that forks the current session to a secondary bot.
// forkFn does the actual branch creation, bot acquisition, and notification.
// The context is passed through so the fork can access the requesting chat ID.
func NewMultiballCommand(forkFn func(ctx context.Context) (string, error)) *Command {
	return &Command{
		Name:        "multiball",
		Description: "Fork session to a secondary bot",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			return forkFn(ctx)
		},
	}
}

// NewTmuxCommand returns a /tmux command that wraps the tmux tool, exposing all
// operations via slash-command syntax. It delegates to execFn (the tool's Execute).
func NewTmuxCommand(execFn func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error)) *Command {
	const usage = `Usage: /tmux <command> [args...]

Commands: list, start, send, read, kill, watch, unwatch`

	return &Command{
		Name:        "tmux",
		Description: "Manage tmux sessions — start, send, read, list, kill, watch, unwatch",
		Category:    "observability",
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "kill", Data: "kill"},
				{Label: "read", Data: "read"},
				{Label: "watch", Data: "watch"},
			}
		},
		ChainKeyboard: func(ctx context.Context, subcommand string) []KeyboardOption {
			switch subcommand {
			case "kill", "read", "watch":
			default:
				return nil
			}
			// List owned + watched sessions to build dynamic buttons
			listParams, _ := json.Marshal(map[string]interface{}{"operation": "list"})
			result, err := execFn(ctx, listParams)
			if err != nil || result.Text == "No tmux sessions." {
				return nil
			}
			var opts []KeyboardOption
			seen := make(map[string]bool)
			for _, line := range strings.Split(result.Text, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "SESSION") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				name := fields[0]
				if seen[name] {
					continue
				}
				seen[name] = true
				// For kill/read: show owned and watched sessions
				// Find status field (4th field: owned/watched/idle)
				status := ""
				if len(fields) >= 4 {
					status = fields[3]
				}
				if status == "owned" || status == "watched" {
					opts = append(opts, KeyboardOption{
						Label: name,
						Data:  subcommand + " " + name,
					})
				}
			}
			return opts
		},
		Execute: func(ctx context.Context, args string) (string, error) {
			fields := strings.Fields(args)

			if len(fields) == 0 {
				return usage, nil
			}

			op := fields[0]
			fields = fields[1:]

			var params map[string]interface{}

			switch op {
			case "list":
				params = map[string]interface{}{"operation": "list"}

			case "start":
				params = map[string]interface{}{"operation": "start"}
				autoWatch := true
				var cmdParts []string
				for i := 0; i < len(fields); i++ {
					if fields[i] == "--no-watch" {
						autoWatch = false
						continue
					}
					if _, ok := params["name"]; !ok {
						params["name"] = fields[i]
					} else {
						cmdParts = append(cmdParts, fields[i:]...)
						break
					}
				}
				if len(cmdParts) > 0 {
					params["command"] = strings.Join(cmdParts, " ")
				}

				raw, _ := json.Marshal(params)
				result, err := execFn(ctx, raw)
				if err != nil {
					return "", err
				}

				// Auto-watch unless --no-watch
				if autoWatch {
					// Extract session name from result "Session started: <name>"
					name, _ := params["name"].(string)
					if name == "" {
						// Auto-generated name — parse from result
						name = strings.TrimPrefix(result.Text, "Session started: ")
					}
					watchParams, _ := json.Marshal(map[string]interface{}{
						"operation": "watch",
						"name":      name,
					})
					watchResult, watchErr := execFn(ctx, watchParams)
					if watchErr != nil {
						return result.Text + "\n(auto-watch failed: " + watchErr.Error() + ")", nil
					}
					return result.Text + "\n" + watchResult.Text, nil
				}
				return result.Text, nil

			case "send":
				if len(fields) < 2 {
					return "", fmt.Errorf("usage: /tmux send <name> <keys...>")
				}
				params = map[string]interface{}{
					"operation": "send",
					"name":      fields[0],
					"keys":      strings.Join(fields[1:], " "),
				}

			case "read":
				if len(fields) < 1 {
					return "", fmt.Errorf("usage: /tmux read <name> [lines]")
				}
				params = map[string]interface{}{
					"operation": "read",
					"name":      fields[0],
				}
				if len(fields) > 1 {
					if n, err := strconv.Atoi(fields[1]); err == nil {
						params["lines"] = n
					}
				}

				raw, _ := json.Marshal(params)
				result, err := execFn(ctx, raw)
				if err != nil {
					return "", err
				}
				return "```\n" + result.Text + "\n```", nil

			case "kill":
				if len(fields) < 1 {
					return "", fmt.Errorf("usage: /tmux kill <name>")
				}
				params = map[string]interface{}{
					"operation": "kill",
					"name":      fields[0],
				}

			case "watch":
				if len(fields) < 1 {
					return "", fmt.Errorf("usage: /tmux watch <name> [threshold_secs]")
				}
				params = map[string]interface{}{
					"operation": "watch",
					"name":      fields[0],
				}
				if len(fields) > 1 {
					if n, err := strconv.Atoi(fields[1]); err == nil {
						params["threshold_seconds"] = n
					}
				}

			case "unwatch":
				if len(fields) < 1 {
					return "", fmt.Errorf("usage: /tmux unwatch <name>")
				}
				params = map[string]interface{}{
					"operation": "unwatch",
					"name":      fields[0],
				}

			default:
				return usage, nil
			}

			raw, _ := json.Marshal(params)
			r, err := execFn(ctx, raw)
			return r.Text, err
		},
	}
}

func NewScriptCommand(name, description, script string, timeout int) *Command {
	if timeout <= 0 {
		timeout = 10
	}
	return &Command{
		Name:        name,
		Description: description,
		Execute: func(ctx context.Context, args string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			cmd.SysProcAttr = childSysProcAttr()
			cmd.Cancel = func() error {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}

			out, err := cmd.CombinedOutput()
			result := strings.TrimRight(string(out), "\n")

			if err != nil {
				if ctx.Err() != nil {
					return result + "\n(timed out)", nil
				}
				return result + "\nError: " + err.Error(), nil
			}
			return result, nil
		},
	}
}

// AgentInfo holds data for a single agent in the /agents listing.
type AgentInfo struct {
	ID           string
	SessionKey   string
	Model        string
	Busy         bool
	MessageCount int
	LastActivity string
}

// NewAgentsCommand returns a /agents command listing active agent sessions.
// If registry and deps are non-nil, also supports "/agents new" to launch the creation wizard.
func NewAgentsCommand(listFn func() []AgentInfo, registry *Registry, deps *AgentNewDeps) *Command {
	return &Command{
		Name:        "agents",
		Description: "List active agent sessions",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			// /agents new — start wizard
			if strings.TrimSpace(strings.ToLower(args)) == "new" {
				if registry == nil || deps == nil {
					return "Agent creation wizard is not available.", nil
				}
				w := newAgentWizard(*deps)
				registry.SetWizard(w)
				return "🧙 New Agent Wizard\n\nAgent ID (lowercase slug, e.g. `greek-tutor`):", nil
			}

			agents := listFn()
			if len(agents) == 0 {
				return "No agents configured.", nil
			}

			// Build row data
			type agentRow struct {
				id, session, status, model, msgs string
			}
			rows := make([]agentRow, len(agents))
			for i, a := range agents {
				r := agentRow{id: a.ID}
				if a.SessionKey == "" {
					r.session = "—"
					r.status = "—"
					r.model = "—"
					r.msgs = "—"
				} else {
					r.session = a.SessionKey
					if a.Busy {
						r.status = "busy"
					} else {
						r.status = "idle"
					}
					r.model = a.Model
					r.msgs = fmt.Sprintf("%d", a.MessageCount)
				}
				rows[i] = r
			}

			cols := []table.Column{
				{Header: "ID"},
				{Header: "Session"},
				{Header: "Status"},
				{Header: "Model"},
				{Header: "Messages", Align: table.AlignRight},
			}
			tableRows := make([][]string, len(rows))
			for i, r := range rows {
				tableRows[i] = []string{r.id, r.session, r.status, r.model, r.msgs}
			}
			return "Agents\n\n```\n" + table.FormatWidth(cols, tableRows, displayWidth(ctx)) + "\n```", nil
		},
	}
}

// NewCompactCommand creates a /compact command that triggers manual session compaction.

