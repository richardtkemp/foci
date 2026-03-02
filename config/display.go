package config

import (
	"bytes"
	"fmt"
	"foci/table"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// configRow is a single row in the /config table output.
type configRow struct {
	Section string
	Key     string
	Value   string
}

// firstWidth returns the first element of maxWidth or 0.
func firstWidth(maxWidth []int) int {
	if len(maxWidth) > 0 {
		return maxWidth[0]
	}
	return 0
}

// FormatConfig returns an aligned columnar table of the running config
// for the given agent. Secrets are redacted. An optional maxWidth constrains
// table columns.
func FormatConfig(cfg *Config, agent AgentConfig, maxWidth ...int) string {
	mw := firstWidth(maxWidth)
	rows := collectAgentRows(agent)
	add := func(section, key string, val interface{}) {
		rows = append(rows, configRow{section, key, formatValue(val)})
	}

	// defaults
	add("defaults", "model", cfg.Defaults.Model)

	add("defaults", "max_tool_loops", cfg.Defaults.MaxToolLoops)
	add("defaults", "max_output_tokens", cfg.Defaults.MaxOutputTokens)
	if cfg.Defaults.Effort != "" {
		add("defaults", "effort", cfg.Defaults.Effort)
	}
	if cfg.Defaults.DuplicateMessages {
		add("defaults", "duplicate_messages", cfg.Defaults.DuplicateMessages)
	}
	if cfg.Defaults.InjectAgentWarnings {
		add("defaults", "inject_agent_warnings", cfg.Defaults.InjectAgentWarnings)
	}
	if cfg.Defaults.ShowToolCalls != nil {
		add("defaults", "show_tool_calls", string(*cfg.Defaults.ShowToolCalls))
	}
	if cfg.Defaults.ShowThinking != nil {
		add("defaults", "show_thinking", string(*cfg.Defaults.ShowThinking))
	}
	if cfg.Defaults.DisplayWidth != nil {
		add("defaults", "display_width", *cfg.Defaults.DisplayWidth)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		add("defaults", "system_files", cfg.Defaults.SystemFiles)
	}

	// telegram
	if len(cfg.Telegram.AllowedUsers) > 0 {
		add("telegram", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.MultiballBots) > 0 {
		add("telegram", "multiball_bots", cfg.Telegram.MultiballBots)
	}
	if len(cfg.Telegram.StopAliases) > 0 {
		add("telegram", "stop_aliases", cfg.Telegram.StopAliases)
	}
	add("telegram", "enable_stop_aliases", cfg.Telegram.EnableStopAliases)
	add("telegram", "enable_startup_notify", cfg.Telegram.EnableStartupNotify)
	add("telegram", "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL)
	add("telegram", "message_queue_size", cfg.Telegram.MessageQueueSize)
	add("telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	if cfg.Telegram.ReceivedFilesDir != "" {
		add("telegram", "received_files_dir", cfg.Telegram.ReceivedFilesDir)
	}

	// sessions
	add("sessions", "dir", cfg.Sessions.Dir)
	add("sessions", "compaction_threshold", cfg.Sessions.CompactionThreshold)
	add("sessions", "compaction_max_tokens", cfg.Sessions.CompactionMaxTokens)
	add("sessions", "compaction_min_messages", cfg.Sessions.CompactionMinMessages)
	if cfg.Sessions.CompactionSummaryPrompt != "" {
		add("sessions", "compaction_summary_prompt", cfg.Sessions.CompactionSummaryPrompt)
	}
	if cfg.Sessions.CompactionHandoffMsg != "" {
		add("sessions", "compaction_handoff_msg", cfg.Sessions.CompactionHandoffMsg)
	}
	if cfg.Sessions.CompactionNotify != nil {
		add("sessions", "compaction_notify", *cfg.Sessions.CompactionNotify)
	}
	add("sessions", "compaction_debug", cfg.Sessions.CompactionDebug)
	add("sessions", "compaction_preserve_messages", cfg.Sessions.CompactionPreserveMessages)
	add("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	add("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.BranchOrientationPrompt != "" {
		add("sessions", "branch_orientation_prompt", cfg.Sessions.BranchOrientationPrompt)
	}

	// memory
	if len(cfg.Memory.Sources) > 0 {
		add("memory", "sources", fmt.Sprintf("(%d configured)", len(cfg.Memory.Sources)))
	}
	if cfg.Memory.ReindexDebounce != "" {
		add("memory", "reindex_debounce", cfg.Memory.ReindexDebounce)
	}
	add("memory", "conversation_weight", cfg.Memory.ConversationWeight)
	add("memory", "search_limit", cfg.Memory.SearchLimit)

	// logging
	add("logging", "level", cfg.Logging.Level)
	add("logging", "event_file", cfg.Logging.EventFile)
	add("logging", "api_file", cfg.Logging.APIFile)
	add("logging", "api_db", cfg.Logging.APIDB)
	add("logging", "conversation_file", cfg.Logging.ConversationFile)
	add("logging", "full_payload", cfg.Logging.FullPayload)
	if cfg.Logging.PayloadFile != "" {
		add("logging", "payload_file", cfg.Logging.PayloadFile)
	}
	add("logging", "messages_in_log", cfg.Logging.MessagesInLog)
	add("logging", "cache_bust_detect", cfg.Logging.CacheBustDetect)
	add("logging", "cache_bust_idle_minutes", cfg.Logging.CacheBustIdleMinutes)
	add("logging", "warning_max_per_window", cfg.Logging.WarningMaxPerWindow)
	add("logging", "warning_window_duration", cfg.Logging.WarningWindowDuration)
	add("logging", "log_rotation", cfg.Logging.LogRotation)
	add("logging", "rotation_period", cfg.Logging.RotationPeriod)
	add("logging", "retention_period", cfg.Logging.RetentionPeriod)
	add("logging", "rotation_max_line_size", cfg.Logging.RotationMaxLineSize)
	if cfg.Logging.ArchiveDir != "" {
		add("logging", "archive_dir", cfg.Logging.ArchiveDir)
	}

	// http
	add("http", "bind", cfg.HTTP.Bind)
	add("http", "port", cfg.HTTP.Port)
	add("http", "graceful_shutdown_timeout", cfg.HTTP.GracefulShutdownTimeout)

	// tools
	add("tools", "max_result_chars", cfg.Tools.MaxResultChars)
	add("tools", "temp_dir", cfg.Tools.TempDir)
	add("tools", "tmux_cols", cfg.Tools.TmuxCols)
	add("tools", "tmux_rows", cfg.Tools.TmuxRows)
	add("tools", "exec_auto_background", cfg.Tools.ExecAutoBackground)
	add("tools", "exec_default_timeout", cfg.Tools.ExecDefaultTimeout)
	add("tools", "max_summary_chars", cfg.Tools.MaxSummaryChars)
	add("tools", "auto_summarise", cfg.Tools.AutoSummarise)
	add("tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	add("tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	add("tools", "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	add("tools", "web_search_timeout", cfg.Tools.WebSearchTimeout)
	add("tools", "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	add("tools", "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	add("tools", "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	add("tools", "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	add("tools", "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	add("tools", "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)
	add("tools", "tmux_braindead", cfg.Tools.TmuxBraindead)
	add("tools", "tmux_watch_threshold", cfg.Tools.TmuxWatchThreshold)

	// environment
	add("environment", "enabled", cfg.Environment.Enabled)
	if cfg.Environment.DocsPath != "" {
		add("environment", "docs_path", cfg.Environment.DocsPath)
	}

	// skills
	if len(cfg.Skills.Dirs) > 0 {
		add("skills", "dirs", cfg.Skills.Dirs)
	}

	// cache
	add("cache", "strategy", cfg.Cache.Strategy)

	// usage_warnings
	add("usage_warnings", "name", cfg.ManaWarnings.Name)
	if len(cfg.ManaWarnings.Thresholds) > 0 {
		add("usage_warnings", "thresholds", cfg.ManaWarnings.Thresholds)
	}

	// voice
	if cfg.Voice.STTEndpoint != "" {
		add("voice", "stt_endpoint", cfg.Voice.STTEndpoint)
	}
	if cfg.Voice.STTModel != "" {
		add("voice", "stt_model", cfg.Voice.STTModel)
	}
	if cfg.Voice.TTSProvider != "" {
		add("voice", "tts_provider", cfg.Voice.TTSProvider)
	}
	if cfg.Voice.TTSEndpoint != "" {
		add("voice", "tts_endpoint", cfg.Voice.TTSEndpoint)
	}
	if cfg.Voice.TTSModel != "" {
		add("voice", "tts_model", cfg.Voice.TTSModel)
	}
	if cfg.Voice.TTSVoice != "" {
		add("voice", "tts_voice", cfg.Voice.TTSVoice)
	}
	if cfg.Voice.TTSRate != 0 {
		add("voice", "tts_rate", cfg.Voice.TTSRate)
	}

	// database
	add("database", "busy_timeout", cfg.Database.BusyTimeout)

	// anthropic
	add("anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout)
	add("anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)

	// prompt_rules
	if len(cfg.PromptRules) > 0 {
		add("prompt_rules", fmt.Sprintf("(%d rules)", len(cfg.PromptRules)), "")
	}

	return formatTableBySection(rows, mw)
}

// collectAgentRows returns the standard set of agent config display rows.
func collectAgentRows(agent AgentConfig) []configRow {
	var rows []configRow
	add := func(key string, val interface{}) {
		rows = append(rows, configRow{"agent", key, formatValue(val)})
	}
	add("id", agent.ID)
	add("model", agent.Model)
	add("workspace", agent.Workspace)

	if len(agent.SystemFiles) > 0 {
		add("system_files", agent.SystemFiles)
	}
	add("duplicate_messages", agent.DuplicateMessages)
	if agent.BranchOrientationPrompt != "" {
		add("branch_orientation_prompt", agent.BranchOrientationPrompt)
	}
	if agent.TelegramBot != "" {
		add("telegram_bot", agent.TelegramBot)
	}
	if len(agent.MultiballBots) > 0 {
		add("multiball_bots", agent.MultiballBots)
	}
	add("max_tool_loops", agent.MaxToolLoops)
	add("max_output_tokens", agent.MaxOutputTokens)
	if agent.Effort != "" {
		add("effort", agent.Effort)
	}
	if agent.TTSRate != 0 {
		add("tts_rate", agent.TTSRate)
	}
	add("inject_agent_warnings", agent.InjectAgentWarnings)
	if agent.StartupNotification != nil {
		add("startup_notification", *agent.StartupNotification)
	}
	if agent.ShowToolCalls != nil {
		add("show_tool_calls", string(*agent.ShowToolCalls))
	}
	if agent.ShowThinking != nil {
		add("show_thinking", string(*agent.ShowThinking))
	}
	if agent.DisplayWidth != nil {
		add("display_width", *agent.DisplayWidth)
	}
	if agent.MessagesInLog != nil {
		add("messages_in_log", *agent.MessagesInLog)
	}
	if agent.ReceivedFilesDir != "" {
		add("received_files_dir", agent.ReceivedFilesDir)
	}
	if len(agent.AllowedUsers) > 0 {
		add("allowed_users", agent.AllowedUsers)
	}
	if agent.CompactionPreserveMessages != nil {
		add("compaction_preserve_messages", *agent.CompactionPreserveMessages)
	}
	if agent.CompactionEffort != "" {
		add("compaction_effort", agent.CompactionEffort)
	}
	if len(agent.UsageWarnings.Thresholds) > 0 {
		add("usage_warnings.thresholds", agent.UsageWarnings.Thresholds)
	}
	add("keepalive.enabled", agent.Keepalive.Enabled)
	add("keepalive.interval", agent.Keepalive.Interval)
	add("keepalive.prompt", agent.Keepalive.Prompt)
	add("background.enabled", agent.Background.Enabled)
	add("background.interval", agent.Background.Interval)
	add("background.prompt", agent.Background.Prompt)
	add("background.invest_interval", agent.Background.InvestInterval)
	add("background.mana_staleness_timeout", agent.Background.ManaStalenessTimeout)
	add("memory_formation.interval", agent.MemoryFormation.Interval)
	if agent.MemoryFormation.IntervalEnabled != nil {
		add("memory_formation.interval_enabled", *agent.MemoryFormation.IntervalEnabled)
	}
	if agent.MemoryFormation.IntervalPrompt != "" {
		add("memory_formation.interval_prompt", agent.MemoryFormation.IntervalPrompt)
	}
	add("memory_formation.consolidation_interval", agent.MemoryFormation.ConsolidationInterval)
	if agent.MemoryFormation.ConsolidationEnabled != nil {
		add("memory_formation.consolidation_enabled", *agent.MemoryFormation.ConsolidationEnabled)
	}
	if agent.MemoryFormation.ConsolidationPrompt != "" {
		add("memory_formation.consolidation_prompt", agent.MemoryFormation.ConsolidationPrompt)
	}
	if agent.MemoryFormation.SessionEndEnabled != nil {
		add("memory_formation.session_end_enabled", *agent.MemoryFormation.SessionEndEnabled)
	}
	if agent.MemoryFormation.SessionEndPrompt != "" {
		add("memory_formation.session_end_prompt", agent.MemoryFormation.SessionEndPrompt)
	}
	return rows
}

// FormatConfigGrouped returns per-group config tables, each wrapped in a
// markdown code block. The first table is "Global" config (all non-agent
// sections), followed by one table for the given agent. Each table is small
// enough to fit in a single Telegram message.
func FormatConfigGrouped(cfg *Config, agent AgentConfig, maxWidth ...int) []string {
	mw := firstWidth(maxWidth)
	// Build global rows (everything except agent-specific)
	var globalRows []configRow

	// isDefined returns true if a key was explicitly set in the TOML file.
	isDefined := func(path string) bool {
		if cfg.DefinedKeys == nil {
			return true // no metadata — assume everything is explicit
		}
		return cfg.DefinedKeys[path]
	}

	// annotate appends indicators to a value string.
	annotate := func(val, tomlPath string, overridden bool) string {
		if overridden {
			val += " (overridden)"
		}
		if !isDefined(tomlPath) {
			val += " (default)"
		}
		return val
	}

	addGlobal := func(section, key string, val interface{}) {
		v := formatValue(val)
		if !isDefined(section + "." + key) {
			v += " (default)"
		}
		globalRows = append(globalRows, configRow{section, key, v})
	}

	// addDefault adds a defaults row with override/default annotations.
	addDefault := func(key string, val interface{}, overridden bool) {
		v := annotate(formatValue(val), "defaults."+key, overridden)
		globalRows = append(globalRows, configRow{"defaults", key, v})
	}

	// defaults — compare with agent values to detect overrides.
	addDefault("model", cfg.Defaults.Model, agent.Model != cfg.Defaults.Model)

	addDefault("max_tool_loops", cfg.Defaults.MaxToolLoops, agent.MaxToolLoops != cfg.Defaults.MaxToolLoops)
	addDefault("max_output_tokens", cfg.Defaults.MaxOutputTokens, agent.MaxOutputTokens != cfg.Defaults.MaxOutputTokens)
	if cfg.Defaults.Effort != "" {
		addDefault("effort", cfg.Defaults.Effort, agent.Effort != cfg.Defaults.Effort)
	}
	if cfg.Defaults.DuplicateMessages {
		addDefault("duplicate_messages", cfg.Defaults.DuplicateMessages, false)
	}
	if cfg.Defaults.InjectAgentWarnings {
		addDefault("inject_agent_warnings", cfg.Defaults.InjectAgentWarnings, false)
	}
	if cfg.Defaults.ShowToolCalls != nil {
		addDefault("show_tool_calls", string(*cfg.Defaults.ShowToolCalls), false)
	}
	if cfg.Defaults.ShowThinking != nil {
		addDefault("show_thinking", string(*cfg.Defaults.ShowThinking), false)
	}
	if cfg.Defaults.DisplayWidth != nil {
		addDefault("display_width", *cfg.Defaults.DisplayWidth, false)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		addDefault("system_files", cfg.Defaults.SystemFiles, false)
	}
	addGlobal("keepalive", "enabled", cfg.Keepalive.Enabled)
	addGlobal("keepalive", "interval", cfg.Keepalive.Interval)
	addGlobal("keepalive", "prompt", cfg.Keepalive.Prompt)
	addGlobal("background", "enabled", cfg.Background.Enabled)
	addGlobal("background", "interval", cfg.Background.Interval)
	addGlobal("background", "prompt", cfg.Background.Prompt)
	addGlobal("background", "invest_interval", cfg.Background.InvestInterval)
	addGlobal("background", "mana_staleness_timeout", cfg.Background.ManaStalenessTimeout)
	addGlobal("memory_formation", "interval", cfg.MemoryFormation.Interval)
	addGlobal("memory_formation", "consolidation_interval", cfg.MemoryFormation.ConsolidationInterval)
	if cfg.MemoryFormation.IntervalEnabled != nil {
		addGlobal("memory_formation", "interval_enabled", *cfg.MemoryFormation.IntervalEnabled)
	}
	if cfg.MemoryFormation.ConsolidationEnabled != nil {
		addGlobal("memory_formation", "consolidation_enabled", *cfg.MemoryFormation.ConsolidationEnabled)
	}
	if cfg.MemoryFormation.SessionEndEnabled != nil {
		addGlobal("memory_formation", "session_end_enabled", *cfg.MemoryFormation.SessionEndEnabled)
	}
	if len(cfg.Telegram.AllowedUsers) > 0 {
		addGlobal("telegram", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.MultiballBots) > 0 {
		addGlobal("telegram", "multiball_bots", cfg.Telegram.MultiballBots)
	}
	if len(cfg.Telegram.StopAliases) > 0 {
		addGlobal("telegram", "stop_aliases", cfg.Telegram.StopAliases)
	}
	addGlobal("telegram", "enable_stop_aliases", cfg.Telegram.EnableStopAliases)
	addGlobal("telegram", "enable_startup_notify", cfg.Telegram.EnableStartupNotify)
	addGlobal("telegram", "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL)
	addGlobal("telegram", "message_queue_size", cfg.Telegram.MessageQueueSize)
	addGlobal("telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	if cfg.Telegram.ReceivedFilesDir != "" {
		addGlobal("telegram", "received_files_dir", cfg.Telegram.ReceivedFilesDir)
	}
	addGlobal("sessions", "dir", cfg.Sessions.Dir)
	addGlobal("sessions", "compaction_threshold", cfg.Sessions.CompactionThreshold)
	addGlobal("sessions", "compaction_max_tokens", cfg.Sessions.CompactionMaxTokens)
	addGlobal("sessions", "compaction_min_messages", cfg.Sessions.CompactionMinMessages)
	if cfg.Sessions.CompactionSummaryPrompt != "" {
		addGlobal("sessions", "compaction_summary_prompt", cfg.Sessions.CompactionSummaryPrompt)
	}
	if cfg.Sessions.CompactionHandoffMsg != "" {
		addGlobal("sessions", "compaction_handoff_msg", cfg.Sessions.CompactionHandoffMsg)
	}
	if cfg.Sessions.CompactionNotify != nil {
		addGlobal("sessions", "compaction_notify", *cfg.Sessions.CompactionNotify)
	}
	addGlobal("sessions", "compaction_debug", cfg.Sessions.CompactionDebug)
	addGlobal("sessions", "compaction_preserve_messages", cfg.Sessions.CompactionPreserveMessages)
	addGlobal("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	addGlobal("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.BranchOrientationPrompt != "" {
		addGlobal("sessions", "branch_orientation_prompt", cfg.Sessions.BranchOrientationPrompt)
	}
	if len(cfg.Memory.Sources) > 0 {
		addGlobal("memory", "sources", fmt.Sprintf("(%d configured)", len(cfg.Memory.Sources)))
	}
	if cfg.Memory.ReindexDebounce != "" {
		addGlobal("memory", "reindex_debounce", cfg.Memory.ReindexDebounce)
	}
	addGlobal("memory", "conversation_weight", cfg.Memory.ConversationWeight)
	addGlobal("memory", "search_limit", cfg.Memory.SearchLimit)
	addGlobal("logging", "level", cfg.Logging.Level)
	addGlobal("logging", "event_file", cfg.Logging.EventFile)
	addGlobal("logging", "api_file", cfg.Logging.APIFile)
	addGlobal("logging", "api_db", cfg.Logging.APIDB)
	addGlobal("logging", "conversation_file", cfg.Logging.ConversationFile)
	addGlobal("logging", "full_payload", cfg.Logging.FullPayload)
	if cfg.Logging.PayloadFile != "" {
		addGlobal("logging", "payload_file", cfg.Logging.PayloadFile)
	}
	addGlobal("logging", "messages_in_log", cfg.Logging.MessagesInLog)
	addGlobal("logging", "cache_bust_detect", cfg.Logging.CacheBustDetect)
	addGlobal("logging", "cache_bust_idle_minutes", cfg.Logging.CacheBustIdleMinutes)
	addGlobal("logging", "warning_max_per_window", cfg.Logging.WarningMaxPerWindow)
	addGlobal("logging", "warning_window_duration", cfg.Logging.WarningWindowDuration)
	addGlobal("logging", "log_rotation", cfg.Logging.LogRotation)
	addGlobal("logging", "rotation_period", cfg.Logging.RotationPeriod)
	addGlobal("logging", "retention_period", cfg.Logging.RetentionPeriod)
	addGlobal("logging", "rotation_max_line_size", cfg.Logging.RotationMaxLineSize)
	if cfg.Logging.ArchiveDir != "" {
		addGlobal("logging", "archive_dir", cfg.Logging.ArchiveDir)
	}
	addGlobal("http", "bind", cfg.HTTP.Bind)
	addGlobal("http", "port", cfg.HTTP.Port)
	addGlobal("http", "graceful_shutdown_timeout", cfg.HTTP.GracefulShutdownTimeout)
	addGlobal("tools", "max_result_chars", cfg.Tools.MaxResultChars)
	addGlobal("tools", "temp_dir", cfg.Tools.TempDir)
	addGlobal("tools", "tmux_cols", cfg.Tools.TmuxCols)
	addGlobal("tools", "tmux_rows", cfg.Tools.TmuxRows)
	addGlobal("tools", "exec_auto_background", cfg.Tools.ExecAutoBackground)
	addGlobal("tools", "exec_default_timeout", cfg.Tools.ExecDefaultTimeout)
	addGlobal("tools", "max_summary_chars", cfg.Tools.MaxSummaryChars)
	addGlobal("tools", "auto_summarise", cfg.Tools.AutoSummarise)
	addGlobal("tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	addGlobal("tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	addGlobal("tools", "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	addGlobal("tools", "web_search_timeout", cfg.Tools.WebSearchTimeout)
	addGlobal("tools", "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	addGlobal("tools", "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	addGlobal("tools", "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	addGlobal("tools", "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	addGlobal("tools", "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	addGlobal("tools", "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)
	addGlobal("tools", "tmux_braindead", cfg.Tools.TmuxBraindead)
	addGlobal("tools", "tmux_watch_threshold", cfg.Tools.TmuxWatchThreshold)
	addGlobal("environment", "enabled", cfg.Environment.Enabled)
	if cfg.Environment.DocsPath != "" {
		addGlobal("environment", "docs_path", cfg.Environment.DocsPath)
	}
	if len(cfg.Skills.Dirs) > 0 {
		addGlobal("skills", "dirs", cfg.Skills.Dirs)
	}
	addGlobal("cache", "strategy", cfg.Cache.Strategy)
	addGlobal("usage_warnings", "name", cfg.ManaWarnings.Name)
	if len(cfg.ManaWarnings.Thresholds) > 0 {
		addGlobal("usage_warnings", "thresholds", cfg.ManaWarnings.Thresholds)
	}
	if cfg.Voice.STTEndpoint != "" {
		addGlobal("voice", "stt_endpoint", cfg.Voice.STTEndpoint)
	}
	if cfg.Voice.STTModel != "" {
		addGlobal("voice", "stt_model", cfg.Voice.STTModel)
	}
	if cfg.Voice.TTSProvider != "" {
		addGlobal("voice", "tts_provider", cfg.Voice.TTSProvider)
	}
	if cfg.Voice.TTSEndpoint != "" {
		addGlobal("voice", "tts_endpoint", cfg.Voice.TTSEndpoint)
	}
	if cfg.Voice.TTSModel != "" {
		addGlobal("voice", "tts_model", cfg.Voice.TTSModel)
	}
	if cfg.Voice.TTSVoice != "" {
		addGlobal("voice", "tts_voice", cfg.Voice.TTSVoice)
	}
	if cfg.Voice.TTSRate != 0 {
		addGlobal("voice", "tts_rate", cfg.Voice.TTSRate)
	}
	addGlobal("database", "busy_timeout", cfg.Database.BusyTimeout)
	addGlobal("anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout)
	addGlobal("anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)
	if len(cfg.PromptRules) > 0 {
		addGlobal("prompt_rules", fmt.Sprintf("(%d rules)", len(cfg.PromptRules)), "")
	}

	var tables []string
	tables = append(tables, "```\nGlobal\n"+formatTableBySection(globalRows, mw)+"\n```")

	// Current agent table
	tables = append(tables, "```\nAgent: "+agent.ID+"\n"+formatTableBySection(collectAgentRows(agent), mw)+"\n```")

	return tables
}

// formatTableBySection groups rows by Section and emits a separate table for
// each group, headed by [section]. Each table has only KEY/VALUE columns.
// Section order is preserved from insertion.
func formatTableBySection(rows []configRow, maxWidth ...int) string {
	mw := firstWidth(maxWidth)
	// Collect sections in insertion order.
	var sections []string
	seen := map[string]bool{}
	grouped := map[string][]configRow{}
	for _, r := range rows {
		if !seen[r.Section] {
			seen[r.Section] = true
			sections = append(sections, r.Section)
		}
		grouped[r.Section] = append(grouped[r.Section], r)
	}

	cols := []table.Column{
		{Header: "KEY"},
		{Header: "VALUE"},
	}

	var parts []string
	for _, sec := range sections {
		sRows := grouped[sec]
		tableRows := make([][]string, len(sRows))
		for i, r := range sRows {
			tableRows[i] = []string{r.Key, r.Value}
		}
		parts = append(parts, "["+sec+"]\n"+table.FormatWidth(cols, tableRows, mw))
	}
	return strings.Join(parts, "\n\n")
}

// formatValue converts a config value to its display string.
func formatValue(val interface{}) string {
	switch v := val.(type) {
	case string:
		return v
	case bool:
		return fmt.Sprintf("%v", v)
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case []string:
		return fmt.Sprintf("%v", v)
	case []int:
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// displayConfig is a struct used for TOML marshaling that includes
// only the relevant sections for a given agent.
type displayConfig struct {
	Agent         AgentConfig        `toml:"agent"`
	Telegram      displayTelegram    `toml:"telegram"`
	Sessions      SessionsConfig     `toml:"sessions"`
	Memory        MemoryConfig       `toml:"memory"`
	HTTP          HTTPConfig         `toml:"http"`
	Logging       LoggingConfig      `toml:"logging"`
	Tools         ToolsConfig        `toml:"tools"`
	Environment   EnvironmentConfig  `toml:"environment"`
	Skills        SkillsConfig       `toml:"skills"`
	Cache         CacheConfig        `toml:"cache"`
	UsageWarnings ManaWarningsConfig `toml:"usage_warnings"`
	Voice         VoiceConfig        `toml:"voice"`
	Database      DatabaseConfig     `toml:"database"`
	Anthropic     displayAnthropic   `toml:"anthropic"`
}

type displayTelegram struct {
	AllowedUsers        []string `toml:"allowed_users,omitempty"`
	MultiballBots       []string `toml:"multiball_bots,omitempty"`
	StopAliases         []string `toml:"stop_aliases,omitempty"`
	EnableStopAliases   bool     `toml:"enable_stop_aliases"`
	EnableStartupNotify bool     `toml:"enable_startup_notify"`
	MultiballSessionTTL string   `toml:"multiball_session_ttl"`
	MessageQueueSize    int      `toml:"message_queue_size"`
	LongPollTimeout     string   `toml:"long_poll_timeout"`
	ReceivedFilesDir    string   `toml:"received_files_dir,omitempty"`
}

type displayAnthropic struct {
	HTTPTimeout     string `toml:"http_timeout"`
	UsageAPITimeout string `toml:"usage_api_timeout"`
}

// FormatConfigTOML returns a TOML-marshalable representation of the running
// config for the given agent. Secrets are redacted.
func FormatConfigTOML(cfg *Config, agent AgentConfig) string {
	dc := displayConfig{
		Agent: agent,
		Telegram: displayTelegram{
			AllowedUsers:        cfg.Telegram.AllowedUsers,
			MultiballBots:       cfg.Telegram.MultiballBots,
			StopAliases:         cfg.Telegram.StopAliases,
			EnableStopAliases:   cfg.Telegram.EnableStopAliases,
			EnableStartupNotify: cfg.Telegram.EnableStartupNotify,
			MultiballSessionTTL: cfg.Telegram.MultiballSessionTTL,
			MessageQueueSize:    cfg.Telegram.MessageQueueSize,
			LongPollTimeout:     cfg.Telegram.LongPollTimeout,
			ReceivedFilesDir:    cfg.Telegram.ReceivedFilesDir,
		},
		Sessions:      cfg.Sessions,
		Memory:        cfg.Memory,
		HTTP:          cfg.HTTP,
		Logging:       cfg.Logging,
		Tools:         cfg.Tools,
		Environment:   cfg.Environment,
		Skills:        cfg.Skills,
		Cache:         cfg.Cache,
		UsageWarnings: cfg.ManaWarnings,
		Voice:         cfg.Voice,
		Database:      cfg.Database,
		Anthropic: displayAnthropic{
			HTTPTimeout:     cfg.Anthropic.HTTPTimeout,
			UsageAPITimeout: cfg.Anthropic.UsageAPITimeout,
		},
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(dc); err != nil {
		return fmt.Sprintf("# error marshaling config: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// availableOption describes a config field that is at its zero/default value.
type availableOption struct {
	Section     string
	Key         string
	Default     string
	Description string
}

// FormatAvailable returns a table of config options that are currently unset
// (at zero or default value) for the agent and global sections.
func FormatAvailable(cfg *Config, agent AgentConfig, maxWidth ...int) string {
	mw := firstWidth(maxWidth)
	var opts []availableOption

	// Agent fields
	if len(agent.SystemFiles) == 0 && len(cfg.Defaults.SystemFiles) == 0 {
		opts = append(opts, availableOption{"agent", "system_files", "[]", "workspace file order for system prompt"})
	}
	if agent.BranchOrientationPrompt == "" && cfg.Sessions.BranchOrientationPrompt == "" {
		opts = append(opts, availableOption{"agent", "branch_orientation_prompt", "\"\"", "prompt file injected into all branch sessions"})
	}
	if agent.TelegramBot == "" {
		opts = append(opts, availableOption{"agent", "telegram_bot", "(agent ID)", "bot name; token via \"telegram.<bot>\" secret"})
	}
	if len(agent.MultiballBots) == 0 {
		opts = append(opts, availableOption{"agent", "multiball_bots", "[]", "additional bot names for multiball"})
	}
	if agent.TTSRate == 0 {
		opts = append(opts, availableOption{"agent", "tts_rate", "0", "per-agent TTS speech rate override (0 = use [voice] tts_rate)"})
	}
	// Only show agent override options when the global fallback isn't covering them.
	if agent.StartupNotification == nil && !cfg.Telegram.EnableStartupNotify {
		opts = append(opts, availableOption{"agent", "startup_notification", "(global)", "send startup notification (nil = use global)"})
	}
	if agent.ShowToolCalls == nil && cfg.Defaults.ShowToolCalls != nil && *cfg.Defaults.ShowToolCalls == ToolCallOff {
		opts = append(opts, availableOption{"agent", "show_tool_calls", "(defaults)", "tool call display mode: off, preview, full (nil = use defaults)"})
	}
	if agent.ShowThinking == nil && cfg.Defaults.ShowThinking != nil && *cfg.Defaults.ShowThinking == ShowThinkingOff {
		opts = append(opts, availableOption{"agent", "show_thinking", "(defaults)", "thinking display mode: off, compact, true (nil = use defaults)"})
	}
	if agent.DisplayWidth == nil {
		dw := 44
		if cfg.Defaults.DisplayWidth != nil {
			dw = *cfg.Defaults.DisplayWidth
		}
		opts = append(opts, availableOption{"agent", "display_width", fmt.Sprintf("%d", dw), "display width for dividers (nil = use defaults)"})
	}
	if agent.Effort == "" && cfg.Defaults.Effort == "" {
		opts = append(opts, availableOption{"agent", "effort", "\"\"", "effort level: low, medium, high (empty = omit)"})
	}
	if agent.CompactionEffort == "" {
		opts = append(opts, availableOption{"agent", "compaction_effort", "\"\"", "effort for compaction API calls (empty = use session effort)"})
	}
	if agent.ReceivedFilesDir == "" && cfg.Telegram.ReceivedFilesDir == "" {
		opts = append(opts, availableOption{"agent", "received_files_dir", "\"\"", "save received files to this directory"})
	}
	if len(agent.AllowedUsers) == 0 && len(cfg.Telegram.AllowedUsers) == 0 {
		opts = append(opts, availableOption{"agent", "allowed_users", "(global)", "per-agent allowed Telegram user IDs (empty = use global)"})
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
	if cfg.Sessions.BranchOrientationPrompt == "" {
		opts = append(opts, availableOption{"sessions", "branch_orientation_prompt", "\"\"", "prompt file injected into all branch sessions"})
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
	if cfg.Voice.STTEndpoint == "" {
		opts = append(opts, availableOption{"voice", "stt_endpoint", "(Groq)", "STT API endpoint"})
	}
	if cfg.Voice.STTModel == "" {
		opts = append(opts, availableOption{"voice", "stt_model", "whisper-large-v3", "STT model name"})
	}
	if cfg.Voice.TTSProvider == "" {
		opts = append(opts, availableOption{"voice", "tts_provider", "edge-tts", "TTS provider"})
	}
	if cfg.Voice.TTSVoice == "" {
		opts = append(opts, availableOption{"voice", "tts_voice", "\"\"", "TTS voice name"})
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

	cols := []table.Column{
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
		parts = append(parts, "["+sec+"]\n"+table.FormatWidth(cols, tableRows, mw))
	}
	return "Unset/default config options:\n\n" + strings.Join(parts, "\n\n")
}


