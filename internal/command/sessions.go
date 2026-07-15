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
	AgentID      string        // scope to one agent (empty = all agents)
	RootKey      string        // scope to a session family: this root + its branches
	TypeFilter   string
	StatusFilter string
	MaxAge       time.Duration // 0 = no limit
	MaxCount     int           // 0 = no limit
	statusSet    bool          // user passed an explicit status token
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
				Description: "Show the current session's index row + all metadata",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					text, err := sessionsInfoCmd(cc, req.SessionKey, req.ChatID)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "index",
				Description: "Query session index (all agents; optional scope: me/this/<agent>/<session-key>)",
				Visible: func(_ context.Context, cc CommandContext) bool {
					return cc.SessionIndex != nil
				},
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					opts := parseIndexArgs(strings.Fields(req.Args), cc.AgentConfig.ID, req.SessionKey, knownAgentSet(cc))
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

// knownSessionTypes is the set of valid type filter values (mirrors the
// SessionType constants in internal/session).
var knownSessionTypes = map[string]bool{
	"chat":            true,
	"facet":           true,
	"independent":     true,
	"spawn":           true,
	"reflection":      true,
	"keepalive":       true,
	"background-task": true,
	"unknown":         true,
}

// familyScopeWords select the calling session's family (root + branches).
var familyScopeWords = map[string]bool{
	"this": true, "here": true, "relatives": true, "related": true,
	"children": true, "branches": true, "family": true, "tree": true, "siblings": true,
}

// selfAgentWords scope the query to the calling agent.
var selfAgentWords = map[string]bool{"me": true, "mine": true, "self": true}

