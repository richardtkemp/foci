package command

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
			return display.Format(cols, tableRows), nil
		},
	}
}

// NewConfigCommand returns a /config command dumping the running config.
// configFn receives the context (for display width) and the subcommand args

func NewConfigCommand(configFn func(ctx context.Context, args string) (string, error)) *Command {
	return &Command{
		Name:        "config",
		Description: "Show running config. Subcommands: toml, table, available",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			return configFn(ctx, args)
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "toml", Data: "toml"},
				{Label: "table", Data: "table"},
				{Label: "available", Data: "available"},
			}
		},
	}
}


type PromptInfo struct {
	Label    string // e.g. "compaction_summary"
	Path     string // resolved file path, or "" if inline/default/disabled
	Inline   string // inline value (for handoff_msg, braindead_prompt)
	Filename string // default prompt filename (e.g. "keepalive.md")
	Exists   bool   // whether the file exists on disk (only meaningful when Path != "")
	Default  bool   // true if resolved text matches embedded default
	Disabled bool   // true if explicitly set to "none"
}

// PromptFile describes a prompt file found on disk.
type PromptFile struct {
	Dir        string // parent directory
	Name       string // filename
	Configured bool   // true if referenced by config
}

// PromptsData holds all data for the /prompts command.
type PromptsData struct {
	AgentID             string
	Prompts             []PromptInfo
	PromptDirs          []string           // directories scanned
	Files               []PromptFile       // files found on disk
	KnownFilenames      map[string]bool    // recognised prompt filenames (embedded + first-run)
	WorkspacePromptsDir string             // {workspace}/prompts/ — target for reinstall
	EmbeddedPrompts     map[string]string  // filename → embedded text (for reinstall)
	ResolvedTexts       map[string]string  // label → resolved text (for diff)
	DefaultTexts        map[string]string  // label → embedded default text (for diff)
}

// PromptsCmdDeps holds dependencies for the /prompts command.
type PromptsCmdDeps struct {
	DataFn        func() PromptsData
	SendDocFn     func(path string) error
	DiffSummaryFn func(ctx context.Context, customText, defaultText, name string) (string, error)
}

// NewPromptsCommand returns a /prompts command showing prompt config and files.
// Subcommands: reinstall, diff <name>.
func NewPromptsCommand(deps PromptsCmdDeps) *Command {
	return &Command{
		Name:        "prompts",
		Description: "Show configured prompts and prompt files on disk",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			data := deps.DataFn()
			parts := strings.Fields(args)

			if len(parts) == 0 {
				return promptsDisplay(ctx, data), nil
			}

			switch parts[0] {
			case "reinstall":
				return promptsReinstall(data)
			case "diff":
				if len(parts) < 2 {
					return "Usage: /prompts diff <name>", nil
				}
				return promptsDiff(ctx, data, strings.Join(parts[1:], " "), deps)
			default:
				return "Unknown subcommand. Usage: /prompts [reinstall | diff <name>]", nil
			}
		},
	}
}

