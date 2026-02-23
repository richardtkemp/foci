package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ChildSysProcAttr is called to get the SysProcAttr for child processes.
// Set this from main to drop supplementary groups (clod-secrets).
// If nil, defaults to {Setpgid: true}.
var ChildSysProcAttr func() *syscall.SysProcAttr

func childSysProcAttr() *syscall.SysProcAttr {
	if ChildSysProcAttr != nil {
		return ChildSysProcAttr()
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

// LastMessageStore tracks the last message received from each user.
// Used by the // (repeat) command to re-send the previous message.
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
		Name:           "repeat",
		Description:    "Repeat your last message (command: //)",
		SkipToolExport: true,
		Hidden:         true,
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

// apiEntry mirrors log.APIEntry for reading api.jsonl without importing log.
type apiEntry struct {
	Timestamp  time.Time `json:"ts"`
	Session    string    `json:"session"`
	Model      string    `json:"model"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cache_read"`
	CacheWrite int       `json:"cache_write"`
	CostUSD    float64   `json:"cost_usd"`
	DurationMS int64     `json:"duration_ms"`
	StopReason string    `json:"stop_reason"`
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

// NewPingCommand returns a /ping command.
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

// NewStatusCommand returns a /status command showing a dashboard overview.
func NewStatusCommand(statusFn func() StatusInfo, apiLogPath string) *Command {
	return &Command{
		Name:        "status",
		Description: "Dashboard overview",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			info := statusFn()

			status := "idle"
			if info.AgentBusy {
				status = "processing"
			}

			// Compute session cost and context from API log
			entries := readAPILog(apiLogPath)
			var sessionCost float64
			var sessionCalls int
			var contextTokens int
			for _, e := range entries {
				if e.Session == info.SessionKey {
					sessionCost += e.CostUSD
					sessionCalls++
					contextTokens = e.Input + e.CacheRead + e.CacheWrite
				}
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "🤖 %s — %s\n", info.AgentID, info.Model)
			sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

			// Session
			created := info.CreatedAt
			if t, err := time.Parse(time.RFC3339, created); err == nil {
				created = t.Format("15:04 UTC")
			}
			active := info.LastActivity
			if t, err := time.Parse(time.RFC3339, active); err == nil {
				active = t.Format("15:04 UTC")
			}
			fmt.Fprintf(&sb, "📊 Session: %s\n", info.SessionKey)
			fmt.Fprintf(&sb, "   Messages: %d | Status: %s\n", info.MessageCount, status)
			fmt.Fprintf(&sb, "   Created: %s | Active: %s\n", created, active)

			// Uptime
			fmt.Fprintf(&sb, "\n⏱️  Uptime: %s (started %s)\n",
				formatDuration(info.Uptime),
				info.StartTime.UTC().Format("15:04:05Z"))

			// Context
			if contextTokens > 0 && info.ContextLimit > 0 {
				pct := float64(contextTokens) / float64(info.ContextLimit) * 100
				threshTokens := int(float64(info.ContextLimit) * info.CompactThreshold)
				remaining := threshTokens - contextTokens
				if remaining < 0 {
					remaining = 0
				}
				fmt.Fprintf(&sb, "\n📈 Context: %.1f%% (%s / %s tokens)\n",
					pct, formatCommas(contextTokens), formatCommas(info.ContextLimit))
				fmt.Fprintf(&sb, "   Compaction at %.0f%% (%sk tokens remaining)\n",
					info.CompactThreshold*100, formatCommas(remaining/1000))
			}

			// Cost
			if sessionCalls > 0 {
				fmt.Fprintf(&sb, "\n💰 Session cost: $%.2f eq. (%d calls)", sessionCost, sessionCalls)
			}

			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// formatCommas formats an integer with comma separators (e.g. 32793 → "32,793").
func formatCommas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + formatCommas(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// NewCacheCommand returns a /cache command showing recent cache hit/miss breakdown.
func NewCacheCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "cache",
		Description: "Last 5 API calls with cache breakdown",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			entries := readAPILog(apiLogPath)
			if len(entries) == 0 {
				return "No API calls logged yet.", nil
			}

			// Take last 5
			start := 0
			if len(entries) > 5 {
				start = len(entries) - 5
			}
			recent := entries[start:]

			// Compute average hit rate across recent entries
			var totalCacheRead, totalInput int
			for _, e := range recent {
				totalCacheRead += e.CacheRead
				totalInput += e.Input + e.CacheRead + e.CacheWrite
			}
			avgHit := 0.0
			if totalInput > 0 {
				avgHit = float64(totalCacheRead) / float64(totalInput) * 100
			}

			// Pre-compute per-row values for column width measurement
			type cacheRow struct {
				time     string
				input    string
				cRead    string
				cWrite   string
				cost     string
				hitPct   string
			}
			rows := make([]cacheRow, len(recent))
			for i, e := range recent {
				hitRate := 0.0
				inp := e.Input + e.CacheRead + e.CacheWrite
				if inp > 0 {
					hitRate = float64(e.CacheRead) / float64(inp) * 100
				}
				rows[i] = cacheRow{
					time:   e.Timestamp.Format("15:04:05"),
					input:  formatCommas(e.Input),
					cRead:  formatCommas(e.CacheRead),
					cWrite: formatCommas(e.CacheWrite),
					cost:   fmt.Sprintf("$%.3f", e.CostUSD),
					hitPct: fmt.Sprintf("%.0f%%", hitRate),
				}
			}

			// Measure column widths
			inW, crW, cwW, costW, hitW := len("Input"), len("CacheRead"), len("CacheWrite"), len("Cost"), len("Hit%")
			for _, r := range rows {
				if len(r.input) > inW { inW = len(r.input) }
				if len(r.cRead) > crW { crW = len(r.cRead) }
				if len(r.cWrite) > cwW { cwW = len(r.cWrite) }
				if len(r.cost) > costW { costW = len(r.cost) }
				if len(r.hitPct) > hitW { hitW = len(r.hitPct) }
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Cache — last %d calls (avg %.1f%% hit)\n", len(recent), avgHit)

			// time column is fixed 8 chars
			sep := strings.Repeat("─", 8+2+inW+2+crW+2+cwW+2+costW+2+hitW)
			b.WriteString("\n```\n")
			fmt.Fprintf(&b, "%-8s  %*s  %*s  %*s  %*s  %*s\n",
				"Time", inW, "Input", crW, "CacheRead", cwW, "CacheWrite", costW, "Cost", hitW, "Hit%")
			b.WriteString(sep + "\n")
			for _, r := range rows {
				fmt.Fprintf(&b, "%-8s  %*s  %*s  %*s  %*s  %*s\n",
					r.time, inW, r.input, crW, r.cRead, cwW, r.cWrite, costW, r.cost, hitW, r.hitPct)
			}
			b.WriteString(sep + "\n")
			b.WriteString("```")
			return b.String(), nil
		},
	}
}

// NewLastCommand returns a /last command showing the most recent API call.
func NewLastCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "last",
		Description: "Last API request details",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			entries := readAPILog(apiLogPath)
			if len(entries) == 0 {
				return "No API calls logged yet.", nil
			}

			e := entries[len(entries)-1]
			return fmt.Sprintf("time: %s\nmodel: %s\nstop: %s\ntokens: in=%d out=%d cache_read=%d cache_write=%d\nduration: %dms\ncost: $%.4f\nsession: %s",
				e.Timestamp.Format(time.RFC3339), e.Model, e.StopReason,
				e.Input, e.Output, e.CacheRead, e.CacheWrite,
				e.DurationMS, e.CostUSD, e.Session), nil
		},
	}
}

// NewCostCommand returns a /cost command showing aggregated costs.
func NewCostCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "cost",
		Description: "API cost summary",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			entries := readAPILog(apiLogPath)
			if len(entries) == 0 {
				return "No API calls logged yet.", nil
			}

			scope := strings.ToLower(strings.TrimSpace(args))

			switch scope {
			case "", "today", "session":
				// Default: today's total with per-session breakdown
				today := time.Now().UTC().Format("2006-01-02")
				var total float64
				var count int
				costs := make(map[string]float64)
				counts := make(map[string]int)
				for _, e := range entries {
					if e.Timestamp.Format("2006-01-02") == today {
						total += e.CostUSD
						count++
						costs[e.Session] += e.CostUSD
						counts[e.Session]++
					}
				}

				var b strings.Builder
				fmt.Fprintf(&b, "💰 Today: $%.2f eq. (%s calls)\n", total, formatCommas(count))

				if len(costs) > 0 {
					// Sort sessions by cost descending
					type sessionCost struct {
						name  string
						cost  float64
						calls int
					}
					sorted := make([]sessionCost, 0, len(costs))
					for s, c := range costs {
						sorted = append(sorted, sessionCost{s, c, counts[s]})
					}
					for i := 0; i < len(sorted)-1; i++ {
						for j := i + 1; j < len(sorted); j++ {
							if sorted[j].cost > sorted[i].cost {
								sorted[i], sorted[j] = sorted[j], sorted[i]
							}
						}
					}

					// Limit to top 10
					shown := sorted
					extra := 0
					if len(sorted) > 10 {
						shown = sorted[:10]
						extra = len(sorted) - 10
					}

					// Measure column widths
					nameW := len("Session")
					costW := len("Cost")
					callsW := len("Calls")
					for _, sc := range shown {
						if len(sc.name) > nameW {
							nameW = len(sc.name)
						}
						cs := fmt.Sprintf("$%.2f", sc.cost)
						if len(cs) > costW {
							costW = len(cs)
						}
						cc := formatCommas(sc.calls)
						if len(cc) > callsW {
							callsW = len(cc)
						}
					}
					// Total row widths
					ts := fmt.Sprintf("$%.2f", total)
					if len(ts) > costW {
						costW = len(ts)
					}
					tc := formatCommas(count)
					if len(tc) > callsW {
						callsW = len(tc)
					}
					if len("Total") > nameW {
						nameW = len("Total")
					}

					sep := strings.Repeat("─", nameW+2+costW+2+callsW)
					b.WriteString("\n```\n")
					fmt.Fprintf(&b, "%-*s  %*s  %*s\n", nameW, "Session", costW, "Cost", callsW, "Calls")
					b.WriteString(sep + "\n")
					for _, sc := range shown {
						fmt.Fprintf(&b, "%-*s  %*s  %*s\n",
							nameW, sc.name,
							costW, fmt.Sprintf("$%.2f", sc.cost),
							callsW, formatCommas(sc.calls))
					}
					if extra > 0 {
						fmt.Fprintf(&b, "  +%d more\n", extra)
					}
					b.WriteString(sep + "\n")
					fmt.Fprintf(&b, "%-*s  %*s  %*s\n",
						nameW, "Total",
						costW, fmt.Sprintf("$%.2f", total),
						callsW, formatCommas(count))
					b.WriteString("```")
				}
				return b.String(), nil

			default:
				// Try parsing as number of days
				days, err := strconv.Atoi(scope)
				if err != nil {
					return "Usage: /cost [today|<days>]", nil
				}
				cutoff := time.Now().UTC().AddDate(0, 0, -days)
				var total float64
				var count int
				for _, e := range entries {
					if e.Timestamp.After(cutoff) {
						total += e.CostUSD
						count++
					}
				}
				return fmt.Sprintf("Last %d days: $%.4f (%d API calls)", days, total, count), nil
			}
		},
	}
}

