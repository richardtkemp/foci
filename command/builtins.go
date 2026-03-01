package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"foci/table"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

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
	Timestamp    time.Time `json:"ts"`
	Session      string    `json:"session"`
	Model        string    `json:"model"`
	Input        int       `json:"input"`
	Output       int       `json:"output"`
	CacheRead    int       `json:"cache_read"`
	CacheWrite   int       `json:"cache_write"`
	CostUSD      float64   `json:"cost_usd"`
	DurationMS   int64     `json:"duration_ms"`
	StopReason   string    `json:"stop_reason"`
	IsCompaction bool      `json:"is_compaction"`
}

// categoryCosts computes per-category cost breakdown from API log entries.
// Duplicates pricing from log.CalculateCost since command can't import log.
func categoryCosts(entries []apiEntry) (cacheRead, cacheWrite, input, output float64) {
	type pricing struct{ input, output, cacheRead, cacheWrite float64 }
	prices := map[string]pricing{
		"claude-haiku-4-5":  {1.00, 5.00, 0.10, 1.25},
		"claude-sonnet-4-5": {3.00, 15.00, 0.30, 3.75},
		"claude-opus-4-6":   {15.00, 75.00, 1.50, 18.75},
	}
	mtok := 1_000_000.0
	for _, e := range entries {
		p := prices[e.Model]
		if p == (pricing{}) {
			p = prices["claude-haiku-4-5"]
		}
		cacheRead += float64(e.CacheRead) / mtok * p.cacheRead
		cacheWrite += float64(e.CacheWrite) / mtok * p.cacheWrite
		input += float64(e.Input) / mtok * p.input
		output += float64(e.Output) / mtok * p.output
	}
	return
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
					if !e.IsCompaction {
						contextTokens = e.Input + e.CacheRead + e.CacheWrite
					}
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
				time   string
				input  string
				cRead  string
				cWrite string
				cost   string
				hitPct string
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

			cols := []table.Column{
				{Header: "Time"},
				{Header: "Input", Align: table.AlignRight},
				{Header: "CacheRead", Align: table.AlignRight},
				{Header: "CacheWrite", Align: table.AlignRight},
				{Header: "Cost", Align: table.AlignRight},
				{Header: "Hit%", Align: table.AlignRight},
			}
			tableRows := make([][]string, len(rows))
			for i, r := range rows {
				tableRows[i] = []string{r.time, r.input, r.cRead, r.cWrite, r.cost, r.hitPct}
			}
			return fmt.Sprintf("Cache — last %d calls (avg %.1f%% hit)\n\n```\n%s\n```",
				len(recent), avgHit, table.Format(cols, tableRows)), nil
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
			case "":
				return "/cost today — today's costs by session\n/cost 24h — last 24 hours with category breakdown\n/cost week — 7-day summary with daily breakdown\n/cost <days> — total for last N days", nil

			case "today", "session":
				// Today's total with per-session breakdown
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

					cols := []table.Column{
						{Header: "Session"},
						{Header: "Cost", Align: table.AlignRight},
						{Header: "Calls", Align: table.AlignRight},
					}
					tableRows := make([][]string, 0, len(shown)+1)
					for _, sc := range shown {
						tableRows = append(tableRows, []string{
							sc.name,
							fmt.Sprintf("$%.2f", sc.cost),
							formatCommas(sc.calls),
						})
					}
					if extra > 0 {
						tableRows = append(tableRows, []string{fmt.Sprintf("  +%d more", extra), "", ""})
					}
					tableRows = append(tableRows, []string{"Total", fmt.Sprintf("$%.2f", total), formatCommas(count)})
					b.WriteString("\n```\n")
					b.WriteString(table.Format(cols, tableRows))
					b.WriteString("\n```")
				}
				return b.String(), nil

			case "24h":
				cutoff := time.Now().UTC().Add(-24 * time.Hour)
				var filtered []apiEntry
				for _, e := range entries {
					if e.Timestamp.After(cutoff) {
						filtered = append(filtered, e)
					}
				}
				var total float64
				for _, e := range filtered {
					total += e.CostUSD
				}
				cr, cw, inp, out := categoryCosts(filtered)
				var b strings.Builder
				fmt.Fprintf(&b, "API cost (last 24h): $%.2f eq.\n", total)
				b.WriteString("\n```\n")
				// Category table
				type catRow struct {
					name string
					cost float64
				}
				cats := []catRow{
					{"Cache reads", cr}, {"Cache writes", cw},
					{"Input", inp}, {"Output", out},
				}
				nameW := len("Category")
				costW := len("Cost")
				for _, c := range cats {
					if len(c.name) > nameW {
						nameW = len(c.name)
					}
					cs := fmt.Sprintf("$%.2f", c.cost)
					if len(cs) > costW {
						costW = len(cs)
					}
				}
				ts := fmt.Sprintf("$%.2f", total)
				if len(ts) > costW {
					costW = len(ts)
				}
				if len("Total") > nameW {
					nameW = len("Total")
				}
				sep := strings.Repeat("─", nameW+2+costW)
				fmt.Fprintf(&b, "%-*s  %*s\n", nameW, "Category", costW, "Cost")
				b.WriteString(sep + "\n")
				for _, c := range cats {
					fmt.Fprintf(&b, "%-*s  %*s\n", nameW, c.name, costW, fmt.Sprintf("$%.2f", c.cost))
				}
				b.WriteString(sep + "\n")
				fmt.Fprintf(&b, "%-*s  %*s\n", nameW, "Total", costW, fmt.Sprintf("$%.2f", total))
				b.WriteString("```")
				return b.String(), nil

			case "week":
				now := time.Now().UTC()
				startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
				cutoff := startOfToday.AddDate(0, 0, -6) // 7 days: today + 6 prior
				var filtered []apiEntry
				for _, e := range entries {
					if !e.Timestamp.Before(cutoff) {
						filtered = append(filtered, e)
					}
				}
				// Group by date
				dayCosts := make(map[string]float64)
				var total float64
				for _, e := range filtered {
					day := e.Timestamp.Format("2006-01-02")
					dayCosts[day] += e.CostUSD
					total += e.CostUSD
				}
				mean := total / 7.0

				var b strings.Builder
				fmt.Fprintf(&b, "API cost (7-day summary): $%.2f eq. (mean $%.2f/day)\n", total, mean)
				b.WriteString("\n```\n")
				// Day table
				dateW := len("2006-01-02")
				costW := len("Cost")
				for i := 0; i < 7; i++ {
					day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
					cs := fmt.Sprintf("$%.2f", dayCosts[day])
					if len(cs) > costW {
						costW = len(cs)
					}
				}
				ts := fmt.Sprintf("$%.2f", total)
				if len(ts) > costW {
					costW = len(ts)
				}
				ms := fmt.Sprintf("$%.2f", mean)
				if len(ms) > costW {
					costW = len(ms)
				}
				sep := strings.Repeat("─", dateW+2+costW)
				fmt.Fprintf(&b, "%-*s  %*s\n", dateW, "Date", costW, "Cost")
				b.WriteString(sep + "\n")
				for i := 0; i < 7; i++ {
					day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
					fmt.Fprintf(&b, "%-*s  %*s\n", dateW, day, costW, fmt.Sprintf("$%.2f", dayCosts[day]))
				}
				b.WriteString(sep + "\n")
				fmt.Fprintf(&b, "%-*s  %*s\n", dateW, "Total", costW, fmt.Sprintf("$%.2f", total))
				fmt.Fprintf(&b, "%-*s  %*s\n", dateW, "Mean/day", costW, fmt.Sprintf("$%.2f", mean))
				b.WriteString("```")
				return b.String(), nil

			default:
				// Try parsing as number of days
				days, err := strconv.Atoi(scope)
				if err != nil {
					return "Usage: /cost [today|24h|week|<days>]", nil
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
// getModel returns current model; setModel switches it; resolveModel resolves aliases.
// Callbacks receive the command's context so callers can resolve per-session state.
func NewModelCommand(getModel func(context.Context) string, setModel func(context.Context, string), resolveModel func(string) string) *Command {
	return &Command{
		Name:        "model",
		Description: "Show or switch model",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return fmt.Sprintf("Current model: %s", getModel(ctx)), nil
			}
			resolved := resolveModel(args)
			setModel(ctx, resolved)
			return fmt.Sprintf("Model switched to: %s", resolved), nil
		},
	}
}

