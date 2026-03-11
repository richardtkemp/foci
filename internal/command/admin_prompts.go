package command

import (
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/display"
)

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
// Subcommands: list, reinstall, diff <name>.
func NewPromptsCommand(deps PromptsCmdDeps) *Command {
	return &Command{
		Name:        "prompts",
		Description: "Prompt config. Subcommands: list, reinstall, diff",
		Category:    "diagnostics",
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "reinstall", Data: "reinstall"},
				{Label: "diff", Data: "diff"},
			}
		},
		ChainKeyboard: func(ctx context.Context, subcommand string) []KeyboardOption {
			if subcommand != "diff" {
				return nil
			}
			data := deps.DataFn()
			var opts []KeyboardOption
			for _, p := range data.Prompts {
				if _, ok := data.ResolvedTexts[p.Label]; ok {
					opts = append(opts, KeyboardOption{Label: p.Label, Data: p.Label})
				}
			}
			return opts
		},
		Execute: func(ctx context.Context, args string) (string, error) {
			data := deps.DataFn()
			parts := strings.Fields(args)

			if len(parts) == 0 {
				return "Usage: /prompts list | reinstall | diff <name>", nil
			}

			switch parts[0] {
			case "list":
				return promptsDisplay(ctx, data), nil
			case "reinstall":
				return promptsReinstall(data)
			case "diff":
				if len(parts) < 2 {
					return "Usage: /prompts diff <name>", nil
				}
				return promptsDiff(ctx, data, strings.Join(parts[1:], " "), deps)
			default:
				return "Unknown subcommand. Usage: /prompts list | reinstall | diff <name>", nil
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

	sb.WriteString(display.MarkdownTable(cols, rows))

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
		sb.WriteString(display.MarkdownTable(fileCols, fileRows))
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
		// #nosec G401 - content comparison only, not security
		if err == nil && md5.Sum(existing) == md5.Sum([]byte(content)) {
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
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write temp file: %w", err)
	}
	_ = tmpFile.Close() // #nosec G104 - file already written successfully

	if deps.SendDocFn != nil {
		if err := deps.SendDocFn(tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("send document: %w", err)
		}
	}
	_ = os.Remove(tmpPath)

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