// NewResetCommand returns a /reset command that clears session history.
// resetFn performs the actual reset; confirmFn asks for confirmation first.
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
// getModel returns current model; setModel switches it.
func NewModelCommand(getModel func() string, setModel func(string)) *Command {
	return &Command{
		Name:        "model",
		Description: "Show or switch model",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return fmt.Sprintf("Current model: %s", getModel()), nil
			}
			setModel(args)
			return fmt.Sprintf("Model switched to: %s", args), nil
		},
	}
}

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
			var b strings.Builder
			for _, t := range tools {
				fmt.Fprintf(&b, "• %s — %s\n", t.Name, t.Description)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// NewConfigCommand returns a /config command dumping the running config.
// configFn returns the config as a string with secrets redacted.
func NewConfigCommand(configFn func() string) *Command {
	return &Command{
		Name:        "config",
		Description: "Show running config (secrets redacted)",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			return configFn(), nil
		},
	}
}

// NewLogCommand returns a /log command showing recent event log lines.
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
			return tailFile(eventLogPath, n)
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
			return tailFileFiltered(eventLogPath, n, func(line string) bool {
				return strings.Contains(line, "ERROR") || strings.Contains(line, "WARN")
			})
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
				cmds  []*Command
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

// BuildInfo holds data for the /version command.
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