// NewEffortCommand returns a /effort command to show or set the effort level.
// getEffort returns current effort; setEffort changes it (runtime only).
// Callbacks receive the command's context so callers can resolve per-session state.
func NewEffortCommand(getEffort func(context.Context) string, setEffort func(context.Context, string)) *Command {
	return &Command{
		Name:        "effort",
		Description: "Show or set effort level (low/medium/high)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			const optionsLine = "Options: 1) low  2) medium  3) high"
			if args == "" {
				e := getEffort(ctx)
				if e == "" {
					return "Effort: not set (using API default)\n" + optionsLine, nil
				}
				return fmt.Sprintf("Effort: %s\n%s", e, optionsLine), nil
			}
			arg := strings.ToLower(strings.TrimSpace(args))
			// Accept numeric aliases
			switch arg {
			case "1":
				arg = "low"
			case "2":
				arg = "medium"
			case "3":
				arg = "high"
			}
			switch arg {
			case "low", "medium", "high":
				setEffort(ctx, arg)
				return fmt.Sprintf("Effort set to: %s", arg), nil
			case "none", "off", "":
				setEffort(ctx, "")
				return "Effort cleared (using API default)", nil
			default:
				return fmt.Sprintf("Invalid effort level: %q\n%s", args, optionsLine), nil
			}
		},
	}
}

