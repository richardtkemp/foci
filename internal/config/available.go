package config

import (
	"fmt"
	"foci/internal/display"
	"sort"
	"strings"
)

// availableOption describes a config field that is at its zero/default value.
type availableOption struct {
	Section     string
	Key         string
	Default     string
	Description string
}

// FormatAvailable returns a table of config options that are currently unset
// (at zero or default value) for the agent and global sections.
func FormatAvailable(cfg *Config, agent AgentConfig) string {
	var opts []availableOption

	// Agent fields
	if len(agent.System.SystemFiles) == 0 && len(cfg.System.SystemFiles) == 0 {
		opts = append(opts, availableOption{"agent", "system_files", "[]", "workspace file order for system prompt"})
	}
	if agent.Sessions.BranchOrientationFacetPrompt == nil && cfg.Sessions.BranchOrientationFacetPrompt == nil {
		opts = append(opts, availableOption{"agent", "branch_orientation_facet_prompt", "\"\"", "prompt file for user-attached facet branches"})
	}
	if agent.Sessions.BranchOrientationHeadlessPrompt == nil && cfg.Sessions.BranchOrientationHeadlessPrompt == nil {
		opts = append(opts, availableOption{"agent", "branch_orientation_headless_prompt", "\"\"", "prompt file for headless branches (cron, spawn, keepalive)"})
	}
	tg := agent.Platform("telegram")
	if tg == nil || tg.Bot == "" {
		opts = append(opts, availableOption{"agent.platforms.telegram", "bot", "(agent ID)", "bot name; token via \"telegram.<bot>\" secret"})
	}
	if tg == nil || len(tg.FacetBots) == 0 {
		opts = append(opts, availableOption{"agent.platforms.telegram", "facet_bots", "[]", "additional bot names for facet"})
	}
	if agent.Voice.TTSRate == nil {
		opts = append(opts, availableOption{"agent", "tts_rate", "0", "per-agent TTS speech rate multiplier (0 = use entry rate)"})
	}
	// Only show agent override options when the global fallback isn't covering them.
	globalTg := cfg.Platform("telegram")
	if agent.Notify.StartupNotify == nil && (globalTg == nil || globalTg.Notify.StartupNotify == nil || !*globalTg.Notify.StartupNotify) {
		opts = append(opts, availableOption{"agent", "startup_notify", "(platform)", "send startup notification (nil = use platform)"})
	}
	if agent.Display.ShowToolCalls == nil && (globalTg == nil || globalTg.Display.ShowToolCalls == nil || *globalTg.Display.ShowToolCalls == ToolCallOff) {
		opts = append(opts, availableOption{"agent", "show_tool_calls", "(platform)", "tool call display mode: off, preview, full"})
	}
	if agent.Display.ShowThinking == nil && (globalTg == nil || globalTg.Display.ShowThinking == nil || *globalTg.Display.ShowThinking == ShowThinkingOff) {
		opts = append(opts, availableOption{"agent", "show_thinking", "(platform)", "thinking display mode: off, compact, true"})
	}
	if tg == nil || tg.Display.DisplayWidth == nil {
		dw := 44
		if globalTg != nil && globalTg.Display.DisplayWidth != nil {
			dw = *globalTg.Display.DisplayWidth
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "display_width", fmt.Sprintf("%d", dw), "display width for dividers"})
	}
	if tg == nil || tg.Telegram == nil || tg.Telegram.TableWrapLines == nil {
		twl := 5
		if globalTg != nil && globalTg.Telegram != nil && globalTg.Telegram.TableWrapLines != nil {
			twl = *globalTg.Telegram.TableWrapLines
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "table_wrap_lines", fmt.Sprintf("%d", twl), "max wrapped lines per table cell"})
	}
	if tg == nil || tg.Telegram == nil || tg.Telegram.TableStyle == nil {
		ts := "pretty"
		if globalTg != nil && globalTg.Telegram != nil && globalTg.Telegram.TableStyle != nil {
			ts = *globalTg.Telegram.TableStyle
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "table_style", fmt.Sprintf("%q", ts), "table style: pretty or markdown"})
	}
	hasRecvDir := tg != nil && tg.Display.ReceivedFilesDir != nil && *tg.Display.ReceivedFilesDir != ""
	globalHasRecvDir := globalTg != nil && globalTg.Display.ReceivedFilesDir != nil && *globalTg.Display.ReceivedFilesDir != ""
	if !hasRecvDir && !globalHasRecvDir {
		opts = append(opts, availableOption{"agent.platforms.telegram", "received_files_dir", "\"\"", "save received files to this directory"})
	}
	globalHasAllowed := globalTg != nil && len(globalTg.Access.AllowedUsers) > 0
	if (tg == nil || len(tg.Access.AllowedUsers) == 0) && !globalHasAllowed {
		opts = append(opts, availableOption{"agent.platforms.telegram", "allowed_users", "(global)", "per-agent allowed Telegram user IDs (empty = use global)"})
	}

	// Sessions fields
	if cfg.Sessions.CompactionSummaryPrompt == nil {
		opts = append(opts, availableOption{"sessions", "compaction_summary_prompt", "\"\"", "path to summary prompt file"})
	}
	if cfg.Sessions.CompactionHandoffMsg == nil {
		opts = append(opts, availableOption{"sessions", "compaction_handoff_msg", "\"\"", "handoff message after compaction"})
	}
	if cfg.Notify.CompactionNotify == nil {
		opts = append(opts, availableOption{"notify", "compaction_notify", "true", "send notification on compaction"})
	}
	if cfg.Sessions.MaxSystemPromptFile == 0 {
		opts = append(opts, availableOption{"sessions", "max_system_prompt_chars_file", "20000", "per-file char warning threshold"})
	}
	if cfg.Sessions.MaxSystemPromptTotal == 0 {
		opts = append(opts, availableOption{"sessions", "max_system_prompt_chars_total", "80000", "total system prompt char warning threshold"})
	}
	if cfg.Sessions.CompactionPreserveMessages == nil {
		opts = append(opts, availableOption{"sessions", "compaction_preserve_messages", "0", "preserve last N messages through compaction"})
	}
	if cfg.Sessions.BranchOrientationFacetPrompt == nil {
		opts = append(opts, availableOption{"sessions", "branch_orientation_facet_prompt", "\"\"", "prompt file for user-attached facet branches"})
	}
	if cfg.Sessions.BranchOrientationHeadlessPrompt == nil {
		opts = append(opts, availableOption{"sessions", "branch_orientation_headless_prompt", "\"\"", "prompt file for headless branches (cron, spawn, keepalive)"})
	}

	// Memory fields
	if cfg.Memory.ReindexDebounce == "" || cfg.Memory.ReindexDebounce == "0s" {
		opts = append(opts, availableOption{"memory", "reindex_debounce", "\"0s\"", "delay before reindex"})
	}

	// Debug fields
	if !DerefBool(cfg.Debug.MessagesInLog) {
		opts = append(opts, availableOption{"debug", "messages_in_log", "false", "log user message content to event log"})
	}
	if !cfg.Logging.FullPayload {
		opts = append(opts, availableOption{"logging", "full_payload", "false", "write full API payloads to file"})
	}
	if !cfg.Logging.CacheBustDetect {
		opts = append(opts, availableOption{"logging", "cache_bust_detect", "false", "alert on cache_read drop"})
	}

	// Voice fields
	if len(cfg.TTS) == 0 {
		opts = append(opts, availableOption{"tts", "(none)", "[[tts]]", "add TTS entries for text-to-speech"})
	}
	if len(cfg.STT) == 0 {
		opts = append(opts, availableOption{"stt", "(none)", "[[stt]]", "add STT entries for speech-to-text"})
	}

	// Environment fields
	if cfg.Environment.DocsPath == "" {
		opts = append(opts, availableOption{"environment", "docs_path", "\"\"", "path to platform docs directory"})
	}

	// Skills fields
	if cfg.Skills.Dir == "" {
		opts = append(opts, availableOption{"skills", "dir", "\"\"", "shared skills directory (default: $home/shared/skills/)"})
	}

	// Mana warnings
	mc := Merge(agent.Mana, cfg.Mana)
	if len(mc.Thresholds) == 0 {
		opts = append(opts, availableOption{"mana", "thresholds", "[]", "mana percentages to warn at"})
	}

	if len(opts) == 0 {
		return "All config options are set."
	}

	// Deduplicate: if a key appears in both "agent" and another section,
	// keep only the non-agent entry to avoid redundant display.
	nonAgentKeys := map[string]bool{}
	for _, o := range opts {
		if o.Section != "agent" {
			nonAgentKeys[o.Key] = true
		}
	}
	deduped := opts[:0]
	for _, o := range opts {
		if o.Section == "agent" && nonAgentKeys[o.Key] {
			continue
		}
		deduped = append(deduped, o)
	}
	opts = deduped

	if len(opts) == 0 {
		return "All config options are set."
	}

	// Sort by section, then key within section.
	sort.Slice(opts, func(i, j int) bool {
		if opts[i].Section != opts[j].Section {
			return opts[i].Section < opts[j].Section
		}
		return opts[i].Key < opts[j].Key
	})

	// Group by section, emit a separate 3-column table per section.
	var sections []string
	seen := map[string]bool{}
	grouped := map[string][]availableOption{}
	for _, o := range opts {
		if !seen[o.Section] {
			seen[o.Section] = true
			sections = append(sections, o.Section)
		}
		grouped[o.Section] = append(grouped[o.Section], o)
	}

	cols := []display.Column{
		{Header: "KEY"},
		{Header: "DEFAULT"},
		{Header: "DESCRIPTION"},
	}
	var parts []string
	for _, sec := range sections {
		sOpts := grouped[sec]
		tableRows := make([][]string, len(sOpts))
		for i, o := range sOpts {
			tableRows[i] = []string{o.Key, o.Default, o.Description}
		}
		parts = append(parts, "["+sec+"]\n"+display.MarkdownTable(cols, tableRows))
	}
	return "Unset/default config options:\n\n" + strings.Join(parts, "\n\n")
}
