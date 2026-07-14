package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/internal/display"
	"foci/internal/procx"
	"foci/internal/timeutil"
)

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

// PingCommand returns a /ping command for liveness checks.
func PingCommand() *Command {
	return &Command{
		Name:        "ping",
		Description: "Liveness check",
		Category:    "session",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: fmt.Sprintf("pong %s", timeutil.Format(timeutil.Now()))}, nil
		},
	}
}

// RepeatCommand creates the // command that repeats the last message.
func RepeatCommand() *Command {
	return &Command{
		Name:        "repeat",
		Description: "Repeat your last message (command: //)",
		Hidden:      true,
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			if req.UserID == "" {
				return Response{}, fmt.Errorf("unable to determine user")
			}
			if cc.LastMessageStore == nil {
				return Response{}, fmt.Errorf("no message store")
			}
			lastMsg := cc.LastMessageStore.Get(req.UserID)
			if lastMsg == "" {
				return Response{}, fmt.Errorf("no previous message to repeat")
			}
			return Response{Text: lastMsg}, nil
		},
	}
}

// FacetCommand returns a /facet command that forks the current session to a secondary bot.
func FacetCommand() *Command {
	return &Command{
		Name:        "facet",
		Description: "Fork session to a secondary bot",
		Category:    "session",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			return forkFacet(ctx, req, cc)
		},
	}
}

// TmuxCommand returns a /tmux command that wraps the tmux tool, exposing all
// operations via slash-command syntax.
func TmuxCommand() *Command {
	execTmux := func(ctx context.Context, cc CommandContext, params json.RawMessage) (string, error) {
		if cc.TmuxTool == nil {
			return "", fmt.Errorf("tmux tool not available")
		}
		result, err := cc.TmuxTool.Execute(ctx, params)
		return result.Text, err
	}

	simpleOp := func(operation string) func(context.Context, Request, CommandContext) (Response, error) {
		return func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			fields := strings.Fields(req.Args)
			if len(fields) < 1 {
				return Response{}, fmt.Errorf("usage: /tmux %s <name>", operation)
			}
			params := map[string]interface{}{
				"operation": operation,
				"name":      fields[0],
			}
			raw, _ := json.Marshal(params)
			text, err := execTmux(ctx, cc, raw)
			return Response{Text: text}, err
		}
	}

	cmd := &Command{
		Name:        "tmux",
		Description: "Manage tmux sessions — start, send, read, list, kill, watch, unwatch",
		Category:    "observability",
		Subcommands: []Subcommand{
			{
				Name:        "list",
				Description: "List tmux sessions",
				Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
					raw, _ := json.Marshal(map[string]interface{}{"operation": "list"})
					text, err := execTmux(ctx, cc, raw)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "start",
				Description: "Start a new tmux session",
				Hidden:      true,
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					fields := strings.Fields(req.Args)
					params := map[string]interface{}{"operation": "start"}
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
					text, err := execTmux(ctx, cc, raw)
					if err != nil {
						return Response{}, err
					}

					if autoWatch {
						name, _ := params["name"].(string)
						if name == "" {
							name = strings.TrimPrefix(text, "Session started: ")
						}
						watchParams, _ := json.Marshal(map[string]interface{}{
							"operation": "watch",
							"name":      name,
						})
						watchText, watchErr := execTmux(ctx, cc, watchParams)
						if watchErr != nil {
							return Response{Text: text + "\n(auto-watch failed: " + watchErr.Error() + ")"}, nil
						}
						return Response{Text: text + "\n" + watchText}, nil
					}
					return Response{Text: text}, nil
				},
			},
			{
				Name:        "send",
				Description: "Send keys to a tmux session",
				Hidden:      true,
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					fields := strings.Fields(req.Args)
					if len(fields) < 2 {
						return Response{}, fmt.Errorf("usage: /tmux send <name> <keys...>")
					}
					params := map[string]interface{}{
						"operation": "send",
						"name":      fields[0],
						"keys":      strings.Join(fields[1:], " "),
					}
					raw, _ := json.Marshal(params)
					text, err := execTmux(ctx, cc, raw)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "read",
				Description: "Read output from a tmux session",
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					fields := strings.Fields(req.Args)
					if len(fields) < 1 {
						return Response{}, fmt.Errorf("usage: /tmux read <name> [lines]")
					}
					params := map[string]interface{}{
						"operation": "read",
						"name":      fields[0],
					}
					if len(fields) > 1 {
						if n, err := strconv.Atoi(fields[1]); err == nil {
							params["lines"] = n
						}
					}
					raw, _ := json.Marshal(params)
					text, err := execTmux(ctx, cc, raw)
					if err != nil {
						return Response{}, err
					}
					return Response{Text: "```\n" + text + "\n```"}, nil
				},
			},
			{
				Name:        "kill",
				Description: "Kill a tmux session",
				Execute:     simpleOp("kill"),
			},
			{
				Name:        "watch",
				Description: "Watch a tmux session for output changes",
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					fields := strings.Fields(req.Args)
					if len(fields) < 1 {
						return Response{}, fmt.Errorf("usage: /tmux watch <name> [threshold_secs]")
					}
					params := map[string]interface{}{
						"operation": "watch",
						"name":      fields[0],
					}
					if len(fields) > 1 {
						if n, err := strconv.Atoi(fields[1]); err == nil {
							params["threshold_seconds"] = n
						}
					}
					raw, _ := json.Marshal(params)
					text, err := execTmux(ctx, cc, raw)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "unwatch",
				Description: "Stop watching a tmux session",
				Hidden:      true,
				Execute:     simpleOp("unwatch"),
			},
		},
		ChainKeyboard: func(ctx context.Context, subcommand string, cc CommandContext) []KeyboardOption {
			switch subcommand {
			case "kill", "read", "watch":
			default:
				return nil
			}
			if cc.TmuxTool == nil {
				return nil
			}
			listParams, _ := json.Marshal(map[string]interface{}{"operation": "list"})
			result, err := cc.TmuxTool.Execute(ctx, listParams)
			if err != nil || result.Text == "No tmux sessions." {
				return nil
			}
			// The tool's list operation already filters to sessions owned by the
			// current session key (from context), so every row is a valid target.
			var opts []KeyboardOption
			seen := make(map[string]bool)
			for _, line := range strings.Split(result.Text, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "SESSION") || strings.HasPrefix(line, "|") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) == 0 || fields[0] == "|" {
					continue
				}
				name := fields[0]
				if seen[name] {
					continue
				}
				seen[name] = true
				opts = append(opts, KeyboardOption{
					Label: name,
					Data:  subcommand + " " + name,
				})
			}
			return opts
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

