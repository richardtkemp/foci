package command

import (
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/display"
	"foci/internal/provider"
	"foci/prompts"
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

// PromptsCommand returns a /prompts command showing prompt config and files.
func PromptsCommand() *Command {
	return &Command{
		Name:        "prompts",
		Description: "Prompt config. Subcommands: list, reinstall, diff",
		Category:    "diagnostics",
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "reinstall", Data: "reinstall"},
				{Label: "diff", Data: "diff"},
			}
		},
		ChainKeyboard: func(_ context.Context, subcommand string, cc CommandContext) []KeyboardOption {
			if subcommand != "diff" {
				return nil
			}
			data := resolvePromptsData(cc)
			var opts []KeyboardOption
			for _, p := range data.Prompts {
				if _, ok := data.ResolvedTexts[p.Label]; ok {
					opts = append(opts, KeyboardOption{Label: p.Label, Data: p.Label})
				}
			}
			return opts
		},
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			data := resolvePromptsData(cc)
			parts := strings.Fields(req.Args)

			if len(parts) == 0 {
				return Response{Text: "Usage: /prompts list | reinstall | diff <name>"}, nil
			}

			switch parts[0] {
			case "list":
				return Response{Text: promptsDisplay(data)}, nil
			case "reinstall":
				text, err := promptsReinstall(data)
				return Response{Text: text}, err
			case "diff":
				if len(parts) < 2 {
					return Response{Text: "Usage: /prompts diff <name>"}, nil
				}
				return promptsDiff(ctx, data, strings.Join(parts[1:], " "), cc)
			default:
				return Response{Text: "Unknown subcommand. Usage: /prompts list | reinstall | diff <name>"}, nil
			}
		},
	}
}

// resolvePromptsData returns PromptsDataFn(cc) if set, otherwise buildPromptsData(cc).
func resolvePromptsData(cc CommandContext) PromptsData {
	if cc.PromptsDataFn != nil {
		return cc.PromptsDataFn(cc)
	}
	return buildPromptsData(cc)
}

