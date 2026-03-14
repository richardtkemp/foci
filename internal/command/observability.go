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

	"foci/internal/compaction"
	"foci/internal/display"
	"foci/internal/provider"
)

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
	CallType   string    `json:"call_type"`
}

// categoryCosts computes per-category cost breakdown from API log entries.
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

// CacheCommand returns a /cache command showing API calls with cache breakdown.
func CacheCommand() *Command {
	return &Command{
		Name:        "cache",
		Description: "API calls with cache breakdown (default 5)",
		Category:    "observability",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			n := 5
			if req.Args != "" {
				if parsed, err := strconv.Atoi(req.Args); err == nil && parsed > 0 {
					n = parsed
				}
			}
			entries := readAPILog(cc.APILogPath)
			if len(entries) == 0 {
				return Response{Text: "No API calls logged yet."}, nil
			}

			start := 0
			if len(entries) > n {
				start = len(entries) - n
			}
			recent := entries[start:]

			var totalCacheRead, totalInput int
			for _, e := range recent {
				totalCacheRead += e.CacheRead
				totalInput += e.Input + e.CacheRead + e.CacheWrite
			}
			avgHit := 0.0
			if totalInput > 0 {
				avgHit = float64(totalCacheRead) / float64(totalInput) * 100
			}

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
					input:  display.FormatCommas(e.Input),
					cRead:  display.FormatCommas(e.CacheRead),
					cWrite: display.FormatCommas(e.CacheWrite),
					cost:   fmt.Sprintf("$%.3f", e.CostUSD),
					hitPct: fmt.Sprintf("%.0f%%", hitRate),
				}
			}

			cols := []display.Column{
				{Header: "Time"},
				{Header: "Input", Align: display.AlignRight},
				{Header: "CacheRead", Align: display.AlignRight},
				{Header: "CacheWrite", Align: display.AlignRight},
				{Header: "Cost", Align: display.AlignRight},
				{Header: "Hit%", Align: display.AlignRight},
			}
			tableRows := make([][]string, len(rows))
			for i, r := range rows {
				tableRows[i] = []string{r.time, r.input, r.cRead, r.cWrite, r.cost, r.hitPct}
			}
			return Response{Text: fmt.Sprintf("Cache — last %d calls (avg %.1f%% hit)\n\n%s",
				len(recent), avgHit, display.MarkdownTable(cols, tableRows))}, nil
		},
	}
}

// agentFromSession extracts the agent ID (first segment) from a session key.
func agentFromSession(session string) string {
	if i := strings.Index(session, "/"); i > 0 {
		return session[:i]
	}
	return session
}

// truncateSession returns a short prefix of a session key for display.
func truncateSession(session string) string {
	parts := strings.SplitN(session, "/", 3)
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return session
}

// LastCommand returns a /last command showing the most recent API call per agent.
func LastCommand() *Command {
	return &Command{
		Name:        "last",
		Description: "Last API call per agent (or /last <agent>)",
		Category:    "observability",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			entries := readAPILog(cc.APILogPath)
			if len(entries) == 0 {
				return Response{Text: "No API calls logged yet."}, nil
			}

			filter := strings.TrimSpace(req.Args)

			latest := make(map[string]apiEntry)
			var order []string
			for i := len(entries) - 1; i >= 0; i-- {
				agent := agentFromSession(entries[i].Session)
				if filter != "" && agent != filter {
					continue
				}
				if _, exists := latest[agent]; !exists {
					latest[agent] = entries[i]
					order = append(order, agent)
				}
			}

			if len(latest) == 0 {
				if filter != "" {
					return Response{Text: fmt.Sprintf("No API calls for agent %q.", filter)}, nil
				}
				return Response{Text: "No API calls logged yet."}, nil
			}

			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}

			cols := []display.Column{
				{Header: "Agent"},
				{Header: "Time"},
				{Header: "Model"},
				{Header: "Tokens"},
				{Header: "$ Cost", Align: display.AlignRight},
				{Header: "Session"},
			}
			tableRows := make([][]string, 0, len(order))
			for _, agent := range order {
				e := latest[agent]
				tableRows = append(tableRows, []string{
					agent,
					display.CompactRelativeTime(e.Timestamp),
					e.Model,
					fmt.Sprintf("in=%d out=%d cR=%d", e.Input, e.Output, e.CacheRead),
					fmt.Sprintf("%.4f", e.CostUSD),
					truncateSession(e.Session),
				})
			}

			title := "Last API call per agent"
			if filter != "" {
				title = fmt.Sprintf("Last API call — %s", filter)
			}
			return Response{Text: fmt.Sprintf("%s\n\n%s", title, display.MarkdownTable(cols, tableRows))}, nil
		},
	}
}

