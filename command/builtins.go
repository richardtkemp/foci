package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
	SessionKey   string
	MessageCount int
	Model        string
	Uptime       time.Duration
	AgentBusy    bool
}

// NewPingCommand returns a /ping command.
func NewPingCommand() *Command {
	return &Command{
		Name:        "ping",
		Description: "Liveness check",
		Execute: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf("pong %s", time.Now().UTC().Format(time.RFC3339)), nil
		},
	}
}

// NewStatusCommand returns a /status command.
// statusFn is called to gather current status; it avoids coupling to internal packages.
func NewStatusCommand(statusFn func() StatusInfo, apiLogPath string) *Command {
	return &Command{
		Name:        "status",
		Description: "Session status overview",
		Execute: func(ctx context.Context, args string) (string, error) {
			info := statusFn()

			status := "idle"
			if info.AgentBusy {
				status = "processing"
			}

			// Sum tokens from api.jsonl for this session
			entries := readAPILog(apiLogPath)
			var totalIn, totalOut, totalCacheRead, totalCacheWrite int
			var totalCost float64
			for _, e := range entries {
				if e.Session == info.SessionKey {
					totalIn += e.Input
					totalOut += e.Output
					totalCacheRead += e.CacheRead
					totalCacheWrite += e.CacheWrite
					totalCost += e.CostUSD
				}
			}

			return fmt.Sprintf("session: %s\nmodel: %s\nmessages: %d\nstatus: %s\nuptime: %s\ntokens: in=%d out=%d cache_read=%d cache_write=%d\ncost: $%.4f",
				info.SessionKey, info.Model, info.MessageCount, status,
				formatDuration(info.Uptime),
				totalIn, totalOut, totalCacheRead, totalCacheWrite, totalCost), nil
		},
	}
}

// NewCacheCommand returns a /cache command showing recent cache hit/miss breakdown.
func NewCacheCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "cache",
		Description: "Last 5 API calls with cache breakdown",
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

			var b strings.Builder
			for _, e := range recent {
				hitRate := 0.0
				totalInput := e.Input + e.CacheRead + e.CacheWrite
				if totalInput > 0 {
					hitRate = float64(e.CacheRead) / float64(totalInput) * 100
				}
				fmt.Fprintf(&b, "%s  in=%d read=%d write=%d cost=$%.4f (%.0f%% hit)\n",
					e.Timestamp.Format("15:04:05"), e.Input, e.CacheRead, e.CacheWrite,
					e.CostUSD, hitRate)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// NewLastCommand returns a /last command showing the most recent API call.
func NewLastCommand(apiLogPath string) *Command {
	return &Command{
		Name:        "last",
		Description: "Last API request details",
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
		Execute: func(ctx context.Context, args string) (string, error) {
			entries := readAPILog(apiLogPath)
			if len(entries) == 0 {
				return "No API calls logged yet.", nil
			}

			scope := strings.ToLower(strings.TrimSpace(args))
			if scope == "" {
				scope = "today"
			}

			switch scope {
			case "today":
				today := time.Now().UTC().Format("2006-01-02")
				var total float64
				var count int
				for _, e := range entries {
					if e.Timestamp.Format("2006-01-02") == today {
						total += e.CostUSD
						count++
					}
				}
				return fmt.Sprintf("Today: $%.4f (%d API calls)", total, count), nil

			case "session":
				costs := make(map[string]float64)
				counts := make(map[string]int)
				for _, e := range entries {
					costs[e.Session] += e.CostUSD
					counts[e.Session]++
				}
				var b strings.Builder
				var grandTotal float64
				for session, cost := range costs {
					fmt.Fprintf(&b, "%s: $%.4f (%d calls)\n", session, cost, counts[session])
					grandTotal += cost
				}
				fmt.Fprintf(&b, "total: $%.4f", grandTotal)
				return b.String(), nil

			default:
				// Try parsing as number of days
				days, err := strconv.Atoi(scope)
				if err != nil {
					return "Usage: /cost [today|session|<days>]", nil
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
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return fmt.Sprintf("Current model: %s", getModel()), nil
			}
			setModel(args)
			return fmt.Sprintf("Model switched to: %s", args), nil
		},
	}
}

// SessionInfo holds data for the /session command.
type SessionInfo struct {
	SessionKey     string
	MessageCount   int
	CreatedAt      string
	LastActivity   string
}

// NewSessionCommand returns a /session command showing raw session metadata.
func NewSessionCommand(infoFn func() SessionInfo) *Command {
	return &Command{
		Name:        "session",
		Description: "Session metadata",
		Execute: func(ctx context.Context, args string) (string, error) {
			info := infoFn()
			return fmt.Sprintf("key: %s\nmessages: %d\ncreated: %s\nlast_activity: %s",
				info.SessionKey, info.MessageCount, info.CreatedAt, info.LastActivity), nil
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
		Execute: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf("version: %s\ngo: %s\ncommit: %s\nbuilt: %s",
				info.Version, info.GoVersion, info.GitCommit, info.BuildTime), nil
		},
	}
}

// NewUptimeCommand returns a /uptime command.
func NewUptimeCommand(startTime time.Time) *Command {
	return &Command{
		Name:        "uptime",
		Description: "Process uptime",
		Execute: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf("uptime: %s\nstarted: %s",
				formatDuration(time.Since(startTime)),
				startTime.Format(time.RFC3339)), nil
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
