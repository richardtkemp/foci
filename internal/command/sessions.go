package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
)

// SessionChatInfo holds per-chat session data for display.
type SessionChatInfo struct {
	ChatID       int64
	Username     string
	MessageCount int
	LastActivity time.Time
	IsDefault    bool
}

// SessionIndexInfo holds session index data for display.
type SessionIndexInfo struct {
	SessionKey       string
	CreatedAt        time.Time
	LastActivityAt   time.Time
	ParentSessionKey string
	SessionType      string
	Status           string
}

// SessionIndexOpts controls filtering for the /sessions index subcommand.
type SessionIndexOpts struct {
	TypeFilter   string
	StatusFilter string
	MaxAge       time.Duration // 0 = no limit
	MaxCount     int           // 0 = no limit
}

// SessionsDeps holds dependencies for the /sessions command.
type SessionsDeps struct {
	AgentID       string
	ListFn        func() ([]SessionChatInfo, error)
	SetDefaultFn  func(chatID int64) error
	DefaultChatFn func() int64
	IndexFn       func(opts SessionIndexOpts) ([]SessionIndexInfo, error) // nil = index not available
}

// NewSessionsCommand creates the /sessions command for managing per-chat sessions.
func NewSessionsCommand(deps SessionsDeps) *Command {
	return &Command{
		Name:        "sessions",
		Description: "List and manage per-chat sessions",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			subcmd := ""
			if len(parts) > 0 {
				subcmd = strings.ToLower(parts[0])
			}

			switch subcmd {
			case "":
				return "Usage: /sessions [list|default <chat_id>|info|index [filters...]]\n\n" +
					"  list              List all chat sessions for this agent\n" +
					"  default <chat_id> Set the default session (used by keepalive, cron)\n" +
					"  info              Show details for the current chat's session\n" +
					"  index [filters]   Query session index (all agents)\n\n" +
					"Index filters: type (chat/spawn/cron/multiball/branch),\n" +
					"  status (active/compacted/archived/cleared/all), duration (3d/4h),\n"+
					"  count (5/10) — show only the N most recent sessions", nil

			case "list":
				chatID, _ := ctx.Value(ChatIDKey{}).(int64)
				return sessionsListCmd(deps, chatID)

			case "default":
				if len(parts) < 2 {
					return "Usage: /sessions default <chat_id>", nil
				}
				chatID, err := strconv.ParseInt(parts[1], 10, 64)
				if err != nil {
					return fmt.Sprintf("Invalid chat ID: %s", parts[1]), nil
				}
				return sessionsDefaultCmd(deps, chatID)

			case "info":
				chatID, _ := ctx.Value(ChatIDKey{}).(int64)
				return sessionsInfoCmd(deps, chatID)

			case "index":
				opts := parseIndexArgs(parts[1:])
				return sessionsIndexCmd(deps, opts)

			default:
				return "Usage: /sessions [list|default <chat_id>|info|index [filters...]]", nil
			}
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			opts := []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "info", Data: "info"},
			}
			if deps.IndexFn != nil {
				opts = append(opts, KeyboardOption{Label: "index", Data: "index"})
			}
			return opts
		},
	}
}

// knownSessionStatuses is the set of valid status filter values.
var knownSessionStatuses = map[string]bool{
	"active":    true,
	"compacted": true,
	"archived":  true,
	"cleared":   true,
	"all":       true,
}

// knownSessionTypes is the set of valid type filter values.
var knownSessionTypes = map[string]bool{
	"chat":      true,
	"spawn":     true,
	"cron":      true,
	"multiball": true,
	"branch":    true,
}

