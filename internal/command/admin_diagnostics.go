package command

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/tools"
)

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
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			sk := tools.SessionKeyFromContext(ctx)
			model := cc.Agent.SessionModel(sk)

			compacting := cc.Agent.IsCompacting(sk)
			status := "idle"
			if cc.Agent.IsTurnInFlight(session.SessionKeyBase(sk)) {
				status = "processing"
			} else if compacting {
				status = "compacting"
			}

			// Query all session stats from api.db — works for both API
			// and delegated (CC backend) sessions.
			stats, _ := log.QuerySessionStats(sk)

			var mc int
			var sessionCost float64
			var sessionCalls int
			var contextTokens int
			created := "n/a"
			active := "n/a"

			if stats != nil {
				mc = stats.TurnCount
				sessionCost = stats.TotalCost
				sessionCalls = stats.TotalCalls
				contextTokens = stats.ContextTokens
				if !stats.CreatedAt.IsZero() {
					created = stats.CreatedAt.Local().Format("15:04")
				}
				if !stats.LastActivity.IsZero() {
					active = stats.LastActivity.Local().Format("15:04")
				}
			}

			contextLimit := cc.Agent.SessionContextLimit(sk)

			var sb strings.Builder
			fmt.Fprintf(&sb, "🤖 %s — %s\n", cc.AgentConfig.ID, model)
			sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

			fmt.Fprintf(&sb, "📊 Session: %s\n", sk)
			fmt.Fprintf(&sb, "   Messages: %d | Status: %s\n", mc, status)
			fmt.Fprintf(&sb, "   Created: %s | Active: %s\n", created, active)

			// Permission mode — always shown so the user knows the current
			// permission posture. Empty session metadata = CC's baseline "default".
			pmDisplay := cc.Agent.SessionPermissionMode(sk)
			if pmDisplay == "" {
				pmDisplay = "default"
			}
			fmt.Fprintf(&sb, "   Mode: %s\n", pmDisplay)

			fmt.Fprintf(&sb, "\n⏱️  Uptime: %s (started %s)\n",
				display.FormatDuration(time.Since(cc.StartTime)),
				timeutil.Format(cc.StartTime))

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

			// Backend liveness for delegated agents.
			if cc.Agent.DelegatedManager != nil {
				if info := cc.Agent.DelegatedManager.BackendInfo(sk, compacting); info != "" {
					fmt.Fprintf(&sb, "\n\n🔌 Backend: %s", info)
				}
			}

			return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
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
func manaCheck(ctx context.Context, _ Request, cc CommandContext, manaName string) string {
	emojis := []string{"🔮", "✨", "🌙", "⚡", "🪄", "💎", "🌟", "🔥", "🧿", "🪬", "💫", "🌀", "🎇"}
	// Deterministic selection based on time (second-level jitter is fine)
	emoji := emojis[time.Now().UnixNano()%int64(len(emojis))]
	displayName := strings.ToUpper(manaName[:1]) + manaName[1:]

	usageClient := cc.Agent.SessionUsageClient(tools.SessionKeyFromContext(ctx))
	if usageClient == nil {
		return fmt.Sprintf("%s %s: No usage data (provider does not support usage API)", emoji, displayName)
	}

	usageClient.Invalidate()
	w, err := usageClient.GetUsage(ctx)
	if err != nil {
		return fmt.Sprintf("%s Error fetching %s: %v", emoji, displayName, err)
	}
	percent := mana.FormatPercent(w)
	if percent == "" {
		return fmt.Sprintf("%s %s: unknown", emoji, displayName)
	}
	result := fmt.Sprintf("%s %s: %s remaining", emoji, displayName, percent)
	if reset := mana.FormatReset(w); reset != "" {
		result += fmt.Sprintf(" (resets %s)", reset)
	}
	if w != nil && w.ExtraInfo != "" {
		result += "\n" + w.ExtraInfo
	}
	return result
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