// relPath returns path relative to the current working directory.
// Falls back to the absolute path if the relative form starts with "..".
func relPath(path string) string {
	pwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(pwd, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// promptsDisplay renders the /prompts output (no subcommand).
func promptsDisplay(_ context.Context, data PromptsData) string {
	var sb strings.Builder

	// Part 1 — Configured prompts table
	fmt.Fprintf(&sb, "Prompts (agent: %s)\n\n", data.AgentID)

	cols := []display.Column{
		{Header: ""},
		{Header: "Prompt"},
		{Header: "Location"},
	}
	var rows [][]string
	for _, p := range data.Prompts {
		var emoji, location string
		switch {
		case p.Disabled:
			emoji = "⛔"
			location = "disabled"
		case p.Inline != "":
			tag := "default"
			if !p.Default {
				tag = "custom"
				emoji = "✏️"
			} else {
				emoji = "✅"
			}
			location = fmt.Sprintf("[%s inline: %d chars]", tag, len(p.Inline))
		case p.Path != "" && p.Exists:
			rel := relPath(p.Path)
			if p.Default {
				emoji = "✅"
			} else {
				emoji = "✏️"
			}
			// Omit filename when it matches the default
			if p.Filename != "" && filepath.Base(p.Path) == p.Filename {
				location = filepath.Dir(rel) + "/"
			} else {
				location = rel
			}
		case p.Path != "" && !p.Exists:
			emoji = "❌"
			location = relPath(p.Path) + " [not found]"
		default:
			emoji = "✅"
			location = "[default]"
		}
		rows = append(rows, []string{emoji, p.Label, location})
	}

	sb.WriteString(display.Format(cols, rows))

	// Part 2 — Unrecognised files
	var unrecognised []PromptFile
	for _, f := range data.Files {
		if !data.KnownFilenames[f.Name] {
			unrecognised = append(unrecognised, f)
		}
	}
	if len(unrecognised) > 0 {
		sb.WriteString("\n\nUnrecognised prompt files\n\n")
		fileCols := []display.Column{
			{Header: "Dir"},
			{Header: "File"},
		}
		var fileRows [][]string
		for _, f := range unrecognised {
			fileRows = append(fileRows, []string{relPath(f.Dir) + "/", f.Name})
		}
		sb.WriteString(display.Format(fileCols, fileRows))
	}

	return sb.String()
}

// promptsReinstall writes all embedded prompts to the workspace prompts directory.
func promptsReinstall(data PromptsData) (string, error) {
	dir := data.WorkspacePromptsDir
	if dir == "" {
		return "", fmt.Errorf("workspace prompts directory not configured")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create prompts dir: %w", err)
	}

	wrote, matched := 0, 0
	total := len(data.EmbeddedPrompts)
	for name, content := range data.EmbeddedPrompts {
		path := filepath.Join(dir, name)
		existing, err := os.ReadFile(path)
		if err == nil && md5.Sum(existing) == md5.Sum([]byte(content)) { // #nosec G401 - content comparison, not security
			matched++
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
		wrote++
	}
	return fmt.Sprintf("Wrote %d of %d prompts to %s (%d already match defaults)", wrote, total, dir, matched), nil
}

// promptsDiff generates a unified diff between the current and default prompt text,
// gets an AI summary, writes both to a temp file, and sends it as a document.
func promptsDiff(ctx context.Context, data PromptsData, name string, deps PromptsCmdDeps) (string, error) {
	label := promptsMatchLabel(name, data)
	if label == "" {
		var names []string
		for _, p := range data.Prompts {
			names = append(names, p.Label)
		}
		return "", fmt.Errorf("no prompt matching %q — valid names: %s", name, strings.Join(names, ", "))
	}

	customText := data.ResolvedTexts[label]
	defaultText := data.DefaultTexts[label]

	diff := diffLines(defaultText, customText, "default", "current")
	if diff == "" {
		return fmt.Sprintf("Prompt %q matches the embedded default — no differences.", label), nil
	}

	// Get AI summary
	summary := ""
	if deps.DiffSummaryFn != nil {
		var err error
		summary, err = deps.DiffSummaryFn(ctx, customText, defaultText, label)
		if err != nil {
			summary = fmt.Sprintf("(summary unavailable: %v)", err)
		}
	}

	// Write combined output to temp file
	var content strings.Builder
	fmt.Fprintf(&content, "# Prompt diff: %s\n\n", label)
	if summary != "" {
		content.WriteString("## Summary\n\n")
		content.WriteString(summary)
		content.WriteString("\n\n")
	}
	content.WriteString("## Diff\n\n```diff\n")
	content.WriteString(diff)
	content.WriteString("\n```\n")

	tmpFile, err := os.CreateTemp("", "prompt-diff-*.md")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(content.String()); err != nil {
		_ = tmpFile.Close() // #nosec G104 - best effort cleanup
		os.Remove(tmpPath)
		return "", fmt.Errorf("write temp file: %w", err)
	}
	_ = tmpFile.Close() // #nosec G104 - file already written successfully

	if deps.SendDocFn != nil {
		if err := deps.SendDocFn(tmpPath); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("send document: %w", err)
		}
	}
	os.Remove(tmpPath)

	changed := diffChangedLines(diff)
	return fmt.Sprintf("Diff for %s sent (%d lines changed).", label, changed), nil
}

// promptsMatchLabel fuzzy-matches a user-provided name to a prompt label.
func promptsMatchLabel(name string, data PromptsData) string {
	norm := promptsNormalize(name)

	// Labels that have diff data
	candidates := make([]string, 0, len(data.Prompts))
	for _, p := range data.Prompts {
		if _, ok := data.ResolvedTexts[p.Label]; ok {
			candidates = append(candidates, p.Label)
		}
	}

	// 1. Exact match on label
	for _, label := range candidates {
		if promptsNormalize(label) == norm {
			return label
		}
	}

	// 2. Exact match on embedded filename stem → find label via default text
	for fn, embeddedText := range data.EmbeddedPrompts {
		fnNorm := promptsNormalize(strings.TrimSuffix(fn, ".md"))
		if fnNorm == norm {
			for _, label := range candidates {
				if data.DefaultTexts[label] == embeddedText {
					return label
				}
			}
		}
	}

	// 3. Substring match on labels
	for _, label := range candidates {
		labelNorm := promptsNormalize(label)
		if strings.Contains(labelNorm, norm) || strings.Contains(norm, labelNorm) {
			return label
		}
	}

	return ""
}

func promptsNormalize(s string) string {
	s = strings.TrimSuffix(s, ".md")
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "_")
	return s
}


func NewLogCommand(eventLogPath string) *Command {
	return &Command{
		Name:        "log",
		Description: "Recent event log lines",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			n := 20
			if args != "" {
				if parsed, err := strconv.Atoi(args); err == nil && parsed > 0 {
					n = parsed
				}
			}
			result, err := tailFile(eventLogPath, n)
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
			n := 10
			if args != "" {
				if parsed, err := strconv.Atoi(args); err == nil && parsed > 0 {
					n = parsed
				}
			}
			result, err := tailFileFiltered(eventLogPath, n, func(line string) bool {
				return strings.Contains(line, " ERROR ") || strings.Contains(line, " WARN ")
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


func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "Log file not found.", nil
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if len(lines) == 0 {
		return "Log is empty.", nil
	}

	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	return strings.Join(lines[start:], "\n"), nil
}

// tailFileFiltered returns the last n lines matching a filter.
func tailFileFiltered(path string, n int, filter func(string) bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "Log file not found.", nil
	}
	defer func() { _ = f.Close() }()

	var matching []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if filter(line) {
			matching = append(matching, line)
		}
	}

	if len(matching) == 0 {
		return "No matching lines.", nil
	}

	start := 0
	if len(matching) > n {
		start = len(matching) - n
	}
	return strings.Join(matching[start:], "\n"), nil
}


func NewCompactCommand(compactFn func(ctx context.Context, dryRun bool) (int, error)) *Command {
	return &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
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
// notifyFn is called before the restart to send a notification (e.g., Telegram).
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

type SecretsStore interface {
	Names() []string
	Set(name, value string)
	Remove(name string) bool
	Save() error
	SectionAllowedHosts(section string) []string
	AddAllowedHost(section, host string)
	RemoveAllowedHost(section, host string) bool
	SetAllowedHosts(section string, hosts []string)
}

// NewSecretsCommand creates the /secrets slash command for managing secrets.
func NewSecretsCommand(store SecretsStore) *Command {
	return &Command{
		Name:        "secrets",
		Description: "Manage secrets (list/set/remove)",
		Category:    "operations",
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "set", Data: "set"},
				{Label: "remove", Data: "remove"},
			}
		},
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			if len(parts) == 0 {
				return secretsUsage, nil
			}

			switch parts[0] {
			case "list":
				names := store.Names()
				if len(names) == 0 {
					return "No secrets configured.", nil
				}
				// Group by section, preserving insertion order
				type secGroup struct {
					name string
					keys []string
				}
				var groups []secGroup
				groupIdx := make(map[string]int)
				for _, name := range names {
					p := strings.SplitN(name, ".", 2)
					sec := p[0]
					key := name
					if len(p) == 2 {
						key = p[1]
					}
					if idx, ok := groupIdx[sec]; ok {
						groups[idx].keys = append(groups[idx].keys, key)
					} else {
						groupIdx[sec] = len(groups)
						groups = append(groups, secGroup{name: sec, keys: []string{key}})
					}
				}

				// Build hosts display per section
				sectionHosts := make(map[string]string)
				for _, g := range groups {
					hosts := store.SectionAllowedHosts(g.name)
					if len(hosts) == 0 {
						sectionHosts[g.name] = "(none)"
					} else {
						sectionHosts[g.name] = strings.Join(hosts, ", ")
					}
				}

				cols := []display.Column{
					{Header: "Section"},
					{Header: "Key"},
					{Header: "Allowed Hosts"},
				}
				var tableRows [][]string
				for _, g := range groups {
					for i, k := range g.keys {
						sec := g.name
						hosts := sectionHosts[g.name]
						if i > 0 {
							sec = ""   // don't repeat section name
							hosts = "" // don't repeat hosts
						}
						tableRows = append(tableRows, []string{sec, k, hosts})
					}
				}
				return fmt.Sprintf("Secrets (%d keys)\n\n%s",
					len(names), display.Format(cols, tableRows)), nil

			case "hosts":
				return secretsHostsSubcmd(store, parts[1:])

			case "set":
				if len(parts) < 3 {
					return "Usage: /secrets set <section.key> <value>", nil
				}
				name := parts[1]
				if !strings.Contains(name, ".") {
					return "Key must be in section.key format (e.g. custom.api_key)", nil
				}
				value := strings.Join(parts[2:], " ")
				store.Set(name, value)
				if err := store.Save(); err != nil {
					return "", fmt.Errorf("save secrets: %w", err)
				}
				return fmt.Sprintf("Secret %s set.", name), nil

			case "remove":
				if len(parts) < 2 {
					return "Usage: /secrets remove <section.key>", nil
				}
				name := parts[1]
				if !store.Remove(name) {
					return fmt.Sprintf("Secret %s not found.", name), nil
				}
				if err := store.Save(); err != nil {
					return "", fmt.Errorf("save secrets: %w", err)
				}
				return fmt.Sprintf("Secret %s removed.", name), nil

			default:
				return secretsUsage, nil
			}
		},
	}
}