// ScriptCommand returns a command that runs a shell script.
func ScriptCommand(name, description, script string, timeout int) *Command {
	if timeout <= 0 {
		timeout = 10
	}
	return &Command{
		Name:        name,
		Description: description,
		Execute: func(ctx context.Context, _ Request, _ CommandContext) (Response, error) {
			ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()

			cmd := procx.Spawn(ctx, "sh", "-c", script)
			cmd.Cancel = func() error {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}

			out, err := cmd.CombinedOutput()
			result := strings.TrimRight(string(out), "\n")

			if err != nil {
				if ctx.Err() != nil {
					return Response{Text: result + "\n(timed out)"}, nil
				}
				return Response{Text: result + "\nError: " + err.Error()}, nil
			}
			return Response{Text: result}, nil
		},
	}
}

// AgentInfo holds data for a single agent in the /agents listing.
type AgentInfo struct {
	ID           string
	SessionKey   string
	Model        string
	MessageCount int
	LastActivity string
}

// AgentsCommand returns a /agents command listing active agent sessions.
func AgentsCommand() *Command {
	return &Command{
		Name:        "agents",
		Description: "List active agent sessions",
		Category:    "session",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			// /agents new — start wizard
			if strings.TrimSpace(strings.ToLower(req.Args)) == "new" {
				if cc.AgentNewDeps == nil {
					return Response{Text: "Agent creation wizard is not available."}, nil
				}
				w := newAgentWizard(*cc.AgentNewDeps)
				// Need registry reference — pass via AgentNewDeps.Registry
				if cc.AgentNewDeps.Registry != nil {
					cc.AgentNewDeps.Registry.SetWizard(req.SessionKey, w)
				}
				return Response{Text: "🧙 New Agent Wizard\n\nAgent name (e.g. `Greek Tutor`):"}, nil
			}

			if cc.AgentListFn == nil {
				return Response{Text: "No agents configured."}, nil
			}
			agents := cc.AgentListFn()
			if len(agents) == 0 {
				return Response{Text: "No agents configured."}, nil
			}

			type agentRow struct {
				id, session, model, msgs string
			}
			rows := make([]agentRow, len(agents))
			for i, a := range agents {
				r := agentRow{id: a.ID}
				if a.SessionKey == "" {
					r.session = "—"
					r.model = "—"
					r.msgs = "—"
				} else {
					r.session = a.SessionKey
					r.model = a.Model
					r.msgs = fmt.Sprintf("%d", a.MessageCount)
				}
				rows[i] = r
			}

			cols := []display.Column{
				{Header: "ID"},
				{Header: "Session"},
				{Header: "Model"},
				{Header: "Messages", Align: display.AlignRight},
			}
			tableRows := make([][]string, len(rows))
			for i, r := range rows {
				tableRows[i] = []string{r.id, r.session, r.model, r.msgs}
			}
			return Response{Text: "Agents\n\n" + display.MarkdownTable(cols, tableRows)}, nil
		},
	}
}