// NewThinkingCommand returns a /thinking command to show or set the thinking mode.
// getThinking returns current mode; setThinking changes it (runtime only).
// Callbacks receive the command's context so callers can resolve per-session state.
func NewThinkingCommand(getThinking func(context.Context) string, setThinking func(context.Context, string)) *Command {
	return &Command{
		Name:        "thinking",
		Description: "Show or set thinking mode (off/adaptive)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			const optionsLine = "Options: 0) off  1) adaptive"
			if args == "" {
				t := getThinking(ctx)
				if t == "" || t == "off" {
					return "Thinking: off\n" + optionsLine, nil
				}
				return fmt.Sprintf("Thinking: %s\n%s", t, optionsLine), nil
			}
			arg := strings.ToLower(strings.TrimSpace(args))
			switch arg {
			case "0":
				arg = "off"
			case "1":
				arg = "adaptive"
			}
			switch arg {
			case "off", "none":
				setThinking(ctx, "")
				return "Thinking: off", nil
			case "adaptive":
				setThinking(ctx, "adaptive")
				return "Thinking: adaptive", nil
			default:
				return fmt.Sprintf("Invalid thinking mode: %q\n%s", args, optionsLine), nil
			}
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
			cols := []table.Column{
				{Header: "Name"},
				{Header: "Description"},
			}
			tableRows := make([][]string, len(tools))
			for i, t := range tools {
				tableRows[i] = []string{t.Name, t.Description}
			}
			return "```\n" + table.Format(cols, tableRows) + "\n```", nil
		},
	}
}

// NewConfigCommand returns a /config command dumping the running config.
// configFn receives the subcommand args ("toml", "available", or "") and
// returns the formatted config with secrets redacted.
func NewConfigCommand(configFn func(args string) (string, error)) *Command {
	return &Command{
		Name:        "config",
		Description: "Show running config. Subcommands: toml, table, available",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			return configFn(args)
		},
	}
}

// PromptInfo describes one configured prompt path/value.
type PromptInfo struct {
	Label  string // e.g. "compaction_summary"
	Path   string // file path, or "" if inline/default
	Inline string // inline value (for handoff_msg)
	Exists bool   // whether the file exists on disk
}

// PromptFile describes a prompt file found on disk.
type PromptFile struct {
	Dir        string // parent directory
	Name       string // filename
	Configured bool   // true if referenced by config
}

