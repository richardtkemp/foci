package command

import (
	"foci/table"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	ParentSessionKey string
	SessionType      string
	Status           string
}

// SessionsDeps holds dependencies for the /sessions command.
type SessionsDeps struct {
	AgentID       string
	ListFn        func() ([]SessionChatInfo, error)
	SetDefaultFn  func(chatID int64) error
	DefaultChatFn func() int64
	IndexFn       func(sessionType, status string) ([]SessionIndexInfo, error) // nil = index not available
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
				return "Usage: /sessions [list|default <chat_id>|info|index]\n\n" +
					"  list              List all chat sessions for this agent\n" +
					"  default <chat_id> Set the default session (used by keepalive, cron)\n" +
					"  info              Show details for the current chat's session\n" +
					"  index [type] [status]  Query session index (all agents)", nil

			case "list":
				return sessionsListCmd(deps)

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
				var typeFilter, statusFilter string
				if len(parts) > 1 {
					typeFilter = parts[1]
				}
				if len(parts) > 2 {
					statusFilter = parts[2]
				}
				return sessionsIndexCmd(deps, typeFilter, statusFilter)

			default:
				return "Usage: /sessions [list|default <chat_id>|info|index]", nil
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

func sessionsListCmd(deps SessionsDeps) (string, error) {
	sessions, err := deps.ListFn()
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return "No chat sessions yet.", nil
	}

	defaultChat := deps.DefaultChatFn()

	type row struct {
		chatID, username, msgs, active, def string
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
		if s.ChatID == defaultChat {
			r.def = "★"
		} else {
			r.def = ""
		}
		rows[i] = r
	}

	cols := []table.Column{
		{Header: "Chat ID"},
		{Header: "User"},
		{Header: "Msgs", Align: table.AlignRight},
		{Header: "Active"},
		{Header: "Def"},
	}
	tableRows := make([][]string, len(rows))
	for i, r := range rows {
		tableRows[i] = []string{r.chatID, r.username, r.msgs, r.active, r.def}
	}
	return fmt.Sprintf("Sessions — %s (%d)\n\n```\n%s\n```\n★ = default session (used by keepalive, cron)",
		deps.AgentID, len(sessions), table.Format(cols, tableRows)), nil
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

func sessionsIndexCmd(deps SessionsDeps, typeFilter, statusFilter string) (string, error) {
	if deps.IndexFn == nil {
		return "Session index not available.", nil
	}

	entries, err := deps.IndexFn(typeFilter, statusFilter)
	if err != nil {
		return "", fmt.Errorf("query session index: %w", err)
	}
	if len(entries) == 0 {
		msg := "No sessions found"
		if typeFilter != "" || statusFilter != "" {
			msg += " matching filters"
		}
		return msg + ".", nil
	}

	cols := []table.Column{
		{Header: "Session Key"},
		{Header: "Type"},
		{Header: "Status"},
		{Header: "Created"},
		{Header: "Parent"},
	}
	tableRows := make([][]string, len(entries))
	for i, e := range entries {
		created := "—"
		if !e.CreatedAt.IsZero() {
			created = e.CreatedAt.Format("Jan 02 15:04")
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
			created,
			parent,
		}
	}

	filterDesc := ""
	if typeFilter != "" {
		filterDesc += " type=" + typeFilter
	}
	if statusFilter != "" {
		filterDesc += " status=" + statusFilter
	}
	if filterDesc != "" {
		filterDesc = " (" + strings.TrimSpace(filterDesc) + ")"
	}

	return fmt.Sprintf("Session Index — %d sessions%s\n\n```\n%s\n```",
		len(entries), filterDesc, table.Format(cols, tableRows)), nil
}

// shortenSessionKey abbreviates a session key for table display.
// "agent:mybot:chat:5970082313" → "mybot:chat:5970082313"
func shortenSessionKey(key string) string {
	if strings.HasPrefix(key, "agent:") {
		return key[len("agent:"):]
	}
	return key
}