// parseIndexArgs parses flexible filter arguments for /sessions index. The first
// recognised scope token (a family word, a self word, a "/"-bearing session key,
// or a known agent id) sets the query scope; remaining tokens set status/type/
// age/count filters.
func parseIndexArgs(args []string, callerAgentID, callerSessionKey string, knownAgents map[string]bool) SessionIndexOpts {
	opts := SessionIndexOpts{
		StatusFilter: "active",
	}

	for _, arg := range args {
		lower := strings.ToLower(arg)

		if opts.AgentID == "" && opts.RootKey == "" {
			switch {
			case familyScopeWords[lower]:
				opts.RootKey = rootKeyOf(callerSessionKey)
				continue
			case selfAgentWords[lower]:
				opts.AgentID = callerAgentID
				continue
			case strings.Contains(arg, "/"):
				opts.RootKey = rootKeyOf(arg)
				continue
			case knownAgents[lower]:
				opts.AgentID = lower
				continue
			}
		}

		if knownSessionStatuses[lower] {
			opts.statusSet = true
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

	// A family query wants the whole tree by default — all statuses, uncapped
	// (see sessionsIndexCmd) — unless the user narrowed it explicitly.
	if opts.RootKey != "" && !opts.statusSet {
		opts.StatusFilter = ""
	}

	return opts
}

// rootKeyOf returns the root session key for any key; unparseable input is
// returned unchanged (used as a literal prefix).
func rootKeyOf(key string) string {
	if sk, err := session.ParseSessionKey(key); err == nil {
		return sk.Root().String()
	}
	return key
}

// knownAgentSet returns the lowercased set of currently-known agent ids.
func knownAgentSet(cc CommandContext) map[string]bool {
	set := map[string]bool{}
	if cc.AgentListFn != nil {
		for _, a := range cc.AgentListFn() {
			set[strings.ToLower(a.ID)] = true
		}
	}
	return set
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

	var defaultChats map[int64]bool
	if cc.SessionIndex != nil {
		defaultChats = cc.SessionIndex.DefaultChatIDs(cc.AgentConfig.ID)
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
			r.active = cs.LastActivity.Local().Format("15:04")
		}
		var flags []string
		if cs.ChatID == currentChatID {
			flags = append(flags, "◉")
		}
		if defaultChats[cs.ChatID] {
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
	if cc.SessionIndex == nil {
		return "", fmt.Errorf("no session index configured")
	}
	// A CC-delegated / app chat's transcript lives in the backend's own store, so
	// it has no <agent>/c<chatID>/root.jsonl for the old ListChatSessions check to
	// find — even the active current session then reported "no session found"
	// (the bug this fixes). Accept the chat if it exists in ANY session-storage
	// location: the index (backend sessions), a platform registration (app chats),
	// or the file store (legacy file-backed chats).
	if !chatSessionExists(cc, chatID) {
		return fmt.Sprintf("No session found for chat ID %d.", chatID), nil
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

// knownSessionMetadataKeys is the canonical set of session_metadata keys, with
// how each renders when unset. There is no single source of truth for these in
// the codebase (they are scattered string literals at the read/write sites), so
// this list is the display contract for /sessions info.
var knownSessionMetadataKeys = []struct {
	key   string
	unset string
}{
	{"model", "null"},
	{"model_endpoint", "null"},
	{"model_format", "null"},
	{"effort", "null"},
	{"permission_mode", "null"},
	{"cc_resume_id", "null"},
	{"last_activity", "null"},
	{"no_compact", "false"},
	{"display_show_thinking", "false"},
	{"orientation_consumed", "false"},
}

// chatSessionExists reports whether a session exists for agent+chat in any
// storage location: the session index, a platform registration, or the file
// store.
func chatSessionExists(cc CommandContext, chatID int64) bool {
	key := session.NewChatSessionKey(cc.AgentConfig.ID, chatID)
	if _, err := cc.SessionIndex.Get(key); err == nil {
		return true
	}
	if cc.SessionIndex.PlatformForChat(cc.AgentConfig.ID, chatID) != "" {
		return true
	}
	if cc.Sessions != nil {
		if chatSessions, err := cc.Sessions.ListChatSessions(cc.AgentConfig.ID); err == nil {
			for _, cs := range chatSessions {
				if cs.ChatID == chatID {
					return true
				}
			}
		}
	}
	return false
}

func sessionsInfoCmd(cc CommandContext, sessionKey string, chatID int64) (string, error) {
	if cc.SessionIndex == nil {
		return "Session index not available.", nil
	}
	if sessionKey == "" {
		if chatID == 0 {
			return "Not in a chat context.", nil
		}
		sessionKey = session.NewChatSessionKey(cc.AgentConfig.ID, chatID)
	}

	cols, vals, found, err := cc.SessionIndex.IndexRow(sessionKey)
	if err != nil {
		return "", fmt.Errorf("read session index: %w", err)
	}
	meta, err := cc.SessionIndex.AllSessionMetadata(sessionKey)
	if err != nil {
		return "", fmt.Errorf("read session metadata: %w", err)
	}

	tableCols := []display.Column{{Header: "Field"}, {Header: "Value"}}
	var rows [][]string
	if found {
		for i, c := range cols {
			rows = append(rows, []string{c, vals[i]})
		}
	} else {
		rows = append(rows,
			[]string{"session_key", sessionKey},
			[]string{"(index)", "no row — new or backend-only session"},
		)
	}
	rows = append(rows, metadataRows(meta)...)

	return fmt.Sprintf("Session info — %s\n\n%s",
		sessionKey, display.MarkdownTable(tableCols, rows)), nil
}

// metadataRows renders every known session_metadata key (unset ones as their
// null/false default) plus any present-but-unknown keys, as Field/Value rows.
func metadataRows(meta map[string]string) [][]string {
	seen := make(map[string]bool, len(knownSessionMetadataKeys))
	var rows [][]string
	for _, mk := range knownSessionMetadataKeys {
		seen[mk.key] = true
		v, ok := meta[mk.key]
		if !ok {
			v = mk.unset
		}
		rows = append(rows, []string{"meta:" + mk.key, v})
	}
	var extra []string
	for k := range meta {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		rows = append(rows, []string{"meta:" + k, meta[k]})
	}
	return rows
}

func sessionsIndexCmd(cc CommandContext, opts SessionIndexOpts) (string, error) {
	if cc.SessionIndex == nil {
		return "Session index not available.", nil
	}

	qopts := session.QueryOptions{
		AgentID:     opts.AgentID,
		RootKey:     opts.RootKey,
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

	// Hybrid recency sort: a user-facing session (chat/facet/independent) is
	// ranked by when a HUMAN last touched it (last_user_activity_at) — automated
	// keepalive/background turns shouldn't float a dormant chat to the top.
	// Non-user-facing sessions (spawn, reflection, keepalive, background-task)
	// have little/no user activity, so they rank by any-turn activity
	// (last_activity_at). Both fall back to created_at.
	sortKey := func(e session.SessionIndexEntry) time.Time {
		if e.SessionType.IsUserFacing() && !e.LastUserActivityAt.IsZero() {
			return e.LastUserActivityAt
		}
		if !e.LastActivityAt.IsZero() {
			return e.LastActivityAt
		}
		return e.CreatedAt
	}
	sort.Slice(entries, func(i, j int) bool {
		return sortKey(entries[i]).After(sortKey(entries[j]))
	})

	totalCount := len(entries)
	displayCount := opts.MaxCount
	if displayCount == 0 {
		// A family (relatives) query shows the whole tree; a plain index caps at 10.
		if opts.RootKey != "" {
			displayCount = len(entries)
		} else {
			displayCount = 10
		}
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
	if opts.AgentID != "" {
		filterDesc += " agent=" + opts.AgentID
	}
	if opts.RootKey != "" {
		filterDesc += " family=" + opts.RootKey
	}
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