// CostCommand returns a /cost command showing aggregated costs.
func CostCommand() *Command {
	return &Command{
		Name:        "cost",
		Description: "API cost summary",
		Category:    "observability",
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "today", Data: "today"},
				{Label: "24h", Data: "24h"},
				{Label: "week", Data: "week"},
			}
		},
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			entries := readAPILog(cc.APILogPath)
			if len(entries) == 0 {
				return Response{Text: "No API calls logged yet."}, nil
			}

			scope := strings.ToLower(strings.TrimSpace(req.Args))

			switch scope {
			case "":
				return Response{Text: costUsage()}, nil
			case "today", "session":
				return Response{Text: costToday(entries)}, nil
			case "24h":
				return Response{Text: cost24h(entries)}, nil
			case "week":
				return Response{Text: costWeek(entries)}, nil
			default:
				return Response{Text: costDays(entries, scope)}, nil
			}
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
	SystemSections   []SystemSection  // workspace file sections
	EnvironmentChars int              // environment block chars
	SkillsChars      int              // skills/extra system blocks chars
	Messages         MessageBreakdown // conversation breakdown
	CountTokensFn    func(ctx context.Context) (*TokenCounts, error)
}

// ContextCommand returns a /context command showing context size breakdown.
func ContextCommand() *Command {
	return &Command{
		Name:        "context",
		Description: "Context window breakdown: system prompt, conversation, compaction status",
		Category:    "observability",
		Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
			infoFn := cc.ContextInfoFn
			if infoFn == nil {
				infoFn = buildContextInfo
			}
			info := infoFn(cc)

			entries := readAPILog(cc.APILogPath)
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

			var tc *TokenCounts
			if info.CountTokensFn != nil {
				tc, _ = info.CountTokensFn(ctx)
			}

			totalTokens := lastInput + lastCacheRead + lastCacheWrite
			if tc == nil && totalTokens == 0 {
				return Response{Text: "No API calls yet for this session."}, nil
			}

			headerTokens := totalTokens
			useExact := tc != nil
			if useExact {
				headerTokens = tc.Total
			}

			threshTokens := int(float64(info.ContextLimit) * info.CompactionThresh)
			percentUsed := float64(headerTokens) / float64(info.ContextLimit) * 100
			percentThresh := info.CompactionThresh * 100

			var sb strings.Builder

			tokenLabel := display.FormatCommas(headerTokens)
			if !useExact {
				tokenLabel = "~" + tokenLabel
			}
			sb.WriteString("```\n")
			fmt.Fprintf(&sb, "Context: %s / %s tokens (%.1f%%)\n",
				tokenLabel, display.FormatCommas(info.ContextLimit), percentUsed)
			fmt.Fprintf(&sb, "Compaction at: %s (%.0f%%)\n",
				display.FormatCommas(threshTokens), percentThresh)
			if headerTokens >= threshTokens {
				sb.WriteString("Status: at/above threshold\n")
			} else {
				remaining := threshTokens - headerTokens
				fmt.Fprintf(&sb, "Status: %s tokens until compaction\n", display.FormatCommas(remaining))
			}
			sb.WriteString("```")

			sb.WriteString("\n\n```\n")
			if useExact {
				fmt.Fprintf(&sb, "System prompt: %s tokens\n", display.FormatCommas(tc.System))
				maxNameLen := 0
				for _, s := range tc.Sections {
					if len(s.Name) > maxNameLen {
						maxNameLen = len(s.Name)
					}
				}
				for _, s := range tc.Sections {
					fmt.Fprintf(&sb, "  %-*s  %s tokens\n", maxNameLen, s.Name, display.FormatCommas(s.Tokens))
				}
				fmt.Fprintf(&sb, "\nTools: %s tokens\n", display.FormatCommas(tc.Tools))
			} else {
				totalSystemChars := 0
				for _, s := range info.SystemSections {
					totalSystemChars += s.Chars
				}
				totalSystemChars += info.EnvironmentChars + info.SkillsChars
				fmt.Fprintf(&sb, "System prompt: ~%s tokens\n", display.FormatCommas(totalSystemChars/4))

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
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, "Environment", display.FormatCommas(info.EnvironmentChars/4))
				}
				for _, s := range info.SystemSections {
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, s.Name, display.FormatCommas(s.Chars/4))
				}
				if info.SkillsChars > 0 {
					fmt.Fprintf(&sb, "  %-*s  ~%s tokens\n", maxNameLen, "Skills", display.FormatCommas(info.SkillsChars/4))
				}
			}
			sb.WriteString("```")

			mb := info.Messages
			sb.WriteString("\n\n```\n")
			if useExact {
				fmt.Fprintf(&sb, "Conversation: %s tokens (%d messages)\n",
					display.FormatCommas(tc.Conversation), mb.UserCount+mb.AssistantCount)
			} else {
				totalConvChars := mb.UserChars + mb.AssistantChars + mb.ToolResultChars
				fmt.Fprintf(&sb, "Conversation: ~%s tokens (%d messages)\n",
					display.FormatCommas(totalConvChars/4), mb.UserCount+mb.AssistantCount)
			}
			fmt.Fprintf(&sb, "  User messages     ~%s tokens (%d msgs)\n",
				display.FormatCommas(mb.UserChars/4), mb.UserCount)
			fmt.Fprintf(&sb, "  Assistant         ~%s tokens (%d msgs)\n",
				display.FormatCommas(mb.AssistantChars/4), mb.AssistantCount)
			if mb.ToolResultChars > 0 {
				fmt.Fprintf(&sb, "  Tool results      ~%s tokens\n",
					display.FormatCommas(mb.ToolResultChars/4))
			}
			sb.WriteString("```")

			sb.WriteString("\n\n```\n")
			fmt.Fprintf(&sb, "Last API call tokens:\n")
			fmt.Fprintf(&sb, "  input:       %s\n", display.FormatCommas(lastInput))
			fmt.Fprintf(&sb, "  cache_read:  %s\n", display.FormatCommas(lastCacheRead))
			fmt.Fprintf(&sb, "  cache_write: %s\n", display.FormatCommas(lastCacheWrite))
			fmt.Fprintf(&sb, "  output:      %s\n", display.FormatCommas(lastOutput))
			sb.WriteString("```")

			return Response{Text: sb.String()}, nil
		},
	}
}