// PromptsData holds all data for the /prompts command.
type PromptsData struct {
	AgentID    string
	Prompts    []PromptInfo
	PromptDirs []string     // directories scanned
	Files      []PromptFile // files found on disk
}

// NewPromptsCommand returns a /prompts command showing prompt config and files.
func NewPromptsCommand(dataFn func() PromptsData) *Command {
	return &Command{
		Name:        "prompts",
		Description: "Show configured prompts and prompt files on disk",
		Category:    "diagnostics",
		Execute: func(ctx context.Context, args string) (string, error) {
			data := dataFn()
			var sb strings.Builder

			// Configured prompts
			sb.WriteString("```\n")
			fmt.Fprintf(&sb, "Configured prompts (agent: %s):\n", data.AgentID)

			maxLabel := 0
			for _, p := range data.Prompts {
				if len(p.Label) > maxLabel {
					maxLabel = len(p.Label)
				}
			}
			for _, p := range data.Prompts {
				if p.Inline != "" {
					fmt.Fprintf(&sb, "  %-*s  [inline: %d chars]\n", maxLabel, p.Label, len(p.Inline))
				} else if p.Path == "" {
					fmt.Fprintf(&sb, "  %-*s  [default]\n", maxLabel, p.Label)
				} else if p.Exists {
					fmt.Fprintf(&sb, "  %-*s  %s  ✓\n", maxLabel, p.Label, p.Path)
				} else {
					fmt.Fprintf(&sb, "  %-*s  %s  ✗ (not found)\n", maxLabel, p.Label, p.Path)
				}
			}
			sb.WriteString("```")

			// Files on disk
			if len(data.Files) > 0 {
				sb.WriteString("\n\n```\n")
				sb.WriteString("Prompt files on disk:\n")
				currentDir := ""
				for _, f := range data.Files {
					if f.Dir != currentDir {
						currentDir = f.Dir
						fmt.Fprintf(&sb, "  %s/\n", f.Dir)
					}
					tag := "[cron/other]"
					if f.Configured {
						tag = "[configured]"
					}
					fmt.Fprintf(&sb, "    %-36s %s\n", f.Name, tag)
				}
				sb.WriteString("```")
			} else if len(data.PromptDirs) > 0 {
				sb.WriteString("\n\nNo prompt files found on disk.")
			}

			return sb.String(), nil
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
				return strings.Contains(line, "ERROR") || strings.Contains(line, "WARN")
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
// Callbacks receive the command's context so callers can resolve per-session state.
func NewVoiceCommand(getVoice func(context.Context) bool, setVoice func(context.Context, bool)) *Command {
	return &Command{
		Name:        "voice",
		Description: "Toggle voice mode (replies sent as voice notes)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			current := getVoice(ctx)
			setVoice(ctx, !current)
			if !current {
				return "Voice mode ON — replies will be sent as voice notes.", nil
			}
			return "Voice mode OFF — replies will be sent as text.", nil
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

// NewTmuxCommand returns a /tmux command that wraps the tmux tool, exposing all
// operations via slash-command syntax. It delegates to execFn (the tool's Execute).
func NewTmuxCommand(execFn func(ctx context.Context, params json.RawMessage) (string, error)) *Command {
	const usage = `Usage: /tmux [operation] [args...]

Operations:
  list                          List all tmux sessions (default)
  start [name] [command...]     Start a new session (auto-watches)
  start [name] --no-watch [cmd] Start without auto-watch
  send <name> <keys...>         Send keystrokes to a session
  read <name> [lines]           Read pane output
  kill <name>                   Kill a session
  watch <name> [threshold_secs] Watch session for inactivity
  unwatch <name>                Stop watching a session`

	return &Command{
		Name:           "tmux",
		Description:    "Manage tmux sessions — start, send, read, list, kill, watch, unwatch",
		Category:       "observability",
		SkipToolExport: true,
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
						name = strings.TrimPrefix(result, "Session started: ")
					}
					watchParams, _ := json.Marshal(map[string]interface{}{
						"operation": "watch",
						"name":      name,
					})
					watchResult, watchErr := execFn(ctx, watchParams)
					if watchErr != nil {
						return result + "\n(auto-watch failed: " + watchErr.Error() + ")", nil
					}
					return result + "\n" + watchResult, nil
				}
				return result, nil

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
				return "```\n" + result + "\n```", nil

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
			return execFn(ctx, raw)
		},
	}
}

// SystemSection describes one section of the system prompt with its character count.
type SystemSection struct {
	Name  string
	Chars int
}

// MessageBreakdown holds character counts by message role.
type MessageBreakdown struct {
	UserChars       int
	AssistantChars  int
	ToolResultChars int
	UserCount       int
	AssistantCount  int
}

// SectionTokens holds the exact token count for one system prompt section.
type SectionTokens struct {
	Name   string
	Tokens int
}

// TokenCounts holds exact token counts from the counting API.
type TokenCounts struct {
	Total        int             // total input tokens (full request)
	System       int             // system prompt tokens
	Conversation int             // conversation tokens (total - system - tools)
	Tools        int             // tool definition tokens
	Sections     []SectionTokens // per-component breakdown (env, files, skills)
}

// ContextInfo holds data for the /context command.
type ContextInfo struct {
	SessionKey       string
	Model            string
	CompactionThresh float64
	ContextLimit     int
	SystemSections   []SystemSection                                 // workspace file sections
	EnvironmentChars int                                             // environment block chars
	SkillsChars      int                                             // skills/extra system blocks chars
	Messages         MessageBreakdown                                // conversation breakdown
	CountTokensFn    func(ctx context.Context) (*TokenCounts, error) // nil = use estimates
}

// NewContextCommand returns a /context command showing context size breakdown.
func NewContextCommand(apiLogPath string, infoFn func() ContextInfo) *Command {
	return &Command{
		Name:        "context",
		Description: "Context window breakdown: system prompt, conversation, compaction status",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			info := infoFn()

			// Get last API call for this session
			entries := readAPILog(apiLogPath)
			var lastInput, lastCacheRead, lastCacheWrite, lastOutput int
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Session == info.SessionKey {
					lastInput = entries[i].Input
					lastCacheRead = entries[i].CacheRead
					lastCacheWrite = entries[i].CacheWrite
					lastOutput = entries[i].Output
					break
				}
			}

			// Try detailed token counts via counting API
			var tc *TokenCounts
			if info.CountTokensFn != nil {
				tc, _ = info.CountTokensFn(ctx) // ignore error, fall back to estimates
			}

			totalTokens := lastInput + lastCacheRead + lastCacheWrite
			if tc == nil && totalTokens == 0 {
				return "No API calls yet for this session.", nil
			}

			// Header tokens
			headerTokens := totalTokens
			useExact := tc != nil
			if useExact {
				headerTokens = tc.Total
			}

			threshTokens := int(float64(info.ContextLimit) * info.CompactionThresh)
			percentUsed := float64(headerTokens) / float64(info.ContextLimit) * 100
			percentThresh := info.CompactionThresh * 100

			var sb strings.Builder

			// Header section
			tokenLabel := formatCommas(headerTokens)
			if !useExact {
				tokenLabel = "~" + tokenLabel
			}
			sb.WriteString("```\n")
			fmt.Fprintf(&sb, "Context: %s / %s tokens (%.1f%%)\n",
				tokenLabel, formatCommas(info.ContextLimit), percentUsed)
			fmt.Fprintf(&sb, "Compaction at: %s (%.0f%%)\n",
				formatCommas(threshTokens), percentThresh)
			if headerTokens >= threshTokens {
				sb.WriteString("Status: at/above threshold\n")
			} else {
				remaining := threshTokens - headerTokens
				fmt.Fprintf(&sb, "Status: %s tokens until compaction\n", formatCommas(remaining))
			}
			sb.WriteString("```")

			// System prompt breakdown
			sb.WriteString("\n\n```\n")
			if useExact {
				fmt.Fprintf(&sb, "System prompt: %s tokens\n", formatCommas(tc.System))
				maxNameLen := 0
				for _, s := range tc.Sections {
					if len(s.Name) > maxNameLen {
						maxNameLen = len(s.Name)
					}
				}
				for _, s := range tc.Sections {
					fmt.Fprintf(&sb, "  %-*s  %s tokens\n", maxNameLen, s.Name, formatCommas(s.Tokens))
				}
				fmt.Fprintf(&sb, "\nTools: %s tokens\n", formatCommas(tc.Tools))
			} else {
				totalSystemChars := 0
				for _, s := range info.SystemSections {
					totalSystemChars += s.Chars
				}
				totalSystemChars += info.EnvironmentChars + info.SkillsChars
				fmt.Fprintf(&sb, "System prompt: ~%s tokens\n", formatCommas(totalSystemChars/4))

				maxNameLen := 0
				if info.EnvironmentChars > 0 && len("Environment") > maxNameLen {
					maxNameLen = len("Environment")
				}
				if info.SkillsChars > 0 && len("Skills") > maxNameLen {
					maxNameLen = len("Skills")
				}
				for _, s := range info.SystemSections {
					if len(s.Name) > maxNameLen {
						maxNameLen = len(s.Name)
					}
				}
				if info.EnvironmentChars > 0 {
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, "Environment", formatCommas(info.EnvironmentChars/4))
				}
				for _, s := range info.SystemSections {
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, s.Name, formatCommas(s.Chars/4))
				}
				if info.SkillsChars > 0 {
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, "Skills", formatCommas(info.SkillsChars/4))
				}
			}
			sb.WriteString("```")

			// Conversation breakdown
			mb := info.Messages
			sb.WriteString("\n\n```\n")
			if useExact {
				fmt.Fprintf(&sb, "Conversation: %s tokens (%d messages)\n",
					formatCommas(tc.Conversation), mb.UserCount+mb.AssistantCount)
			} else {
				totalConvChars := mb.UserChars + mb.AssistantChars + mb.ToolResultChars
				fmt.Fprintf(&sb, "Conversation: ~%s tokens (%d messages)\n",
					formatCommas(totalConvChars/4), mb.UserCount+mb.AssistantCount)
			}
			// Per-role always estimated from chars
			fmt.Fprintf(&sb, "  User messages     ~%s tokens (%d msgs)\n",
				formatCommas(mb.UserChars/4), mb.UserCount)
			fmt.Fprintf(&sb, "  Assistant         ~%s tokens (%d msgs)\n",
				formatCommas(mb.AssistantChars/4), mb.AssistantCount)
			if mb.ToolResultChars > 0 {
				fmt.Fprintf(&sb, "  Tool results      ~%s tokens\n",
					formatCommas(mb.ToolResultChars/4))
			}
			sb.WriteString("```")

			// Token breakdown from last API call
			sb.WriteString("\n\n```\n")
			fmt.Fprintf(&sb, "Last API call tokens:\n")
			fmt.Fprintf(&sb, "  input:       %s\n", formatCommas(lastInput))
			fmt.Fprintf(&sb, "  cache_read:  %s\n", formatCommas(lastCacheRead))
			fmt.Fprintf(&sb, "  cache_write: %s\n", formatCommas(lastCacheWrite))
			fmt.Fprintf(&sb, "  output:      %s\n", formatCommas(lastOutput))
			sb.WriteString("```")

			return sb.String(), nil
		},
	}
}

// NewReloadCommand returns a /reload command that reloads config and system files.
// reloadFn is a callback that performs the reload (avoids import coupling).
// This command is human-only (SkipToolExport: true) because it changes the
// system prompt prefix, which would cause expensive cache busts.
func NewReloadCommand(reloadFn func() (string, error)) *Command {
	return &Command{
		Name:           "reload",
		Description:    "Reload config, skills, and system prompt from disk",
		Category:       "operations",
		SkipToolExport: true,
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
			return "Agents\n\n```\n" + table.Format(cols, tableRows) + "\n```", nil
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

// SecretsStore is the interface needed by the /secrets command.
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

				cols := []table.Column{
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
				return fmt.Sprintf("Secrets (%d keys)\n\n```\n%s\n```",
					len(names), table.Format(cols, tableRows)), nil

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