// NewVoiceCommand returns a /voice command to toggle voice mode.
func NewVoiceCommand(getVoice func() bool, setVoice func(bool)) *Command {
	return &Command{
		Name:        "voice",
		Description: "Toggle voice mode (replies sent as voice notes)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			current := getVoice()
			setVoice(!current)
			if !current {
				return "Voice mode ON — replies will be sent as voice notes.", nil
			}
			return "Voice mode OFF — replies will be sent as text.", nil
		},
	}
}

// NewMultiballCommand returns a /multiball command that forks the current session to a secondary bot.
// forkFn does the actual branch creation, bot acquisition, and notification.
func NewMultiballCommand(forkFn func() (string, error)) *Command {
	return &Command{
		Name:        "multiball",
		Description: "Fork session to a secondary bot",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			return forkFn()
		},
	}
}

// NewUsageCommand returns a /usage command that checks Claude subscription usage.
// usageFn is a callback that fetches the usage data (avoids import coupling).
func NewUsageCommand(usageFn func(context.Context) (string, error)) *Command {
	return &Command{
		Name:        "usage",
		Description: "Check Claude subscription usage and rate limits",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			return usageFn(ctx)
		},
	}
}

// NewManaCommand returns a dynamic slash command for checking quota.
// The command name is configurable (e.g., /mana, /juice, /credits).
func NewManaCommand(name string, manaFn func(context.Context) (string, error)) *Command {
	return &Command{
		Name:        name,
		Description: "Check current " + name + " (quota remaining)",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			return manaFn(ctx)
		},
	}
}