// buildContextInfo constructs ContextInfo from CommandContext.
func buildContextInfo(cc CommandContext) ContextInfo {
	sk := cc.DefaultSessionKey()
	model := cc.Agent.SessionModel(sk)

	var sections []SystemSection
	for _, s := range cc.Bootstrap.SectionSizes() {
		sections = append(sections, SystemSection{Name: s.Name, Chars: s.Chars})
	}
	var skillsChars int
	for _, b := range cc.Agent.ExtraSystemBlocks {
		skillsChars += len(b.Text)
	}

	totalSysChars := len(cc.Agent.EnvironmentBlock) + skillsChars
	for _, s := range sections {
		totalSysChars += s.Chars
	}

	var msgs []provider.Message
	if sk != "" {
		if loaded, err := cc.Sessions.LoadFull(sk); err == nil {
			msgs = loaded
		}
	}
	mb := MessageBreakdown{}
	for _, m := range msgs {
		chars := 0
		var hasToolResult bool
		for _, cb := range m.Content {
			switch cb.Type {
			case "text":
				chars += len(cb.Text)
			case "tool_use":
				chars += len(cb.Name) + len(cb.Input)
			case "tool_result":
				chars += len(cb.Content)
				hasToolResult = true
			}
		}
		switch {
		case hasToolResult:
			mb.ToolResultChars += chars
		case m.Role == "user":
			mb.UserChars += chars
			mb.UserCount++
		case m.Role == "assistant":
			mb.AssistantChars += chars
			mb.AssistantCount++
		}
	}

	msgCount := len(msgs)

	var countFn func(ctx context.Context) (*TokenCounts, error)
	if cc.Client != nil {
		countFn = func(ctx context.Context) (*TokenCounts, error) {
			if cc.TokenCountCache != nil {
				if cached := cc.TokenCountCache.Get(msgCount, totalSysChars); cached != nil {
					return cached, nil
				}
			}
			tc, err := countContextTokens(ctx, cc, sk, model)
			if err != nil {
				return nil, err
			}
			if cc.TokenCountCache != nil {
				cc.TokenCountCache.Set(msgCount, totalSysChars, tc)
			}
			return tc, nil
		}
	}

	return ContextInfo{
		SessionKey:       sk,
		Model:            model,
		CompactionThresh: cc.CompactionThreshold,
		ContextLimit:     compaction.ContextLimit(model),
		SystemSections:   sections,
		EnvironmentChars: len(cc.Agent.EnvironmentBlock),
		SkillsChars:      skillsChars,
		Messages:         mb,
		CountTokensFn:    countFn,
	}
}