// buildPromptsData constructs the data for the /prompts command.
func buildPromptsData(cc CommandContext) PromptsData {
	dirs := cc.PromptSearchDirs
	acfg := cc.AgentConfig
	cfg := cc.Config

	allPrompts := []PromptInfo{
		resolvePromptInfo("compaction_summary",
			resolveString(acfg.CompactionSummaryPrompt, cfg.Sessions.CompactionSummaryPrompt),
			"compaction-summary.md", prompts.CompactionSummary(), dirs),
		resolvePromptInfo("branch_orient_multiball",
			resolveOrientPath(acfg.BranchOrientationMultiballPrompt, cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt),
			"branch-orientation-multiball.md", prompts.BranchOrientationMultiball(), dirs),
		resolvePromptInfo("branch_orient_headless",
			resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt),
			"branch-orientation-headless.md", prompts.BranchOrientationHeadless(), dirs),
		resolvePromptInfo("keepalive",
			acfg.Keepalive.Prompt,
			"keepalive.md", prompts.Keepalive(), dirs),
		resolvePromptInfo("background",
			acfg.Background.Prompt,
			"background.md", prompts.Background(), dirs),
		resolvePromptInfo("memory_formation",
			acfg.MemoryFormation.IntervalPrompt,
			"memory-formation.md", prompts.MemoryFormation(), dirs),
		resolvePromptInfo("memory_consolidation",
			acfg.MemoryFormation.ConsolidationPrompt,
			"memory-consolidation.md", prompts.MemoryConsolidation(), dirs),
		resolvePromptInfo("memory_session_end",
			acfg.MemoryFormation.SessionEndPrompt,
			"memory-formation.md", prompts.MemoryFormation(), dirs),
	}

	allPrompts = append(allPrompts,
		inlinePromptInfo("compaction_handoff",
			resolveString(acfg.CompactionHandoffMsg, cfg.Sessions.CompactionHandoffMsg),
			prompts.CompactionHandoff()),
		inlinePromptInfo("braindead_warning",
			acfg.BraindeadPrompt, ""),
	)

	embedded := map[string]string{
		"compaction-summary.md":           prompts.CompactionSummary(),
		"compaction-handoff.md":           prompts.CompactionHandoff(),
		"branch-orientation-multiball.md": prompts.BranchOrientationMultiball(),
		"branch-orientation-headless.md":  prompts.BranchOrientationHeadless(),
		"keepalive.md":                    prompts.Keepalive(),
		"background.md":                   prompts.Background(),
		"memory-formation.md":             prompts.MemoryFormation(),
		"memory-consolidation.md":         prompts.MemoryConsolidation(),
	}

	type promptDef struct {
		label, configPath, filename string
		embeddedDefault             string
	}
	fileDefs := []promptDef{
		{"compaction_summary", resolveString(acfg.CompactionSummaryPrompt, cfg.Sessions.CompactionSummaryPrompt), "compaction-summary.md", prompts.CompactionSummary()},
		{"branch_orient_multiball", resolveOrientPath(acfg.BranchOrientationMultiballPrompt, cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt), "branch-orientation-multiball.md", prompts.BranchOrientationMultiball()},
		{"branch_orient_headless", resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt), "branch-orientation-headless.md", prompts.BranchOrientationHeadless()},
		{"keepalive", acfg.Keepalive.Prompt, "keepalive.md", prompts.Keepalive()},
		{"background", acfg.Background.Prompt, "background.md", prompts.Background()},
		{"memory_formation", acfg.MemoryFormation.IntervalPrompt, "memory-formation.md", prompts.MemoryFormation()},
		{"memory_consolidation", acfg.MemoryFormation.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation()},
		{"memory_session_end", acfg.MemoryFormation.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation()},
	}
	resolvedTexts := make(map[string]string, len(fileDefs)+2)
	defaultTexts := make(map[string]string, len(fileDefs)+2)
	for _, d := range fileDefs {
		resolvedTexts[d.label] = prompts.ResolvePrompt(d.configPath, d.filename, d.embeddedDefault, dirs...)
		defaultTexts[d.label] = d.embeddedDefault
	}

	handoffVal := resolveString(acfg.CompactionHandoffMsg, cfg.Sessions.CompactionHandoffMsg)
	if handoffVal == "" {
		resolvedTexts["compaction_handoff"] = prompts.CompactionHandoff()
	} else if handoffVal != "none" {
		resolvedTexts["compaction_handoff"] = handoffVal
	}
	defaultTexts["compaction_handoff"] = prompts.CompactionHandoff()
	if acfg.BraindeadPrompt != "" && acfg.BraindeadPrompt != "none" {
		resolvedTexts["braindead_warning"] = acfg.BraindeadPrompt
	}
	defaultTexts["braindead_warning"] = ""

	configuredPaths := make(map[string]bool)
	for _, pi := range allPrompts {
		if pi.Path != "" {
			configuredPaths[pi.Path] = true
		}
	}

	var promptDirs []string
	var files []PromptFile
	sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts")
	wsDir := filepath.Join(acfg.Workspace, "prompts")
	for _, dir := range []string{sharedDir, wsDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		promptDirs = append(promptDirs, dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			fullPath := filepath.Join(dir, e.Name())
			files = append(files, PromptFile{
				Dir:        dir,
				Name:       e.Name(),
				Configured: configuredPaths[fullPath],
			})
		}
	}

	knownFilenames := make(map[string]bool, len(embedded)+1)
	for name := range embedded {
		knownFilenames[name] = true
	}
	knownFilenames["first-run.md"] = true

	return PromptsData{
		AgentID:             acfg.ID,
		Prompts:             allPrompts,
		PromptDirs:          promptDirs,
		Files:               files,
		KnownFilenames:      knownFilenames,
		WorkspacePromptsDir: filepath.Join(acfg.Workspace, "prompts"),
		EmbeddedPrompts:     embedded,
		ResolvedTexts:       resolvedTexts,
		DefaultTexts:        defaultTexts,
	}
}

// resolveString returns the first non-empty string.
func resolveString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// isDefaultPrompt compares resolved text to the embedded default via MD5.
func isDefaultPrompt(resolved, embeddedDefault string) bool {
	return md5.Sum([]byte(resolved)) == md5.Sum([]byte(embeddedDefault)) // #nosec G401 - content comparison, not security
}

// resolvePromptInfo builds a PromptInfo for a file-path-based prompt.
func resolvePromptInfo(label, configPath, filename, embeddedDefault string, searchDirs []string) PromptInfo {
	if configPath == "none" {
		return PromptInfo{Label: label, Filename: filename, Disabled: true}
	}

	resolved := prompts.ResolvePrompt(configPath, filename, embeddedDefault, searchDirs...)
	def := isDefaultPrompt(resolved, embeddedDefault)

	path := configPath
	if path == "" || path == "default" {
		for _, dir := range searchDirs {
			fp := filepath.Join(dir, filename)
			if _, err := os.Stat(fp); err == nil {
				path = fp
				break
			}
		}
	}

	if path == "" || path == "default" {
		return PromptInfo{Label: label, Filename: filename, Default: def}
	}

	_, err := os.Stat(path)
	return PromptInfo{Label: label, Path: path, Filename: filename, Exists: err == nil, Default: def}
}

