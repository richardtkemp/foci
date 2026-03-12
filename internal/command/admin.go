package command

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"foci/internal/display"
)

func NewResetCommand(resetFn func() error) *Command {
	return &Command{
		Name:        "reset",
		Description: "Clear session history",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if err := resetFn(); err != nil {
				return "", err
			}
			return "Session cleared.", nil
		},
	}
}

// NewModelCommand returns a /model command to show or switch the model.
// getModel returns current model; setModel switches it with endpoint and model;
// resolveModel resolves input to (endpoint, model).
// modelAliases provides the alias map for keyboard options (may be nil).

// ToolInfo holds data for a single tool in the /tools listing.
type ToolInfo struct {
	Name        string
	Description string
}

// NewToolsCommand returns a /tools command listing registered tools.
func NewToolsCommand(listFn func() []ToolInfo) *Command {
	return &Command{
		Name:        "tools",
		Description: "List registered tools",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			tools := listFn()
			if len(tools) == 0 {
				return "No tools registered.", nil
			}
			cols := []display.Column{
				{Header: "Name"},
				{Header: "Description"},
			}
			tableRows := make([][]string, len(tools))
			for i, t := range tools {
				tableRows[i] = []string{t.Name, t.Description}
			}
			return display.MarkdownTable(cols, tableRows), nil
		},
	}
}

// NewConfigCommand returns a /config command for viewing and editing the running config.
// configFn handles the read-only subcommands (toml, table, available).
// registry and setDeps enable the interactive /config set wizard; pass nil to disable.
func NewConfigCommand(configFn func(ctx context.Context, args string) (string, error), registry *Registry, setDeps *ConfigSetDeps) *Command {
	return &Command{
		Name:        "config",
		Description: "Show or edit config. Subcommands: toml, table, available, set",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			if len(parts) > 0 && strings.ToLower(parts[0]) == "set" {
				if setDeps == nil {
					return "Config set is not available.", nil
				}
				setArgs := strings.TrimSpace(strings.TrimPrefix(args, parts[0]))
				return configSet(registry, setDeps, setArgs)
			}
			return configFn(ctx, args)
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "toml", Data: "toml"},
				{Label: "table", Data: "table"},
				{Label: "available", Data: "available"},
				{Label: "set", Data: "set"},
			}
		},
	}
}

// configSet handles /config set — either starts a wizard (bare) or does a direct set.
func configSet(registry *Registry, deps *ConfigSetDeps, args string) (string, error) {
	// Direct mode: /config set section.key=value
	if args != "" && strings.Contains(args, "=") {
		return ConfigSetDirect(*deps, args)
	}

	// Interactive mode: start wizard.
	if registry == nil {
		return "Config set wizard is not available.", nil
	}

	w := newConfigSetWizard(*deps)
	registry.SetWizard(w)

	sections := deps.SectionsFn()
	return fmt.Sprintf("Which section?\n%s", strings.Join(sections, ", ")), nil
}

func NewLogCommand(eventLogPath string) *Command {
	return &Command{
		Name:        "log",
		Description: "Recent event log lines",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			n := parseLineCount(args, 20)
			result, err := tailFile(eventLogPath, n, nil)
			if err != nil || result == "Log file not found." || result == "Log is empty." {
				return result, err
			}
			return "```\n" + result + "\n```", nil
		},
	}
}

// NewErrorsCommand returns a /errors command showing recent ERROR/WARN lines.
func NewErrorsCommand(eventLogPath string) *Command {
	return &Command{
		Name:        "errors",
		Description: "Recent error/warning log lines",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			n := parseLineCount(args, 10)
			result, err := tailFile(eventLogPath, n, func(line string) bool {
				return logLineLevel(line) == "ERROR" || logLineLevel(line) == "WARN"
			})
			if err != nil || result == "Log file not found." || result == "No matching lines." {
				return result, err
			}
			return "```\n" + result + "\n```", nil
		},
	}
}