// countContextTokens calls the counting API to get exact token counts.
func countContextTokens(ctx context.Context, cc CommandContext, sk, model string) (*TokenCounts, error) {
	system := cc.Bootstrap.SystemBlocks()
	if cc.Agent.EnvironmentBlock != "" {
		system = append(system, provider.SystemBlock{Type: "text", Text: cc.Agent.EnvironmentBlock})
	}
	system = append(system, cc.Agent.ExtraSystemBlocks...)

	msgs, _ := cc.Sessions.LoadFull(sk)
	toolDefs := cc.ToolsRegistry.ToolDefs()

	req := &provider.MessageRequest{
		Model:  model,
		System: system,
		Tools:  toolDefs,
	}
	for _, m := range msgs {
		req.Messages = append(req.Messages, m)
	}

	total, err := cc.Client.CountTokens(ctx, req)
	if err != nil {
		return nil, err
	}

	// Compute system tokens by counting without messages
	sysReq := &provider.MessageRequest{
		Model:    model,
		System:   system,
		Tools:    toolDefs,
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("x")}},
	}
	sysTotal, err := cc.Client.CountTokens(ctx, sysReq)
	if err != nil {
		return nil, err
	}

	// Tool tokens: count with just tools + minimal message
	toolReq := &provider.MessageRequest{
		Model:    model,
		Tools:    toolDefs,
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("x")}},
	}
	toolTotal, err := cc.Client.CountTokens(ctx, toolReq)
	if err != nil {
		return nil, err
	}

	// Minimal baseline (no system, no tools)
	baseReq := &provider.MessageRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("x")}},
	}
	baseTotal, _ := cc.Client.CountTokens(ctx, baseReq)

	toolTokens := toolTotal - baseTotal
	if toolTokens < 0 {
		toolTokens = 0
	}
	systemTokens := sysTotal - baseTotal - toolTokens
	if systemTokens < 0 {
		systemTokens = 0
	}
	convTokens := total - sysTotal
	if convTokens < 0 {
		convTokens = 0
	}

	// Per-section breakdown
	var sectionTokens []SectionTokens
	if cc.Agent.EnvironmentBlock != "" {
		envReq := &provider.MessageRequest{
			Model:    model,
			System:   []provider.SystemBlock{{Type: "text", Text: cc.Agent.EnvironmentBlock}},
			Messages: []provider.Message{{Role: "user", Content: provider.TextContent("x")}},
		}
		envTotal, _ := cc.Client.CountTokens(ctx, envReq)
		sectionTokens = append(sectionTokens, SectionTokens{Name: "Environment", Tokens: envTotal - baseTotal})
	}
	for _, s := range cc.Bootstrap.SectionSizes() {
		estimated := s.Chars / 4
		sectionTokens = append(sectionTokens, SectionTokens{Name: s.Name, Tokens: estimated})
	}
	if len(cc.Agent.ExtraSystemBlocks) > 0 {
		var skillChars int
		for _, b := range cc.Agent.ExtraSystemBlocks {
			skillChars += len(b.Text)
		}
		sectionTokens = append(sectionTokens, SectionTokens{Name: "Skills", Tokens: skillChars / 4})
	}

	return &TokenCounts{
		Total:        total,
		System:       systemTokens,
		Conversation: convTokens,
		Tools:        toolTokens,
		Sections:     sectionTokens,
	}, nil
}

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
