package command

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/provider"
	"foci/internal/skills"
	"foci/prompts"
)

// ResetCommand returns a /reset command that clears session history with memory formation.
func ResetCommand() *Command {
	return &Command{
		Name:        "reset",
		Description: "Clear session history",
		Category:    "operations",
		Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.Agent.IsProcessing() {
				return Response{}, fmt.Errorf("agent is processing — send /stop first, then /reset")
			}
			sk := cc.DefaultSessionKey()
			if sk == "" {
				return Response{}, fmt.Errorf("no active session to reset")
			}
			resetOrientPath := resolveOrientPath(
				cc.AgentConfig.BranchOrientationHeadlessPrompt, cc.Config.Sessions.BranchOrientationHeadlessPrompt,
				cc.AgentConfig.BranchOrientationPrompt, cc.Config.Sessions.BranchOrientationPrompt,
			)
			FireSessionEndMemory(cc.Agent, cc.Sessions, sk, cc.AgentConfig.MemoryFormation, func(bk, pk, bt string) string {
				return BuildBranchOrientation(resetOrientPath, bk, pk, bt, false, cc.PromptSearchDirs)
			}, cc.PromptSearchDirs, ctx, false)
			newKey, err := cc.Sessions.RotateKey(sk)
			if err != nil {
				return Response{}, err
			}
			cc.Agent.RotateSession(sk, newKey)
			cc.Bootstrap.Reload()
			cc.Agent.InvalidateSystemCaches()
			return Response{Text: "Session cleared."}, nil
		},
	}
}

// ToolInfo holds data for a single tool in the /tools listing.
type ToolInfo struct {
	Name        string
	Description string
}

// ToolsCommand returns a /tools command listing registered tools.
func ToolsCommand() *Command {
	return &Command{
		Name:        "tools",
		Description: "List registered tools",
		Category:    "session",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.ToolsRegistry == nil {
				return Response{Text: "No tools registered."}, nil
			}
			allTools := cc.ToolsRegistry.All()
			if len(allTools) == 0 {
				return Response{Text: "No tools registered."}, nil
			}
			cols := []display.Column{
				{Header: "Name"},
				{Header: "Description"},
			}
			tableRows := make([][]string, len(allTools))
			for i, t := range allTools {
				tableRows[i] = []string{t.Name, t.Description}
			}
			return Response{Text: display.MarkdownTable(cols, tableRows)}, nil
		},
	}
}

// ConfigCommand returns a /config command for viewing and editing the running config.
func ConfigCommand() *Command {
	return &Command{
		Name:        "config",
		Description: "Show or edit config. Subcommands: toml, table, available, set",
		Category:    "diagnostics",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			parts := strings.Fields(req.Args)
			if len(parts) > 0 && strings.ToLower(parts[0]) == "set" {
				if cc.ConfigSetDeps == nil {
					return Response{Text: "Config set is not available."}, nil
				}
				setArgs := strings.TrimSpace(strings.TrimPrefix(req.Args, parts[0]))
				text, err := configSet(cc.ConfigSetDeps, setArgs)
				return Response{Text: text}, err
			}
			switch strings.TrimSpace(strings.ToLower(req.Args)) {
			case "toml":
				return Response{Text: config.FormatConfigTOML(cc.Config, cc.AgentConfig)}, nil
			case "table":
				return Response{Text: strings.Join(config.FormatConfigGrouped(cc.Config, cc.AgentConfig), "\x00")}, nil
			case "available":
				return Response{Text: config.FormatAvailable(cc.Config, cc.AgentConfig)}, nil
			default:
				return Response{Text: "/config toml — raw TOML of running config (secrets redacted)\n/config table — formatted table of current config values\n/config available — unset options with defaults\n/config set [section.key=value] — edit config file"}, nil
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
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
func configSet(deps *ConfigSetDeps, args string) (string, error) {
	if args != "" && strings.Contains(args, "=") {
		return ConfigSetDirect(*deps, args)
	}

	if deps.Registry == nil {
		return "Config set wizard is not available.", nil
	}

	w := newConfigSetWizard(*deps)
	deps.Registry.SetWizard(w)

	sections := deps.SectionsFn()
	return fmt.Sprintf("Which section?\n%s", strings.Join(sections, ", ")), nil
}

// LogCommand returns a /log command showing recent event log lines.
func LogCommand() *Command {
	return &Command{
		Name:        "log",
		Description: "Recent event log lines",
		Category:    "diagnostics",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			n := parseLineCount(req.Args, 20)
			result, err := tailFile(cc.EventLogPath, n, nil)
			if err != nil || result == "Log file not found." || result == "Log is empty." {
				return Response{Text: result}, err
			}
			return Response{Text: "```\n" + result + "\n```"}, nil
		},
	}
}

// ErrorsCommand returns a /errors command showing recent ERROR/WARN lines.
func ErrorsCommand() *Command {
	return &Command{
		Name:        "errors",
		Description: "Recent error/warning log lines",
		Category:    "diagnostics",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			n := parseLineCount(req.Args, 10)
			result, err := tailFile(cc.EventLogPath, n, func(line string) bool {
				return logLineLevel(line) == "ERROR" || logLineLevel(line) == "WARN"
			})
			if err != nil || result == "Log file not found." || result == "No matching lines." {
				return Response{Text: result}, err
			}
			return Response{Text: "```\n" + result + "\n```"}, nil
		},
	}
}

// HelpCommand returns a /help command that lists all registered commands.
// registry is needed to enumerate commands; pass it after registration.
func HelpCommand(registry *Registry) *Command {
	return &Command{
		Name:        "help",
		Description: "List available commands",
		Category:    "session",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
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
			return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
		},
	}
}