// NewHelpCommand returns a /help command that lists all registered commands.
func NewHelpCommand(registry *Registry) *Command {
	return &Command{
		Name:        "help",
		Description: "List available commands",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			// Collect visible commands grouped by category.
			type group struct {
				emoji string
				label string
			}
			categoryOrder := []string{"observability", "operations", "diagnostics", "session"}
			categoryMeta := map[string]group{
				"observability": {emoji: "📊", label: "Observability"},
				"operations":    {emoji: "⚙️", label: "Operations"},
				"diagnostics":   {emoji: "🔍", label: "Diagnostics"},
				"session":       {emoji: "💬", label: "Session"},
			}
			groups := make(map[string][]*Command)
			var other []*Command

			for _, cmd := range registry.All() {
				if cmd.Hidden {
					continue
				}
				if cmd.Category != "" {
					groups[cmd.Category] = append(groups[cmd.Category], cmd)
				} else {
					other = append(other, cmd)
				}
			}

			var sb strings.Builder
			for _, cat := range categoryOrder {
				cmds := groups[cat]
				if len(cmds) == 0 {
					continue
				}
				meta := categoryMeta[cat]
				fmt.Fprintf(&sb, "%s %s\n", meta.emoji, meta.label)
				for _, cmd := range cmds {
					fmt.Fprintf(&sb, "  /%s — %s\n", cmd.Name, cmd.Description)
				}
				sb.WriteByte('\n')
			}
			if len(other) > 0 {
				sb.WriteString("📦 Other\n")
				for _, cmd := range other {
					fmt.Fprintf(&sb, "  /%s — %s\n", cmd.Name, cmd.Description)
				}
				sb.WriteByte('\n')
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

type BuildInfo struct {
	Version   string
	GoVersion string
	GitCommit string
	BuildTime string
}

// NewVersionCommand returns a /version command.
func NewVersionCommand(info BuildInfo) *Command {
	return &Command{
		Name:        "version",
		Description: "Build version info",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf("version: %s\ngo: %s\ncommit: %s\nbuilt: %s",
				info.Version, info.GoVersion, info.GitCommit, info.BuildTime), nil
		},
	}
}

func NewReloadCommand(reloadFn func() (string, error)) *Command {
	return &Command{
		Name:        "reload",
		Description: "Reload config, skills, and system prompt from disk",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			return reloadFn()
		},
	}
}

func NewCompactCommand(compactFn func(ctx context.Context, dryRun bool) (int, error)) *Command {
	return &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "compact", Data: "run"},
				{Label: "dry-run", Data: "dry-run"},
			}
		},
		Execute: func(ctx context.Context, args string) (string, error) {
			dryRun := strings.TrimSpace(args) == "dry-run"
			oldCount, err := compactFn(ctx, dryRun)
			if err != nil {
				return "", err
			}
			if dryRun {
				return fmt.Sprintf("Dry-run complete — %d messages would be summarised. Summary sent.", oldCount), nil
			}
			return fmt.Sprintf("Context compacted — %d messages summarised.", oldCount), nil
		},
	}
}

// NewRestartCommand creates a /restart command that restarts the foci service.
// notifyFn is called before the restart to send a notification (e.g., platform message).
func NewRestartCommand(notifyFn func(string)) *Command {
	return &Command{
		Name:        "restart",
		Description: "Restart the foci service",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if notifyFn != nil {
				notifyFn("Restarting...")
			}

			cmd := exec.Command("systemctl", "restart", "foci")
			if err := cmd.Start(); err != nil {
				return "", fmt.Errorf("restart failed: %w", err)
			}
			// Don't wait — process will be killed by systemd
			return "Restarting...", nil
		},
	}
}

// parseLineCount parses a line count from args, returning defaultN if empty or invalid.
func parseLineCount(args string, defaultN int) int {
	if args != "" {
		if parsed, err := strconv.Atoi(args); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultN
}

// logLineLevel extracts the log level field from a structured log line.
// Format: "{RFC3339} {LEVEL} [{component}] {msg}" — level is the second
// space-delimited field, trimmed of padding.
func logLineLevel(line string) string {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimSpace(fields[1])
}

// tailFile returns the last n lines from a file. If filter is non-nil,
// only lines for which filter returns true are considered.
func tailFile(path string, n int, filter func(string) bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "Log file not found.", nil
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if filter == nil || filter(line) {
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		if filter != nil {
			return "No matching lines.", nil
		}
		return "Log is empty.", nil
	}

	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	return strings.Join(lines[start:], "\n"), nil
}
