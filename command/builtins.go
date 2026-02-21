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