// VersionCommand returns a /version command.
func VersionCommand() *Command {
	return &Command{
		Name:        "version",
		Description: "Build version info",
		Category:    "diagnostics",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			return Response{Text: fmt.Sprintf("version: %s\ngo: %s\ncommit: %s\nbuilt: %s",
				cc.BuildInfo.Version, cc.BuildInfo.GoVersion, cc.BuildInfo.GitCommit, cc.BuildInfo.BuildTime)}, nil
		},
	}
}

// ReloadCommand returns a /reload command that reloads config, skills, and system prompt.
func ReloadCommand() *Command {
	return &Command{
		Name:        "reload",
		Description: "Reload config, skills, and system prompt from disk",
		Category:    "operations",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			cc.Bootstrap.Reload()
			if cc.Agent.NudgeReloadFunc != nil {
				cc.Agent.NudgeReloadFunc()
			}
			cc.Agent.InvalidateSystemCaches()
			newSkillRegistry := skills.Load(cc.SkillsDirs)
			var newExtraSystemBlocks []provider.SystemBlock
			if newSkillRegistry.Len() > 0 {
				newExtraSystemBlocks = []provider.SystemBlock{
					{Type: "text", Text: newSkillRegistry.SystemBlock(cc.AgentConfig.Workspace)},
				}
			}
			cc.Agent.ExtraSystemBlocks = newExtraSystemBlocks
			return Response{Text: fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: foci.toml config changes require a service restart to take effect. Prompt file changes take effect immediately.", newSkillRegistry.Len())}, nil
		},
	}
}

// CompactCommand creates a /compact command that triggers manual session compaction.
func CompactCommand() *Command {
	return &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "compact", Data: "run"},
				{Label: "dry-run", Data: "dry-run"},
			}
		},
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			dryRun := strings.TrimSpace(req.Args) == "dry-run"
			oldCount, err := runCompaction(ctx, cc, dryRun)
			if err != nil {
				return Response{}, err
			}
			if dryRun {
				return Response{Text: fmt.Sprintf("Dry-run complete — %d messages would be summarised. Summary sent.", oldCount)}, nil
			}
			return Response{Text: fmt.Sprintf("Context compacted — %d messages summarised.", oldCount)}, nil
		},
	}
}

