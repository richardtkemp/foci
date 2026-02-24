package command

import (
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

// SessionsDeps holds dependencies for the /sessions command.
type SessionsDeps struct {
	AgentID        string
	ListFn         func() ([]SessionChatInfo, error)
	SetDefaultFn   func(chatID int64) error
	DefaultChatFn  func() int64
}

// NewSessionsCommand creates the /sessions command for managing per-chat sessions.
func NewSessionsCommand(deps SessionsDeps) *Command {
	return &Command{
		Name:        "sessions",
		Description: "List and manage per-chat sessions",
		Category:    "session",
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			subcmd := "list"
			if len(parts) > 0 {
				subcmd = strings.ToLower(parts[0])
			}

			switch subcmd {
			case "list", "":
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

			default:
				return "Usage: /sessions [list|default <chat_id>|info]", nil
			}
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

	// Measure column widths
	cidW, unW, msgW, actW := len("Chat ID"), len("User"), len("Msgs"), len("Active")
	for _, r := range rows {
		if len(r.chatID) > cidW { cidW = len(r.chatID) }
		if len(r.username) > unW { unW = len(r.username) }
		if len(r.msgs) > msgW { msgW = len(r.msgs) }
		if len(r.active) > actW { actW = len(r.active) }
	}

	sep := strings.Repeat("─", cidW+2+unW+2+msgW+2+actW+4)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Sessions — %s (%d)\n\n```\n", deps.AgentID, len(sessions))
	fmt.Fprintf(&sb, "%-*s  %-*s  %*s  %-*s  Def\n", cidW, "Chat ID", unW, "User", msgW, "Msgs", actW, "Active")
	sb.WriteString(sep + "\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-*s  %-*s  %*s  %-*s  %s\n",
			cidW, r.chatID, unW, r.username, msgW, r.msgs, actW, r.active, r.def)
	}
	sb.WriteString(sep + "\n")
	sb.WriteString("```\n★ = default session (used by heartbeats, cron)")
	return sb.String(), nil
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
