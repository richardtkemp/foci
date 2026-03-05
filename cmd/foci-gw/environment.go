package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"foci/internal/agent"
	"foci/internal/anthropic"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// checkSystemPromptSizes logs warnings if system prompt files exceed thresholds.
func checkSystemPromptSizes(bootstrap *workspace.Bootstrap, sess config.SessionsConfig, agentID string) {
	maxFile := sess.MaxSystemPromptFile
	if maxFile == 0 {
		maxFile = 20000
	}
	maxTotal := sess.MaxSystemPromptTotal
	if maxTotal == 0 {
		maxTotal = 80000
	}
	for _, w := range bootstrap.CheckSizes(maxFile, maxTotal) {
		log.Warnf(agentID, "%s", w)
	}
}

// checkSkillSizes logs warnings if any skill's SKILL.md exceeds maxResultChars.
func checkSkillSizes(registry *skills.Registry, maxResultChars int, agentID string) {
	for _, w := range registry.CheckSizes(maxResultChars) {
		log.Warnf(agentID, "%s", w)
	}
}

// buildContextInfoFn returns the closure used by the /context command.
// It computes system prompt section sizes, message breakdown, and provides
// a CountTokensFn that fires parallel API calls to count tokens per section.
func buildContextInfoFn(
	ag *agent.Agent,
	bootstrap *workspace.Bootstrap,
	registry *tools.Registry,
	acfg config.AgentConfig,
	client provider.Client,
	sessions *session.Store,
	defaultSessionKey func() string,
	compactionThreshold float64,
) func() command.ContextInfo {
	// Token count cache (persists across calls, invalidated when context changes)
	var (
		tcCacheMu     sync.Mutex
		tcCacheCounts *command.TokenCounts
		tcCacheMsgCnt int
		tcCacheSysChr int
	)

	return func() command.ContextInfo {
		// System prompt section sizes from workspace files
		var sections []command.SystemSection
		for _, s := range bootstrap.SectionSizes() {
			sections = append(sections, command.SystemSection{Name: s.Name, Chars: s.Chars})
		}
		// Skills/extra system blocks character count
		var skillsChars int
		for _, b := range ag.ExtraSystemBlocks {
			skillsChars += len(b.Text)
		}
		// System chars total (used as cache key)
		totalSysChars := len(ag.EnvironmentBlock) + skillsChars
		for _, s := range sections {
			totalSysChars += s.Chars
		}
		// Load messages once (shared between breakdown and counting)
		sk := defaultSessionKey()
		var msgs []anthropic.Message
		if sk != "" {
			if loaded, err := sessions.LoadFull(sk); err == nil {
				msgs = loaded
			}
		}
		msgCount := len(msgs)
		// Message breakdown from loaded messages
		var mb command.MessageBreakdown
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
		return command.ContextInfo{
			SessionKey:       sk,
			Model:            ag.Model,
			CompactionThresh: compactionThreshold,
			ContextLimit:     compaction.ContextLimit(ag.Model),
			SystemSections:   sections,
			EnvironmentChars: len(ag.EnvironmentBlock),
			SkillsChars:      skillsChars,
			Messages:         mb,
			CountTokensFn: func(ctx context.Context) (*command.TokenCounts, error) {
				// Check cache
				tcCacheMu.Lock()
				if tcCacheCounts != nil && tcCacheMsgCnt == msgCount && tcCacheSysChr == totalSysChars {
					r := tcCacheCounts
					tcCacheMu.Unlock()
					return r, nil
				}
				tcCacheMu.Unlock()

				// Build full system blocks (same assembly as agent)
				bootstrapBlocks := bootstrap.SystemBlocks()
				bootstrapSizes := bootstrap.SectionSizes()
				// Strip cache_control from bootstrap blocks
				for i := range bootstrapBlocks {
					bootstrapBlocks[i].CacheControl = nil
				}

				var allSystem []anthropic.SystemBlock
				if ag.EnvironmentBlock != "" {
					allSystem = append(allSystem, anthropic.SystemBlock{Type: "text", Text: ag.EnvironmentBlock})
				}
				allSystem = append(allSystem, bootstrapBlocks...)
				var cleanExtra []anthropic.SystemBlock
				if len(ag.ExtraSystemBlocks) > 0 {
					cleanExtra = make([]anthropic.SystemBlock, len(ag.ExtraSystemBlocks))
					copy(cleanExtra, ag.ExtraSystemBlocks)
					for i := range cleanExtra {
						cleanExtra[i].CacheControl = nil
					}
					allSystem = append(allSystem, cleanExtra...)
				}

				// Build per-section list for individual counting
				type sectionDef struct {
					name   string
					blocks []anthropic.SystemBlock
				}
				var secs []sectionDef
				if ag.EnvironmentBlock != "" {
					secs = append(secs, sectionDef{
						name:   "Environment",
						blocks: []anthropic.SystemBlock{{Type: "text", Text: ag.EnvironmentBlock}},
					})
				}
				for i, sz := range bootstrapSizes {
					if i < len(bootstrapBlocks) {
						secs = append(secs, sectionDef{
							name:   sz.Name,
							blocks: []anthropic.SystemBlock{bootstrapBlocks[i]},
						})
					}
				}
				if len(cleanExtra) > 0 {
					secs = append(secs, sectionDef{name: "Skills", blocks: cleanExtra})
				}

				// Common request components
				dummyMsgs := []anthropic.Message{
					{Role: "user", Content: anthropic.TextContent(".")},
				}
				toolDefs := registry.ToolDefs()
				maxOutput := acfg.MaxOutputTokens
				if maxOutput <= 0 {
					maxOutput = 8192
				}
				messages := msgs
				if len(messages) == 0 {
					messages = dummyMsgs
				}

				// Parallel API calls
				var wg sync.WaitGroup
				var fullCount, systemCount, baselineCount int
				var fullErr, systemErr, baselineErr error
				rawSecCounts := make([]int, len(secs))
				rawSecErrs := make([]error, len(secs))

				wg.Add(3 + len(secs))

				go func() {
					defer wg.Done()
					fullCount, fullErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						System: allSystem, Messages: messages, Tools: toolDefs,
					})
				}()
				go func() {
					defer wg.Done()
					systemCount, systemErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						System: allSystem, Messages: dummyMsgs, Tools: toolDefs,
					})
				}()
				go func() {
					defer wg.Done()
					baselineCount, baselineErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						Messages: dummyMsgs, Tools: toolDefs,
					})
				}()
				for i, sec := range secs {
					i, sec := i, sec
					go func() {
						defer wg.Done()
						rawSecCounts[i], rawSecErrs[i] = client.CountTokens(ctx, &anthropic.MessageRequest{
							Model: ag.Model, MaxTokens: maxOutput,
							System: sec.blocks, Messages: dummyMsgs, Tools: toolDefs,
						})
					}()
				}

				wg.Wait()

				if fullErr != nil {
					return nil, fullErr
				}
				if systemErr != nil {
					return nil, systemErr
				}
				if baselineErr != nil {
					return nil, baselineErr
				}

				tc := &command.TokenCounts{
					Total:        fullCount,
					System:       systemCount - baselineCount,
					Conversation: fullCount - systemCount,
					Tools:        baselineCount,
				}
				for i, sec := range secs {
					tokens := 0
					if rawSecErrs[i] == nil {
						tokens = rawSecCounts[i] - baselineCount
						if tokens < 0 {
							tokens = 0
						}
					}
					tc.Sections = append(tc.Sections, command.SectionTokens{
						Name: sec.name, Tokens: tokens,
					})
				}

				// Update cache
				tcCacheMu.Lock()
				tcCacheCounts = tc
				tcCacheMsgCnt = msgCount
				tcCacheSysChr = totalSysChars
				tcCacheMu.Unlock()

				return tc, nil
			},
		}
	}
}