const secretsUsage = "Usage: /secrets list | /secrets set <section.key> <value> | /secrets remove <section.key> | /secrets hosts <section> [add|remove|clear] [host]"

// secretsHostsSubcmd handles /secrets hosts <section> [add|remove|clear] [host].
func secretsHostsSubcmd(store SecretsStore, args []string) (string, error) {
	if len(args) == 0 {
		return "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]", nil
	}

	section := args[0]

	// /secrets hosts <section> — show current hosts
	if len(args) == 1 {
		hosts := store.SectionAllowedHosts(section)
		if len(hosts) == 0 {
			return fmt.Sprintf("[%s] allowed_hosts: (none)", section), nil
		}
		return fmt.Sprintf("[%s] allowed_hosts: %s", section, strings.Join(hosts, ", ")), nil
	}

	action := args[1]
	switch action {
	case "add":
		if len(args) < 3 {
			return "Usage: /secrets hosts <section> add <host>", nil
		}
		host := strings.ToLower(strings.TrimSpace(args[2]))
		store.AddAllowedHost(section, host)
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Added %s to [%s] allowed_hosts.", host, section), nil

	case "remove":
		if len(args) < 3 {
			return "Usage: /secrets hosts <section> remove <host>", nil
		}
		host := args[2]
		if !store.RemoveAllowedHost(section, host) {
			return fmt.Sprintf("Host %s not found in [%s] allowed_hosts.", host, section), nil
		}
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Removed %s from [%s] allowed_hosts.", host, section), nil

	case "clear":
		store.SetAllowedHosts(section, nil)
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Cleared allowed_hosts for [%s].", section), nil

	default:
		return "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]", nil
	}
}

