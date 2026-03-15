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
	if len(agent.SystemFiles) == 0 && len(cfg.Defaults.SystemFiles) == 0 {
		opts = append(opts, availableOption{"agent", "system_files", "[]", "workspace file order for system prompt"})
	}
	if agent.BranchOrientationFacetPrompt == "" && cfg.Sessions.BranchOrientationFacetPrompt == "" {
		opts = append(opts, availableOption{"agent", "branch_orientation_facet_prompt", "\"\"", "prompt file for user-attached facet branches"})
	}
	if agent.BranchOrientationHeadlessPrompt == "" && cfg.Sessions.BranchOrientationHeadlessPrompt == "" {
		opts = append(opts, availableOption{"agent", "branch_orientation_headless_prompt", "\"\"", "prompt file for headless branches (cron, spawn, keepalive)"})
	}
	tg := agent.GetTelegramPlatform()
	if tg == nil || tg.Bot == "" {
		opts = append(opts, availableOption{"agent.platforms.telegram", "bot", "(agent ID)", "bot name; token via \"telegram.<bot>\" secret"})
	}
	if tg == nil || len(tg.FacetBots) == 0 {
		opts = append(opts, availableOption{"agent.platforms.telegram", "facet_bots", "[]", "additional bot names for facet"})
	}
	if agent.TTSRate == 0 {
		opts = append(opts, availableOption{"agent", "tts_rate", "0", "per-agent TTS speech rate multiplier (0 = use entry rate)"})
	}
	// Only show agent override options when the global fallback isn't covering them.
	if agent.StartupNotify == nil && !cfg.Telegram.StartupNotify {
		opts = append(opts, availableOption{"agent", "startup_notify", "(telegram)", "send startup notification (nil = use telegram)"})
	}
	if agent.ShowToolCalls == nil && cfg.Telegram.ShowToolCalls != nil && *cfg.Telegram.ShowToolCalls == ToolCallOff {
		opts = append(opts, availableOption{"agent", "show_tool_calls", "(telegram)", "tool call display mode: off, preview, full (nil = use telegram)"})
	}
	if agent.ShowThinking == nil && cfg.Telegram.ShowThinking != nil && *cfg.Telegram.ShowThinking == ShowThinkingOff {
		opts = append(opts, availableOption{"agent", "show_thinking", "(telegram)", "thinking display mode: off, compact, true (nil = use telegram)"})
	}
	if tg == nil || tg.DisplayWidth == nil {
		dw := 44
		if cfg.Telegram.DisplayWidth != nil {
			dw = *cfg.Telegram.DisplayWidth
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "display_width", fmt.Sprintf("%d", dw), "display width for dividers (nil = use telegram)"})
	}
	if tg == nil || tg.TableWrapLines == nil {
		twl := 5
		if cfg.Telegram.TableWrapLines != nil {
			twl = *cfg.Telegram.TableWrapLines
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "table_wrap_lines", fmt.Sprintf("%d", twl), "max wrapped lines per table cell (nil = use telegram)"})
	}
	if tg == nil || tg.TableStyle == nil {
		ts := "pretty"
		if cfg.Telegram.TableStyle != nil {
			ts = *cfg.Telegram.TableStyle
		}
		opts = append(opts, availableOption{"agent.platforms.telegram", "table_style", fmt.Sprintf("%q", ts), "table style: pretty or markdown (nil = use telegram)"})
	}
	if agent.Effort == "" && cfg.Anthropic.Effort == "" {
		opts = append(opts, availableOption{"agent", "effort", "\"\"", "effort level: low, medium, high (empty = omit)"})
	}
	if agent.CompactionEffort == "" {
		opts = append(opts, availableOption{"agent", "compaction_effort", "\"\"", "effort for compaction API calls (empty = use session effort)"})
	}
	if (tg == nil || tg.ReceivedFilesDir == "") && cfg.Telegram.ReceivedFilesDir == "" {
		opts = append(opts, availableOption{"agent.platforms.telegram", "received_files_dir", "\"\"", "save received files to this directory"})
	}
	if (tg == nil || len(tg.AllowedUsers) == 0) && len(cfg.Telegram.AllowedUsers) == 0 {
		opts = append(opts, availableOption{"agent.platforms.telegram", "allowed_users", "(global)", "per-agent allowed Telegram user IDs (empty = use global)"})
	}

	// Sessions fields
	if cfg.Sessions.CompactionSummaryPrompt == "" {
		opts = append(opts, availableOption{"sessions", "compaction_summary_prompt", "\"\"", "path to summary prompt file"})
	}
	if cfg.Sessions.CompactionHandoffMsg == "" {
		opts = append(opts, availableOption{"sessions", "compaction_handoff_msg", "\"\"", "handoff message after compaction"})
	}
	if cfg.Sessions.CompactionNotify == nil {
		opts = append(opts, availableOption{"sessions", "compaction_notify", "true", "send Telegram notification on compaction"})
	}
	if cfg.Sessions.MaxSystemPromptFile == 0 {
		opts = append(opts, availableOption{"sessions", "max_system_prompt_chars_file", "20000", "per-file char warning threshold"})
	}
	if cfg.Sessions.MaxSystemPromptTotal == 0 {
		opts = append(opts, availableOption{"sessions", "max_system_prompt_chars_total", "80000", "total system prompt char warning threshold"})
	}
	if cfg.Sessions.CompactionPreserveMessages == 0 {
		opts = append(opts, availableOption{"sessions", "compaction_preserve_messages", "0", "preserve last N messages through compaction"})
	}
	if cfg.Sessions.BranchOrientationFacetPrompt == "" {
		opts = append(opts, availableOption{"sessions", "branch_orientation_facet_prompt", "\"\"", "prompt file for user-attached facet branches"})
	}
	if cfg.Sessions.BranchOrientationHeadlessPrompt == "" {
		opts = append(opts, availableOption{"sessions", "branch_orientation_headless_prompt", "\"\"", "prompt file for headless branches (cron, spawn, keepalive)"})
	}

	// Memory fields
	if cfg.Memory.ReindexDebounce == "" || cfg.Memory.ReindexDebounce == "0s" {
		opts = append(opts, availableOption{"memory", "reindex_debounce", "\"0s\"", "delay before reindex"})
	}

	// Logging fields
	if !cfg.Logging.MessagesInLog {
		opts = append(opts, availableOption{"logging", "messages_in_log", "false", "log user message content to event log"})
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
	if len(cfg.Skills.Dirs) == 0 {
		opts = append(opts, availableOption{"skills", "dirs", "[]", "directories to scan for skills"})
	}

	// Usage warnings
	if len(cfg.ManaWarnings.Thresholds) == 0 && len(agent.UsageWarnings.Thresholds) == 0 {
		opts = append(opts, availableOption{"usage_warnings", "thresholds", "[]", "mana percentages to warn at"})
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
