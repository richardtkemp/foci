package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/table"
)
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
	CallType     string    `json:"call_type"`
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
					if e.CallType == "conversation" || e.CallType == "" {
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


func NewCacheCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "cache",
		Description: "API calls with cache breakdown (default 5)",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			n := 5
			if args != "" {
				if parsed, err := strconv.Atoi(args); err == nil && parsed > 0 {
					n = parsed
				}
			}
			entries := readAPILog(apiLogPath)
			if len(entries) == 0 {
				return "No API calls logged yet.", nil
			}

			// Take last n
			start := 0
			if len(entries) > n {
				start = len(entries) - n
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
				len(recent), avgHit, table.FormatWidth(cols, tableRows, displayWidth(ctx))), nil
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
				return costUsage(), nil
			case "today", "session":
				return costToday(entries, ctx)
			case "24h":
				return cost24h(entries, ctx)
			case "week":
				return costWeek(entries, ctx)
			default:
				return costDays(entries, scope)
			}
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

func readAPILog(path string) []apiEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

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


type TodoItem struct {
	ID       int64
	Text     string
	Status   string
	Priority string
	Tags     string
}

// NewTodoCommand returns a /todo command that lists open todo items.
// listFn returns open todos, optionally filtered by tag (empty = all).
// searchFn returns todos matching a query.
func NewTodoCommand(listFn func(tag string) ([]TodoItem, error), searchFn func(query string) ([]TodoItem, error)) *Command {
	return &Command{
		Name:        "todo",
		Description: "List open todo items",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			args = strings.TrimSpace(args)

			if strings.ToLower(args) == "search" || strings.HasPrefix(strings.ToLower(args), "search ") {
				query := ""
				if len(args) > 7 {
					query = strings.TrimSpace(args[7:])
				}
				if query == "" {
					return "Usage: /todo search <query>", nil
				}
				items, err := searchFn(query)
				if err != nil {
					return "", fmt.Errorf("search todos: %w", err)
				}
				if len(items) == 0 {
					return fmt.Sprintf("No todos matching %q.", query), nil
				}
				var lines []string
				for _, item := range items {
					if item.Status != "open" {
						continue
					}
					lines = append(lines, formatTodoLine(item))
				}
				if len(lines) == 0 {
					return fmt.Sprintf("No open todos matching %q.", query), nil
				}
				return "📋 Search results\n\n" + strings.Join(lines, "\n"), nil
			}

			includeBackground := strings.ToLower(args) == "all"

			items, err := listFn("")
			if err != nil {
				return "", fmt.Errorf("list todos: %w", err)
			}

			var visible, background []TodoItem
			for _, item := range items {
				if hasBackgroundTag(item.Tags) {
					background = append(background, item)
				} else {
					visible = append(visible, item)
				}
			}

			total := len(items)
			backgroundCount := len(background)

			if !includeBackground {
				items = visible
			} else {
				items = append(visible, background...)
			}
			sortTodosByPriority(items)

			if len(items) == 0 {
				if backgroundCount > 0 {
					return fmt.Sprintf("No open todos (%d background items hidden).", backgroundCount), nil
				}
				return "No open todos.", nil
			}

			limit := 20
			showing := len(items)
			if len(items) > limit {
				items = items[:limit]
			}

			var header string
			if showing <= limit && backgroundCount == 0 {
				header = fmt.Sprintf("📋 Open todos (%d)", total)
			} else if backgroundCount > 0 && !includeBackground {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d, hiding %d background items)", len(items), total, backgroundCount)
			} else if backgroundCount > 0 && includeBackground {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d, including %d background)", len(items), total, backgroundCount)
			} else {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d)", len(items), total)
			}

			var lines []string
			for _, item := range items {
				lines = append(lines, formatTodoLine(item))
			}

			return header + "\n\n" + strings.Join(lines, "\n"), nil
		},
	}
}

func hasBackgroundTag(tags string) bool {
	for _, t := range strings.Split(tags, ",") {
		if strings.TrimSpace(t) == "background" {
			return true
		}
	}
	return false
}

func sortTodosByPriority(items []TodoItem) {
	sort.Slice(items, func(i, j int) bool {
		pi := priorityOrder(items[i].Priority)
		pj := priorityOrder(items[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return items[i].ID < items[j].ID
	})
}

func priorityOrder(p string) int {
	switch p {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

func formatTodoLine(item TodoItem) string {
	emoji := "🟢"
	switch item.Priority {
	case "high":
		emoji = "🔴"
	case "medium":
		emoji = "🟡"
	}
	text := item.Text
	if len(text) > 77 {
		text = text[:74] + "..."
	}
	return fmt.Sprintf("%s #%d [%s] %s", emoji, item.ID, item.Priority, text)
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