// ContextInfo holds data for the /context command.
type ContextInfo struct {
	SessionKey       string
	Model            string
	CompactionThresh float64
	ContextLimit     int
}

// NewContextCommand returns a /context command showing context size breakdown.
func NewContextCommand(apiLogPath string, infoFn func() ContextInfo) *Command {
	return &Command{
		Name:        "context",
		Description: "Context size and compaction threshold",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			info := infoFn()

			// Get last API call for this session
			entries := readAPILog(apiLogPath)
			var lastInput, lastCacheRead, lastCacheWrite int
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Session == info.SessionKey {
					lastInput = entries[i].Input
					lastCacheRead = entries[i].CacheRead
					lastCacheWrite = entries[i].CacheWrite
					break
				}
			}

			totalTokens := lastInput + lastCacheRead + lastCacheWrite
			if totalTokens == 0 {
				return "No API calls yet for this session.", nil
			}

			threshTokens := int(float64(info.ContextLimit) * info.CompactionThresh)
			percentUsed := float64(totalTokens) / float64(info.ContextLimit) * 100
			percentThresh := info.CompactionThresh * 100

			var status string
			if totalTokens >= threshTokens {
				status = "at/above threshold"
			} else {
				remaining := threshTokens - totalTokens
				status = fmt.Sprintf("%d tokens until threshold", remaining)
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "model: %s\n", info.Model)
			fmt.Fprintf(&sb, "context: %d / %d tokens (%.1f%%)\n", totalTokens, info.ContextLimit, percentUsed)
			fmt.Fprintf(&sb, "breakdown:\n")
			fmt.Fprintf(&sb, "  input: %d\n", lastInput)
			fmt.Fprintf(&sb, "  cache_read: %d\n", lastCacheRead)
			fmt.Fprintf(&sb, "  cache_write: %d\n", lastCacheWrite)
			fmt.Fprintf(&sb, "compaction: at %.0f%% (%d tokens)\n", percentThresh, threshTokens)
			fmt.Fprintf(&sb, "status: %s", status)

			return sb.String(), nil
		},
	}
}

// NewReloadCommand returns a /reload command that reloads config and system files.
// reloadFn is a callback that performs the reload (avoids import coupling).
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

// tailFile returns the last n lines from a file.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "Log file not found.", nil
	}
	defer f.Close()

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
	defer f.Close()

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