// runCompaction executes manual context compaction.
func runCompaction(ctx context.Context, cc CommandContext, dryRun bool) (int, error) {
	if cc.Agent.Compactor == nil {
		return 0, fmt.Errorf("compaction is not configured")
	}
	sk := cc.DefaultSessionKey()
	if sk == "" {
		return 0, fmt.Errorf("no active session to compact")
	}
	mc, _ := cc.Sessions.MessageCount(sk)
	if mc < 5 {
		return 0, fmt.Errorf("too few messages to compact (%d)", mc)
	}
	if dryRun {
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, "⏳ Running compaction dry-run...")
		}
	} else {
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, "⏳ Compacting context...")
		}
	}

	system := cc.Bootstrap.SystemBlocks()
	summaryPrompt := prompts.ResolvePrompt(cc.Agent.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), cc.PromptSearchDirs...)
	handoffMsg := cc.Agent.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), cc.PromptSearchDirs...)
	}

	summary, newKey, err := cc.Agent.Compactor.Compact(ctx, cc.Agent.SessionClient(sk), sk, system, summaryPrompt, handoffMsg, dryRun)
	if err != nil {
		return 0, fmt.Errorf("compaction failed: %w", err)
	}

	if dryRun {
		if len(cc.Agent.CompactionDebugFunc) > 0 && summary != "" {
			for _, fn := range cc.Agent.CompactionDebugFunc {
				fn(sk, summary)
			}
		} else if summary != "" {
			if cc.ConnMgr != nil {
				if conn := cc.ConnMgr.Primary(cc.AgentConfig.ID); conn != nil {
					f, tmpErr := os.CreateTemp("", "compaction-dryrun-*.md")
					if tmpErr == nil {
						if _, writeErr := f.WriteString(summary); writeErr == nil {
							_ = f.Close()
							if sendErr := conn.SendDocument(f.Name()); sendErr != nil {
								log.Warnf("agent", "dry-run: send document: %v", sendErr)
							}
						} else {
							_ = f.Close()
						}
						_ = os.Remove(f.Name())
					}
				}
			}
		}
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, "✅ Dry-run complete — summary sent.")
		}
	} else {
		if newKey != "" {
			cc.Agent.RotateSession(sk, newKey)
		}
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
		}
		if summary != "" {
			for _, fn := range cc.Agent.CompactionDebugFunc {
				fn(sk, summary)
			}
		}
		cc.Bootstrap.Reload()
		cc.Agent.InvalidateSystemCaches()
		resetKey := sk
		if newKey != "" {
			resetKey = newKey
		}
		cc.Agent.ResetCacheBaseline(resetKey)
	}
	return mc, nil
}

// RestartCommand creates a /restart command that restarts the foci service.
func RestartCommand() *Command {
	return &Command{
		Name:        "restart",
		Description: "Restart the foci service",
		Category:    "operations",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			cmd := exec.Command("systemctl", "restart", "foci")
			if err := cmd.Start(); err != nil {
				return Response{}, fmt.Errorf("restart failed: %w", err)
			}
			return Response{Text: "Restarting..."}, nil
		},
	}
}

// ManaCommand returns a dynamic slash command for checking quota.
func ManaCommand(name string) *Command {
	return &Command{
		Name:        name,
		Description: "Check current " + name + " (quota remaining)",
		Category:    "observability",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			return Response{Text: manaCheck(ctx, req, cc, name)}, nil
		},
	}
}

// manaCheck fetches and formats the current mana/quota status.
func manaCheck(ctx context.Context, req Request, cc CommandContext, manaName string) string {
	emojis := []string{"🔮", "✨", "🌙", "⚡", "🪄", "💎", "🌟", "🔥", "🧿", "🪬", "💫", "🌀", "🎇"}
	// Deterministic selection based on time (second-level jitter is fine)
	emoji := emojis[time.Now().UnixNano()%int64(len(emojis))]
	displayName := strings.ToUpper(manaName[:1]) + manaName[1:]

	usageClient := cc.Agent.SessionUsageClient(req.SessionKey)
	if usageClient == nil {
		return fmt.Sprintf("%s %s: No usage data (provider does not support usage API)", emoji, displayName)
	}

	usageClient.Invalidate()
	usage, err := usageClient.GetUsage(ctx)
	if err != nil {
		return fmt.Sprintf("%s Error fetching %s: %v", emoji, displayName, err)
	}
	percent := mana.FormatPercent(usage)
	if percent == "" {
		return fmt.Sprintf("%s %s: unknown", emoji, displayName)
	}
	result := fmt.Sprintf("%s %s: %s remaining", emoji, displayName, percent)
	if reset := mana.FormatReset(usage); reset != "" {
		result += fmt.Sprintf(" (resets %s)", reset)
	}
	return result
}