// parseIndexArgs parses flexible filter arguments for /sessions index.
// Each arg is classified as a status, type, or duration.
// Default: status=active, no age limit.
func parseIndexArgs(args []string) SessionIndexOpts {
	opts := SessionIndexOpts{
		StatusFilter: "active", // default to showing only active sessions
	}

	for _, arg := range args {
		lower := strings.ToLower(arg)

		// Check for status keywords
		if knownSessionStatuses[lower] {
			if lower == "all" {
				opts.StatusFilter = "" // no status filter
			} else {
				opts.StatusFilter = lower
			}
			continue
		}

		// Check for type keywords
		if knownSessionTypes[lower] {
			opts.TypeFilter = lower
			continue
		}

		// Try to parse as a duration (e.g. "3d", "4h", "168h")
		if d, ok := parseFriendlyDuration(lower); ok {
			opts.MaxAge = d
			continue
		}

		// Try to parse as a plain count (e.g. "5", "10")
		if n, err := strconv.Atoi(lower); err == nil && n > 0 {
			opts.MaxCount = n
			continue
		}
	}

	return opts
}

// parseFriendlyDuration parses duration strings including "Nd" for days.
func parseFriendlyDuration(s string) (time.Duration, bool) {
	// Handle "Nd" (days) syntax
	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, true
		}
	}
	// Standard Go duration
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	return 0, false
}

func sessionsListCmd(deps SessionsDeps, currentChatID int64) (string, error) {
	sessions, err := deps.ListFn()
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return "No chat sessions yet.", nil
	}

	defaultChat := deps.DefaultChatFn()

	type row struct {
		chatID, username, msgs, active, flags string
	}
	rows := make([]row, len(sessions))
	for i, s := range sessions {
		r := row{
			chatID: strconv.FormatInt(s.ChatID, 10),
			msgs:   strconv.Itoa(s.MessageCount),
		}
		if s.Username != "" {
			r.username = "@" + s.Username
		} else {
			r.username = "—"
		}
		if s.LastActivity.IsZero() {
			r.active = "—"
		} else {
			r.active = s.LastActivity.Format("15:04 UTC")
		}
		var flags []string
		if s.ChatID == currentChatID {
			flags = append(flags, "◉")
		}
		if s.ChatID == defaultChat {
			flags = append(flags, "★")
		}
		r.flags = strings.Join(flags, " ")
		rows[i] = r
	}

	cols := []display.Column{
		{Header: "Chat ID"},
		{Header: "User"},
		{Header: "Msgs", Align: display.AlignRight},
		{Header: "Active"},
		{Header: ""},
	}
	tableRows := make([][]string, len(rows))
	for i, r := range rows {
		tableRows[i] = []string{r.chatID, r.username, r.msgs, r.active, r.flags}
	}
	return fmt.Sprintf("Sessions — %s (%d)\n\n%s\n◉ = current  ★ = default (keepalive, cron)",
		deps.AgentID, len(sessions), display.MarkdownTable(cols, tableRows)), nil
}

func sessionsDefaultCmd(deps SessionsDeps, chatID int64) (string, error) {
	// Verify the chat ID exists
	sessions, err := deps.ListFn()
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	found := false
	for _, s := range sessions {
		if s.ChatID == chatID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("No session found for chat ID %d.", chatID), nil
	}

	if err := deps.SetDefaultFn(chatID); err != nil {
		return "", fmt.Errorf("set default: %w", err)
	}
	return fmt.Sprintf("Default session set to chat %d.", chatID), nil
}

func sessionsInfoCmd(deps SessionsDeps, chatID int64) (string, error) {
	if chatID == 0 {
		return "Not in a chat context.", nil
	}

	defaultChat := deps.DefaultChatFn()
	sessions, err := deps.ListFn()
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Chat ID: %d\n", chatID)
	if chatID == defaultChat {
		sb.WriteString("Default: yes\n")
	} else {
		sb.WriteString("Default: no\n")
	}

	for _, s := range sessions {
		if s.ChatID == chatID {
			fmt.Fprintf(&sb, "Messages: %d\n", s.MessageCount)
			if !s.LastActivity.IsZero() {
				fmt.Fprintf(&sb, "Last active: %s\n", s.LastActivity.Format(time.RFC3339))
			}
			if s.Username != "" {
				fmt.Fprintf(&sb, "User: @%s\n", s.Username)
			}
			fmt.Fprintf(&sb, "Session: %s/c%d", deps.AgentID, chatID)
			return sb.String(), nil
		}
	}

	fmt.Fprintf(&sb, "Session: %s/c%d (new — no messages yet)", deps.AgentID, chatID)
	return sb.String(), nil
}