// NewScriptCommand creates a command that runs a shell script and returns stdout.
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

// readAPILog reads all entries from an api.jsonl file.
func readAPILog(path string) []apiEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []apiEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e apiEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
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
func NewAgentsCommand(listFn func() []AgentInfo) *Command {
	return &Command{
		Name:        "agents",
		Description: "List active agent sessions",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
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

			// Measure column widths
			idW, sessW, statW, modW, msgW := len("ID"), len("Session"), len("Status"), len("Model"), len("Messages")
			for _, r := range rows {
				if len(r.id) > idW { idW = len(r.id) }
				if len(r.session) > sessW { sessW = len(r.session) }
				if len(r.status) > statW { statW = len(r.status) }
				if len(r.model) > modW { modW = len(r.model) }
				if len(r.msgs) > msgW { msgW = len(r.msgs) }
			}

			sep := strings.Repeat("─", idW+2+sessW+2+statW+2+modW+2+msgW)
			var sb strings.Builder
			sb.WriteString("Agents\n\n```\n")
			fmt.Fprintf(&sb, "%-*s  %-*s  %-*s  %-*s  %*s\n",
				idW, "ID", sessW, "Session", statW, "Status", modW, "Model", msgW, "Messages")
			sb.WriteString(sep + "\n")
			for _, r := range rows {
				fmt.Fprintf(&sb, "%-*s  %-*s  %-*s  %-*s  %*s\n",
					idW, r.id, sessW, r.session, statW, r.status, modW, r.model, msgW, r.msgs)
			}
			sb.WriteString(sep + "\n")
			sb.WriteString("```")
			return sb.String(), nil
		},
	}
}

// NewCompactCommand creates a /compact command that triggers manual session compaction.
// compactFn performs the compaction and returns the old message count, or an error.
func NewCompactCommand(compactFn func(ctx context.Context) (int, error)) *Command {
	return &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			oldCount, err := compactFn(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Context compacted — %d messages summarised.", oldCount), nil
		},
	}
}

// NewRestartCommand creates a /restart command that restarts the clod service.
// notifyFn is called before the restart to send a notification (e.g., Telegram).
func NewRestartCommand(notifyFn func(string)) *Command {
	return &Command{
		Name:        "restart",
		Description: "Restart the clod service",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if notifyFn != nil {
				notifyFn("Restarting...")
			}

			cmd := exec.Command("systemctl", "restart", "clod")
			if err := cmd.Start(); err != nil {
				return "", fmt.Errorf("restart failed: %w", err)
			}
			// Don't wait — process will be killed by systemd
			return "Restarting...", nil
		},
	}
}

// SecretsStore is the interface needed by the /secrets command.
type SecretsStore interface {
	Names() []string
	Set(name, value string)
	Remove(name string) bool
	Save() error
}

// NewSecretsCommand creates the /secrets slash command for managing secrets.
// CLI-only — must be registered with SkipToolExport=true.
func NewSecretsCommand(store SecretsStore) *Command {
	return &Command{
		Name:           "secrets",
		Description:    "Manage secrets (list/set/remove)",
		Category:       "operations",
		SkipToolExport: true,
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			if len(parts) == 0 {
				return "Usage: /secrets list | /secrets set <section.key> <value> | /secrets remove <section.key>", nil
			}

			switch parts[0] {
			case "list":
				names := store.Names()
				if len(names) == 0 {
					return "No secrets configured.", nil
				}
				// Group by section
				sections := make(map[string][]string)
				var order []string
				for _, name := range names {
					p := strings.SplitN(name, ".", 2)
					sec := p[0]
					key := name
					if len(p) == 2 {
						key = p[1]
					}
					if _, seen := sections[sec]; !seen {
						order = append(order, sec)
					}
					sections[sec] = append(sections[sec], key)
				}
				var lines []string
				for _, sec := range order {
					lines = append(lines, fmt.Sprintf("[%s]", sec))
					for _, key := range sections[sec] {
						lines = append(lines, fmt.Sprintf("  %s", key))
					}
				}
				return strings.Join(lines, "\n"), nil

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
				return "Usage: /secrets list | /secrets set <section.key> <value> | /secrets remove <section.key>", nil
			}
		},
	}
}