// BuildInfo holds version and build information.
type BuildInfo struct {
	Version   string
	GoVersion string
	GitCommit string
	BuildTime string
}

// StatusInfo holds data for the /status command.
type StatusInfo struct {
	AgentID          string
	SessionKey       string
	MessageCount     int
	Model            string
	Uptime           time.Duration
	StartTime        time.Time
	AgentBusy        bool
	CreatedAt        string
	LastActivity     string
	ContextLimit     int     // model context window
	CompactThreshold float64 // e.g. 0.8
}

// StatusCommand returns a /status command showing dashboard overview.
func StatusCommand() *Command {
	return &Command{
		Name:        "status",
		Description: "Dashboard overview",
		Category:    "observability",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			sk := cc.DefaultSessionKey()
			model := cc.Agent.SessionModel(sk)
			mc := sessionMessageCount(cc, sk)

			status := "idle"
			if cc.Agent.IsProcessing() {
				status = "processing"
			}

			entries := readAPILog(cc.APILogPath)
			var sessionCost float64
			var sessionCalls int
			var contextTokens int
			for _, e := range entries {
				if e.Session == sk {
					sessionCost += e.CostUSD
					sessionCalls++
					if e.CallType == "conversation" || e.CallType == "" {
						contextTokens = e.Input + e.CacheRead + e.CacheWrite
					}
				}
			}

			contextLimit := compaction.ContextLimit(model)

			var sb strings.Builder
			fmt.Fprintf(&sb, "🤖 %s — %s\n", cc.AgentConfig.ID, model)
			sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

			created := cc.Sessions.CreatedAt(sk)
			if t, err := time.Parse(time.RFC3339, created); err == nil {
				created = t.Format("15:04 UTC")
			}
			active := cc.Sessions.LastActivity(sk)
			if t, err := time.Parse(time.RFC3339, active); err == nil {
				active = t.Format("15:04 UTC")
			}
			fmt.Fprintf(&sb, "📊 Session: %s\n", sk)
			fmt.Fprintf(&sb, "   Messages: %d | Status: %s\n", mc, status)
			fmt.Fprintf(&sb, "   Created: %s | Active: %s\n", created, active)

			fmt.Fprintf(&sb, "\n⏱️  Uptime: %s (started %s)\n",
				display.FormatDuration(time.Since(cc.StartTime)),
				cc.StartTime.UTC().Format("15:04:05Z"))

			if contextTokens > 0 && contextLimit > 0 {
				pct := float64(contextTokens) / float64(contextLimit) * 100
				threshTokens := int(float64(contextLimit) * cc.CompactionThreshold)
				remaining := threshTokens - contextTokens
				if remaining < 0 {
					remaining = 0
				}
				fmt.Fprintf(&sb, "\n📈 Context: %.1f%% (%s / %s tokens)\n",
					pct, display.FormatCommas(contextTokens), display.FormatCommas(contextLimit))
				fmt.Fprintf(&sb, "   Compaction at %.0f%% (%sk tokens remaining)\n",
					cc.CompactionThreshold*100, display.FormatCommas(remaining/1000))
			}

			if sessionCalls > 0 {
				fmt.Fprintf(&sb, "\n💰 Session cost: $%.2f eq. (%d calls)", sessionCost, sessionCalls)
			}

			return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
		},
	}
}

// sessionMessageCount returns the message count for a session key, logging errors.
func sessionMessageCount(cc CommandContext, key string) int {
	n, err := cc.Sessions.MessageCount(key)
	if err != nil {
		log.Warnf("main", "message count for %s: %v", key, err)
		return 0
	}
	return n
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
func logLineLevel(line string) string {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimSpace(fields[1])
}

// tailFile returns the last n lines from a file.
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