// countCrontabJobs counts the number of active cron jobs for the current user
func countCrontabJobs() int {
	cmd := exec.Command("sh", "-c", "crontab -l 2>/dev/null | grep -v '^#' | grep -v '^$' | wc -l")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0
	}
	return count
}

// buildEnvironmentBlock generates the environment system block content
// from config values known at startup.
func buildEnvironmentBlock(acfg config.AgentConfig, configPath string, cfg *config.Config, crontabCount int) string {
	logDir := filepath.Dir(cfg.Logging.EventFile)

	var b strings.Builder
	b.WriteString("# Environment\n\n")
	b.WriteString("You are running on **foci**, an AI agent platform.\n\n")

	// Workspace
	b.WriteString("## Workspace\n")
	fmt.Fprintf(&b, "- Workspace: %s\n", acfg.Workspace)
	fmt.Fprintf(&b, "- Agent ID: %s\n", acfg.ID)
	b.WriteString("- Platform: foci (https://github.com/richardtkemp/foci)\n")
	if cfg.Environment.DocsPath != "" {
		fmt.Fprintf(&b, "- Platform docs: %s\n", cfg.Environment.DocsPath)
	}
	if acfg.TelegramBot != "" {
		b.WriteString("- Messaging: Telegram\n")
	}
	fmt.Fprintf(&b, "- You may schedule recurring tasks using crontab. You have %d jobs scheduled.\n", crontabCount)

	// Paths
	b.WriteString("\n## Paths\n")
	fmt.Fprintf(&b, "- Config: %s\n", configPath)
	fmt.Fprintf(&b, "- Logs: %s\n", logDir)

	// Message Metadata
	b.WriteString("\n## Message Metadata\n")
	b.WriteString("Every inbound message includes a `[meta]` header with:\n")
	b.WriteString("- **time** — UTC timestamp\n")
	b.WriteString("- **gap** — time since last message\n")
	b.WriteString("- **model** — current model\n")
	b.WriteString("- **prev_cost** — USD equivalent cost of previous turn\n")
	b.WriteString("- **prev_tokens** — token breakdown: in (new input), out (output), cR (cache read), cW (cache write)\n")
	b.WriteString("- **mana** — remaining API quota percentage, followed by 🟢 (above invest threshold — safe for heavy work) or 🔴 (low — conserve, avoid expensive operations)\n")

	// Session Structure
	b.WriteString("\n## Session Structure\n")
	b.WriteString("Your context is assembled from: this environment block, character files, a secrets list, and the conversation history.\n")
	sysFiles := acfg.SystemFiles
	if len(sysFiles) == 0 {
		sysFiles = workspace.DefaultFileOrder
	}
	b.WriteString("Character files (in order): ")
	b.WriteString(strings.Join(sysFiles, ", "))
	b.WriteString("\n")
	b.WriteString("The human only sees the conversation — they cannot see your system prompt, character files, or this environment block. ")
	b.WriteString("Do not assume shared context when referencing system prompt content. If you need the human to understand something from your instructions, explain it in your own words.\n")

	// Visibility: resolve effective show_tool_calls and show_thinking
	toolCalls := config.ToolCallOff
	if acfg.ShowToolCalls != nil {
		toolCalls = *acfg.ShowToolCalls
	} else if cfg.Defaults.ShowToolCalls != nil {
		toolCalls = *cfg.Defaults.ShowToolCalls
	}
	thinking := config.ShowThinkingOff
	if acfg.ShowThinking != nil {
		thinking = *acfg.ShowThinking
	} else if cfg.Defaults.ShowThinking != nil {
		thinking = *cfg.Defaults.ShowThinking
	}
	var toolDesc, thinkDesc string
	switch toolCalls {
	case config.ToolCallOff:
		toolDesc = "Tool calls are hidden from the user — narrate important actions in your replies."
	case config.ToolCallPreview:
		toolDesc = "Tool calls are shown as brief previews (tool name only) — the user sees what tools you use but not the details."
	case config.ToolCallFull:
		toolDesc = "Tool calls are fully visible — the user can see your tool inputs and outputs."
	}
	switch thinking {
	case config.ShowThinkingOff:
		thinkDesc = "Your thinking is hidden from the user."
	case config.ShowThinkingCompact:
		thinkDesc = "Your thinking is available behind a toggle button."
	case config.ShowThinkingTrue:
		thinkDesc = "Your thinking is shown inline before each response."
	}
	if toolDesc != "" || thinkDesc != "" {
		b.WriteString("\n## Visibility\n")
		if toolDesc != "" {
			b.WriteString(toolDesc + "\n")
		}
		if thinkDesc != "" {
			b.WriteString(thinkDesc + "\n")
		}
	}

	return b.String()
}

func sessionMessageCount(sessions *session.Store, key string) int {
	n, err := sessions.MessageCount(key)
	if err != nil {
		log.Warnf("main", "message count for %s: %v", key, err)
		return 0
	}
	return n
}