// inlinePromptInfo builds a PromptInfo for an inline prompt value.
func inlinePromptInfo(label, value, embeddedDefault string) PromptInfo {
	if value == "" {
		return PromptInfo{Label: label, Inline: embeddedDefault, Default: true}
	}
	if value == "none" {
		return PromptInfo{Label: label, Disabled: true}
	}
	return PromptInfo{Label: label, Inline: value, Default: isDefaultPrompt(value, embeddedDefault)}
}

// relPath returns path relative to the current working directory.
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

// promptsDisplay renders the /prompts output.
func promptsDisplay(data PromptsData) string {
	var sb strings.Builder

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

func promptsDiff(ctx context.Context, data PromptsData, name string, cc CommandContext) (Response, error) {
	label := promptsMatchLabel(name, data)
	if label == "" {
		var names []string
		for _, p := range data.Prompts {
			names = append(names, p.Label)
		}
		return Response{}, fmt.Errorf("no prompt matching %q — valid names: %s", name, strings.Join(names, ", "))
	}

	customText := data.ResolvedTexts[label]
	defaultText := data.DefaultTexts[label]

	diff := diffLines(defaultText, customText, "default", "current")
	if diff == "" {
		return Response{Text: fmt.Sprintf("Prompt %q matches the embedded default — no differences.", label)}, nil
	}

	// Get AI summary
	summary := ""
	if cc.Client != nil {
		var err error
		summary, err = buildDiffSummary(ctx, cc, customText, defaultText, label)
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
		return Response{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(content.String()); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return Response{}, fmt.Errorf("write temp file: %w", err)
	}
	_ = tmpFile.Close()

	// Send document via primary connection
	if cc.ConnMgr != nil {
		if conn := cc.ConnMgr.Primary(cc.AgentConfig.ID); conn != nil {
			if err := conn.SendDocument(tmpPath); err != nil {
				_ = os.Remove(tmpPath)
				return Response{}, fmt.Errorf("send document: %w", err)
			}
		}
	}
	_ = os.Remove(tmpPath)

	changed := diffChangedLines(diff)
	return Response{Text: fmt.Sprintf("Diff for %s sent (%d lines changed).", label, changed)}, nil
}

// buildDiffSummary generates an AI summary comparing custom vs default prompt text.
func buildDiffSummary(ctx context.Context, cc CommandContext, customText, defaultText, name string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cheapAlias := "haiku"
	_, bareModelID := config.SplitDeveloperModel(cc.AgentConfig.Model)
	if strings.HasPrefix(bareModelID, "gemini-") {
		cheapAlias = "flash"
	}

	var diffClient provider.Client
	var cheapModel string
	if resolved, err := config.ResolveModel(cheapAlias, "", cc.ModelAliases); err == nil && cc.ClientProvider != nil {
		diffClient = cc.ClientProvider.ResolveEndpointClient(resolved.Endpoint, resolved.Format)
		cheapModel = resolved.Developer + "/" + resolved.ModelID
	}
	if diffClient == nil {
		diffClient = cc.Client
		cheapModel = cheapAlias
	}

	prompt := fmt.Sprintf("Below are two versions of the %q prompt. These prompts are injected into AI agent sessions to guide agent behaviour during specific operations (compaction, keepalive, memory formation, etc).\n\n--- DEFAULT (embedded) ---\n%s\n\n--- CURRENT (resolved from config) ---\n%s\n\nConcisely summarise: 1) what the default version instructs the agent to do, 2) what the current version instructs, 3) key differences.", name, defaultText, customText)
	resp, err := provider.Send(callCtx, diffClient, &provider.MessageRequest{
		Model:     cheapModel,
		MaxTokens: 1024,
		Messages:  []provider.Message{{Role: "user", Content: provider.TextContent(prompt)}},
	}, nil)
	if err != nil {
		return "", err
	}
	return provider.TextOf(resp.Content), nil
}

func promptsMatchLabel(name string, data PromptsData) string {
	norm := promptsNormalize(name)

	candidates := make([]string, 0, len(data.Prompts))
	for _, p := range data.Prompts {
		if _, ok := data.ResolvedTexts[p.Label]; ok {
			candidates = append(candidates, p.Label)
		}
	}

	for _, label := range candidates {
		if promptsNormalize(label) == norm {
			return label
		}
	}

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
