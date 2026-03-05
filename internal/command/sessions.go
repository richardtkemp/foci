package command

import (
	"context"
	"fmt"
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
					"  status (active/compacted/archived/cleared/all), duration (3d/4h)", nil

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
		deps.AgentID, len(sessions), display.Format(cols, tableRows)), nil
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
		return "Not in a chat context (use from Telegram).", nil
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
			fmt.Fprintf(&sb, "Session: agent:%s:chat:%d", deps.AgentID, chatID)
			return sb.String(), nil
		}
	}

	fmt.Fprintf(&sb, "Session: agent:%s:chat:%d (new — no messages yet)", deps.AgentID, chatID)
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

	cols := []display.Column{
		{Header: "Session Key"},
		{Header: "Type"},
		{Header: "Status"},
		{Header: "Last Active"},
		{Header: "Parent"},
	}
	tableRows := make([][]string, len(entries))
	for i, e := range entries {
		activity := "—"
		if !e.LastActivityAt.IsZero() {
			activity = e.LastActivityAt.Format("Jan 02 15:04")
		} else if !e.CreatedAt.IsZero() {
			activity = e.CreatedAt.Format("Jan 02 15:04")
		}
		parent := "—"
		if e.ParentSessionKey != "" {
			// Shorten parent key for display
			parent = shortenSessionKey(e.ParentSessionKey)
		}
		tableRows[i] = []string{
			shortenSessionKey(e.SessionKey),
			e.SessionType,
			e.Status,
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

	return fmt.Sprintf("Session Index — %d sessions%s\n\n%s",
		len(entries), filterDesc, display.Format(cols, tableRows)), nil
}

// shortenSessionKey abbreviates a session key for table display.
// "agent:mybot:chat:5970082313" → "mybot/chat:597…"
// "agent:mybot:branch:abc123-def456" → "mybot/branch:abc123…"
func shortenSessionKey(key string) string {
	if !strings.HasPrefix(key, "agent:") {
		return key
	}
	rest := key[len("agent:"):]
	// Split into agent name and session part
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return rest
	}
	agentName := parts[0]
	sessionPart := parts[1] // e.g. "chat:5970082313" or "branch:abc123-def456"
	// Truncate long IDs (chat IDs, UUIDs)
	typeParts := strings.SplitN(sessionPart, ":", 2)
	if len(typeParts) == 2 && len(typeParts[1]) > 8 {
		typeParts[1] = typeParts[1][:8] + "…"
	}
	return agentName + "/" + strings.Join(typeParts, ":")
}