func sessionsIndexCmd(deps SessionsDeps, opts SessionIndexOpts) (string, error) {
	if deps.IndexFn == nil {
		return "Session index not available.", nil
	}

	entries, err := deps.IndexFn(opts)
	if err != nil {
		return "", fmt.Errorf("query session index: %w", err)
	}
	if len(entries) == 0 {
		msg := "No sessions found"
		if opts.TypeFilter != "" || opts.StatusFilter != "" {
			msg += " matching filters"
		}
		return msg + ".", nil
	}

	// Sort by last activity, most recent first.
	sort.Slice(entries, func(i, j int) bool {
		ti := entries[i].LastActivityAt
		if ti.IsZero() {
			ti = entries[i].CreatedAt
		}
		tj := entries[j].LastActivityAt
		if tj.IsZero() {
			tj = entries[j].CreatedAt
		}
		return ti.After(tj)
	})

	// Apply count limit after sorting.
	totalCount := len(entries)
	if opts.MaxCount > 0 && opts.MaxCount < len(entries) {
		entries = entries[:opts.MaxCount]
	}

	cols := []display.Column{
		{Header: "Session Key"},
		{Header: "Status"},
		{Header: "Active"},
		{Header: "Parent"},
	}
	tableRows := make([][]string, len(entries))
	for i, e := range entries {
		activity := "—"
		if !e.LastActivityAt.IsZero() {
			activity = display.RelativeTime(e.LastActivityAt)
		} else if !e.CreatedAt.IsZero() {
			activity = display.RelativeTime(e.CreatedAt)
		}
		parent := "—"
		if e.ParentSessionKey != "" {
			parent = shortenSessionKey(e.ParentSessionKey)
		}
		tableRows[i] = []string{
			shortenSessionKey(e.SessionKey),
			statusEmoji(e.Status),
			activity,
			parent,
		}
	}

	filterDesc := ""
	if opts.TypeFilter != "" {
		filterDesc += " type=" + opts.TypeFilter
	}
	if opts.StatusFilter != "" {
		filterDesc += " status=" + opts.StatusFilter
	}
	if opts.MaxAge > 0 {
		filterDesc += " age<=" + opts.MaxAge.String()
	}
	if filterDesc != "" {
		filterDesc = " (" + strings.TrimSpace(filterDesc) + ")"
	}

	countDesc := fmt.Sprintf("%d sessions", len(entries))
	if opts.MaxCount > 0 && opts.MaxCount < totalCount {
		countDesc = fmt.Sprintf("%d of %d sessions", len(entries), totalCount)
	}

	return fmt.Sprintf("Session Index — %s%s\n\n%s",
		countDesc, filterDesc, display.MarkdownTable(cols, tableRows)), nil
}

// statusEmoji maps session status strings to emoji indicators.
func statusEmoji(status string) string {
	switch status {
	case "active":
		return "🟢"
	case "compacted":
		return "📦"
	case "archived":
		return "🗄️"
	case "cleared":
		return "🧹"
	default:
		return status
	}
}

// shortenSessionKey abbreviates a session key for table display.
// "scout/c5970082313/1772794601" → "scout/c597…"
// "scout/c5970082313/1772794601/b1772795000" → "scout/c597…/b177…"
func shortenSessionKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return key
	}
	// Always show agent + truncated typeID
	typeID := parts[1]
	if len(typeID) > 4 {
		typeID = typeID[:4] + "…"
	}
	short := parts[0] + "/" + typeID
	// If there's a child suffix, append truncated child
	if len(parts) >= 4 {
		child := parts[3]
		if len(child) > 4 {
			child = child[:4] + "…"
		}
		short += "/" + child
	}
	return short
}
