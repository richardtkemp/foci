package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/session"
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

// SessionsCommand creates the /sessions command for managing per-chat sessions.
func SessionsCommand() *Command {
	cmd := &Command{
		Name:        "sessions",
		Description: "List and manage per-chat sessions",
		Category:    "session",
		Subcommands: []Subcommand{
			{
				Name:        "list",
				Description: "List all chat sessions for this agent",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					text, err := sessionsListCmd(cc, req.ChatID)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "default",
				Description: "Set the default session (used by keepalive, cron)",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					chatID := req.ChatID
					parts := strings.Fields(req.Args)
					if len(parts) >= 1 {
						var err error
						chatID, err = strconv.ParseInt(parts[0], 10, 64)
						if err != nil {
							return Response{Text: fmt.Sprintf("Invalid chat ID: %s", parts[0])}, nil
						}
					}
					text, err := sessionsDefaultCmd(cc, chatID)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "info",
				Description: "Show details for the current chat's session",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					text, err := sessionsInfoCmd(cc, req.ChatID)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "index",
				Description: "Query session index (all agents)",
				Visible: func(_ context.Context, cc CommandContext) bool {
					return cc.SessionIndex != nil
				},
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					opts := parseIndexArgs(strings.Fields(req.Args))
					text, err := sessionsIndexCmd(cc, opts)
					return Response{Text: text}, err
				},
			},
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
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
	"facet": true,
	"branch":    true,
}

// parseIndexArgs parses flexible filter arguments for /sessions index.
func parseIndexArgs(args []string) SessionIndexOpts {
	opts := SessionIndexOpts{
		StatusFilter: "active",
	}

	for _, arg := range args {
		lower := strings.ToLower(arg)

		if knownSessionStatuses[lower] {
			if lower == "all" {
				opts.StatusFilter = ""
			} else {
				opts.StatusFilter = lower
			}
			continue
		}

		if knownSessionTypes[lower] {
			opts.TypeFilter = lower
			continue
		}

		if d, ok := parseFriendlyDuration(lower); ok {
			opts.MaxAge = d
			continue
		}

		if n, err := strconv.Atoi(lower); err == nil && n > 0 {
			opts.MaxCount = n
			continue
		}
	}

	return opts
}

func parseFriendlyDuration(s string) (time.Duration, bool) {
	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, true
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	return 0, false
}

func sessionsListCmd(cc CommandContext, currentChatID int64) (string, error) {
	chatSessions, err := cc.Sessions.ListChatSessions(cc.AgentConfig.ID)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if len(chatSessions) == 0 {
		return "No chat sessions yet.", nil
	}

	var defaultChat int64
	if cc.SessionIndex != nil {
		defaultChat, _ = cc.SessionIndex.DefaultChatForAgent(cc.AgentConfig.ID)
	}

	type row struct {
		chatID, username, msgs, active, flags string
	}
	rows := make([]row, len(chatSessions))
	for i, cs := range chatSessions {
		r := row{
			chatID: strconv.FormatInt(cs.ChatID, 10),
			msgs:   strconv.Itoa(cs.MessageCount),
		}
		// Resolve username from session index
		if cc.SessionIndex != nil {
			if username, err := cc.SessionIndex.GetChatMetadataAnyPlatform(cc.AgentConfig.ID, cs.ChatID, "username"); err == nil && username != "" {
				r.username = "@" + username
			} else {
				r.username = "—"
			}
		} else {
			r.username = "—"
		}
		if cs.LastActivity.IsZero() {
			r.active = "—"
		} else {
			r.active = cs.LastActivity.Format("15:04 UTC")
		}
		var flags []string
		if cs.ChatID == currentChatID {
			flags = append(flags, "◉")
		}
		if cs.ChatID == defaultChat {
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
		cc.AgentConfig.ID, len(chatSessions), display.MarkdownTable(cols, tableRows)), nil
}

func sessionsDefaultCmd(cc CommandContext, chatID int64) (string, error) {
	chatSessions, err := cc.Sessions.ListChatSessions(cc.AgentConfig.ID)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	found := false
	for _, cs := range chatSessions {
		if cs.ChatID == chatID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("No session found for chat ID %d.", chatID), nil
	}

	if cc.SessionIndex == nil {
		return "", fmt.Errorf("no session index configured")
	}
	// Determine platform from the session key stored for this chat.
	plat := ""
	if sk, err := cc.SessionIndex.GetChatMetadataAnyPlatform(cc.AgentConfig.ID, chatID, "session_key"); err == nil && sk != "" {
		plat = cc.SessionIndex.PlatformForChat(cc.AgentConfig.ID, chatID)
	}
	if err := cc.SessionIndex.SetDefaultChat(cc.AgentConfig.ID, plat, chatID); err != nil {
		return "", fmt.Errorf("set default: %w", err)
	}
	return fmt.Sprintf("Default session set to chat %d.", chatID), nil
}

func sessionsInfoCmd(cc CommandContext, chatID int64) (string, error) {
	if chatID == 0 {
		return "Not in a chat context.", nil
	}

	var defaultChat int64
	if cc.SessionIndex != nil {
		defaultChat, _ = cc.SessionIndex.DefaultChatForAgent(cc.AgentConfig.ID)
	}

	chatSessions, err := cc.Sessions.ListChatSessions(cc.AgentConfig.ID)
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

	for _, cs := range chatSessions {
		if cs.ChatID == chatID {
			fmt.Fprintf(&sb, "Messages: %d\n", cs.MessageCount)
			if !cs.LastActivity.IsZero() {
				fmt.Fprintf(&sb, "Last active: %s\n", cs.LastActivity.Format(time.RFC3339))
			}
			// Resolve username
			if cc.SessionIndex != nil {
				if username, err := cc.SessionIndex.GetChatMetadataAnyPlatform(cc.AgentConfig.ID, cs.ChatID, "username"); err == nil && username != "" {
					fmt.Fprintf(&sb, "User: @%s\n", username)
				}
			}
			fmt.Fprintf(&sb, "Session: %s/c%d", cc.AgentConfig.ID, chatID)
			return sb.String(), nil
		}
	}

	fmt.Fprintf(&sb, "Session: %s/c%d (new — no messages yet)", cc.AgentConfig.ID, chatID)
	return sb.String(), nil
}

func sessionsIndexCmd(cc CommandContext, opts SessionIndexOpts) (string, error) {
	if cc.SessionIndex == nil {
		return "Session index not available.", nil
	}

	qopts := session.QueryOptions{
		SessionType: opts.TypeFilter,
		Status:      opts.StatusFilter,
		MaxAge:      opts.MaxAge,
	}
	entries, err := cc.SessionIndex.Query(qopts)
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

	totalCount := len(entries)
	displayCount := opts.MaxCount
	if displayCount == 0 {
		displayCount = 10
	}
	if displayCount < len(entries) {
		entries = entries[:displayCount]
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
			parent = e.ParentSessionKey
		}
		tableRows[i] = []string{
			e.SessionKey,
			statusEmoji(string(e.Status)),
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
	if len(entries) < totalCount {
		countDesc = fmt.Sprintf("%d of %d sessions", len(entries), totalCount)
	}

	return fmt.Sprintf("Session Index — %s%s\n\n%s",
		countDesc, filterDesc, display.MarkdownTable(cols, tableRows)), nil
}

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

