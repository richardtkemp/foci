package config

import (
	"bytes"
	"fmt"
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

// FormatConfig returns an aligned columnar table of the running config
// for the given agent. Secrets are redacted.
func FormatConfig(cfg *Config, agent AgentConfig) string {
	var rows []configRow
	add := func(section, key string, val interface{}) {
		rows = append(rows, configRow{section, key, formatValue(val)})
	}

	// agent
	add("agent", "id", agent.ID)
	add("agent", "model", agent.Model)
	add("agent", "workspace", agent.Workspace)

	if len(agent.SystemFiles) > 0 {
		add("agent", "system_files", agent.SystemFiles)
	}
	add("agent", "duplicate_messages", agent.DuplicateMessages)
	if agent.BranchOrientationPrompt != "" {
		add("agent", "branch_orientation_prompt", agent.BranchOrientationPrompt)
	}
	if agent.ForkPrompt != "" {
		add("agent", "fork_prompt (deprecated)", agent.ForkPrompt)
	}
	if agent.TelegramBot != "" {
		add("agent", "telegram_bot", agent.TelegramBot)
	}
	if len(agent.MultiballBots) > 0 {
		add("agent", "multiball_bots", agent.MultiballBots)
	}
	add("agent", "max_tool_loops", agent.MaxToolLoops)
	add("agent", "max_output_tokens", agent.MaxOutputTokens)
	if agent.Effort != "" {
		add("agent", "effort", agent.Effort)
	}
	if agent.TTSRate != 0 {
		add("agent", "tts_rate", agent.TTSRate)
	}
	add("agent", "inject_agent_warnings", agent.InjectAgentWarnings)
	if agent.StartupNotification != nil {
		add("agent", "startup_notification", *agent.StartupNotification)
	}
	if agent.ShowToolCalls != nil {
		add("agent", "show_tool_calls", *agent.ShowToolCalls)
	}
	if agent.ImageSaveDir != "" {
		add("agent", "image_save_dir", agent.ImageSaveDir)
	}
	if len(agent.AllowedUsers) > 0 {
		add("agent", "allowed_users", agent.AllowedUsers)
	}
	if len(agent.UsageWarnings.Thresholds) > 0 {
		add("agent", "usage_warnings.thresholds", agent.UsageWarnings.Thresholds)
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
	if cfg.Defaults.TTSRate != 0 {
		add("defaults", "tts_rate", cfg.Defaults.TTSRate)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		add("defaults", "system_files", cfg.Defaults.SystemFiles)
	}

	// telegram
	add("telegram", "bot_token", redactString(cfg.Telegram.BotToken))
	if len(cfg.Telegram.AllowedUsers) > 0 {
		add("telegram", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.MultiballBots) > 0 {
		add("telegram", "multiball_bots", cfg.Telegram.MultiballBots)
	}
	if len(cfg.Telegram.Bots) > 0 {
		var names []string
		for k := range cfg.Telegram.Bots {
			names = append(names, k)
		}
		add("telegram", "bots", names)
	}
	if len(cfg.Telegram.StopAliases) > 0 {
		add("telegram", "stop_aliases", cfg.Telegram.StopAliases)
	}
	add("telegram", "enable_stop_aliases", cfg.Telegram.EnableStopAliases)
	add("telegram", "enable_startup_notify", cfg.Telegram.EnableStartupNotify)
	add("telegram", "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL)
	add("telegram", "message_queue_size", cfg.Telegram.MessageQueueSize)
	add("telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	add("telegram", "show_tool_calls", cfg.Telegram.ShowToolCalls)
	if cfg.Telegram.ImageSaveDir != "" {
		add("telegram", "image_save_dir", cfg.Telegram.ImageSaveDir)
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
	add("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	add("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.SessionResetPrompt != "" {
		add("sessions", "session_reset_prompt", cfg.Sessions.SessionResetPrompt)
	}
	if cfg.Sessions.BranchOrientationPrompt != "" {
		add("sessions", "branch_orientation_prompt", cfg.Sessions.BranchOrientationPrompt)
	}

	// memory
	if cfg.Memory.Dir != "" {
		add("memory", "dir", cfg.Memory.Dir)
	}
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
	add("logging", "conversation_file", cfg.Logging.ConversationFile)
	add("logging", "full_payload", cfg.Logging.FullPayload)
	if cfg.Logging.PayloadFile != "" {
		add("logging", "payload_file", cfg.Logging.PayloadFile)
	}
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
	add("tools", "exec_max_output_chars", cfg.Tools.ExecMaxOutputChars)
	add("tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	add("tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	add("tools", "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	add("tools", "web_fetch_max_chars", cfg.Tools.WebFetchMaxChars)
	add("tools", "web_search_timeout", cfg.Tools.WebSearchTimeout)
	add("tools", "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	add("tools", "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	add("tools", "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	add("tools", "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	add("tools", "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	add("tools", "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)

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

	// anthropic (secrets redacted)
	add("anthropic", "token", redactString(cfg.Anthropic.Token))
	add("anthropic", "oauth_token", redactString(cfg.Anthropic.OAuthToken))
	add("anthropic", "brave_api_key", redactString(cfg.Anthropic.BraveAPIKey))
	add("anthropic", "credentials_file", cfg.Anthropic.CredentialsFile)
	add("anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout)
	add("anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)

	// prompt_rules
	if len(cfg.PromptRules) > 0 {
		add("prompt_rules", fmt.Sprintf("(%d rules)", len(cfg.PromptRules)), "")
	}

	return formatTable(rows)
}

// FormatConfigGrouped returns per-group config tables, each wrapped in a
// markdown code block. The first table is "Global" config (all non-agent
// sections), followed by one table for the given agent. Each table is small
// enough to fit in a single Telegram message.
func FormatConfigGrouped(cfg *Config, agent AgentConfig) []string {
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
	if cfg.Defaults.TTSRate != 0 {
		addDefault("tts_rate", cfg.Defaults.TTSRate, agent.TTSRate != cfg.Defaults.TTSRate)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		addDefault("system_files", cfg.Defaults.SystemFiles, false)
	}
	addGlobal("telegram", "bot_token", redactString(cfg.Telegram.BotToken))
	if len(cfg.Telegram.AllowedUsers) > 0 {
		addGlobal("telegram", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.MultiballBots) > 0 {
		addGlobal("telegram", "multiball_bots", cfg.Telegram.MultiballBots)
	}
	if len(cfg.Telegram.Bots) > 0 {
		var names []string
		for k := range cfg.Telegram.Bots {
			names = append(names, k)
		}
		addGlobal("telegram", "bots", names)
	}
	if len(cfg.Telegram.StopAliases) > 0 {
		addGlobal("telegram", "stop_aliases", cfg.Telegram.StopAliases)
	}
	addGlobal("telegram", "enable_stop_aliases", cfg.Telegram.EnableStopAliases)
	addGlobal("telegram", "enable_startup_notify", cfg.Telegram.EnableStartupNotify)
	addGlobal("telegram", "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL)
	addGlobal("telegram", "message_queue_size", cfg.Telegram.MessageQueueSize)
	addGlobal("telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	addGlobal("telegram", "show_tool_calls", cfg.Telegram.ShowToolCalls)
	if cfg.Telegram.ImageSaveDir != "" {
		addGlobal("telegram", "image_save_dir", cfg.Telegram.ImageSaveDir)
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
	addGlobal("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	addGlobal("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.SessionResetPrompt != "" {
		addGlobal("sessions", "session_reset_prompt", cfg.Sessions.SessionResetPrompt)
	}
	if cfg.Sessions.BranchOrientationPrompt != "" {
		addGlobal("sessions", "branch_orientation_prompt", cfg.Sessions.BranchOrientationPrompt)
	}
	if cfg.Memory.Dir != "" {
		addGlobal("memory", "dir", cfg.Memory.Dir)
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
	addGlobal("logging", "conversation_file", cfg.Logging.ConversationFile)
	addGlobal("logging", "full_payload", cfg.Logging.FullPayload)
	if cfg.Logging.PayloadFile != "" {
		addGlobal("logging", "payload_file", cfg.Logging.PayloadFile)
	}
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
	addGlobal("tools", "exec_max_output_chars", cfg.Tools.ExecMaxOutputChars)
	addGlobal("tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	addGlobal("tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	addGlobal("tools", "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	addGlobal("tools", "web_fetch_max_chars", cfg.Tools.WebFetchMaxChars)
	addGlobal("tools", "web_search_timeout", cfg.Tools.WebSearchTimeout)
	addGlobal("tools", "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	addGlobal("tools", "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	addGlobal("tools", "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	addGlobal("tools", "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	addGlobal("tools", "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	addGlobal("tools", "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)
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
	addGlobal("anthropic", "token", redactString(cfg.Anthropic.Token))
	addGlobal("anthropic", "oauth_token", redactString(cfg.Anthropic.OAuthToken))
	addGlobal("anthropic", "brave_api_key", redactString(cfg.Anthropic.BraveAPIKey))
	addGlobal("anthropic", "credentials_file", cfg.Anthropic.CredentialsFile)
	addGlobal("anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout)
	addGlobal("anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)
	if len(cfg.PromptRules) > 0 {
		addGlobal("prompt_rules", fmt.Sprintf("(%d rules)", len(cfg.PromptRules)), "")
	}

	var tables []string
	tables = append(tables, "```\nGlobal\n"+formatTable(globalRows)+"\n```")

	// Current agent table
	{
		var agentRows []configRow
		addAgent := func(key string, val interface{}) {
			agentRows = append(agentRows, configRow{"agent", key, formatValue(val)})
		}
		addAgent("id", agent.ID)
		addAgent("model", agent.Model)
		addAgent("workspace", agent.Workspace)

		if len(agent.SystemFiles) > 0 {
			addAgent("system_files", agent.SystemFiles)
		}
		addAgent("duplicate_messages", agent.DuplicateMessages)
		if agent.BranchOrientationPrompt != "" {
			addAgent("branch_orientation_prompt", agent.BranchOrientationPrompt)
		}
		if agent.ForkPrompt != "" {
			addAgent("fork_prompt (deprecated)", agent.ForkPrompt)
		}
		if agent.TelegramBot != "" {
			addAgent("telegram_bot", agent.TelegramBot)
		}
		if len(agent.MultiballBots) > 0 {
			addAgent("multiball_bots", agent.MultiballBots)
		}
		addAgent("max_tool_loops", agent.MaxToolLoops)
		addAgent("max_output_tokens", agent.MaxOutputTokens)
		if agent.Effort != "" {
			addAgent("effort", agent.Effort)
		}
		if agent.TTSRate != 0 {
			addAgent("tts_rate", agent.TTSRate)
		}
		addAgent("inject_agent_warnings", agent.InjectAgentWarnings)
		if agent.StartupNotification != nil {
			addAgent("startup_notification", *agent.StartupNotification)
		}
		if agent.ShowToolCalls != nil {
			addAgent("show_tool_calls", *agent.ShowToolCalls)
		}
		if agent.ImageSaveDir != "" {
			addAgent("image_save_dir", agent.ImageSaveDir)
		}
		if len(agent.AllowedUsers) > 0 {
			addAgent("allowed_users", agent.AllowedUsers)
		}
		if len(agent.UsageWarnings.Thresholds) > 0 {
			addAgent("usage_warnings.thresholds", agent.UsageWarnings.Thresholds)
		}
		tables = append(tables, "```\nAgent: "+agent.ID+"\n"+formatTable(agentRows)+"\n```")
	}

	return tables
}

// formatTable renders config rows as an aligned columnar table.
func formatTable(rows []configRow) string {
	maxSec, maxKey, maxVal := 0, 0, 0
	for _, r := range rows {
		if len(r.Section) > maxSec {
			maxSec = len(r.Section)
		}
		if len(r.Key) > maxKey {
			maxKey = len(r.Key)
		}
		if len(r.Value) > maxVal {
			maxVal = len(r.Value)
		}
	}

	var b strings.Builder
	hdr := fmt.Sprintf("%-*s  %-*s  %s", maxSec, "SECTION", maxKey, "KEY", "VALUE")
	b.WriteString(hdr)
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("─", len(hdr)))
	b.WriteByte('\n')
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %-*s  %s\n", maxSec, r.Section, maxKey, r.Key, r.Value)
	}
	return strings.TrimRight(b.String(), "\n")
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
	ShowToolCalls       bool     `toml:"show_tool_calls"`
	ImageSaveDir        string   `toml:"image_save_dir,omitempty"`
}

type displayAnthropic struct {
	Token           string `toml:"token"`
	OAuthToken      string `toml:"oauth_token"`
	BraveAPIKey     string `toml:"brave_api_key"`
	CredentialsFile string `toml:"credentials_file"`
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
			ShowToolCalls:       cfg.Telegram.ShowToolCalls,
			ImageSaveDir:        cfg.Telegram.ImageSaveDir,
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
			Token:           redactString(cfg.Anthropic.Token),
			OAuthToken:      redactString(cfg.Anthropic.OAuthToken),
			BraveAPIKey:     redactString(cfg.Anthropic.BraveAPIKey),
			CredentialsFile: cfg.Anthropic.CredentialsFile,
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
func FormatAvailable(cfg *Config, agent AgentConfig) string {
	var opts []availableOption

	// Agent fields
	if len(agent.SystemFiles) == 0 && len(cfg.Defaults.SystemFiles) == 0 {
		opts = append(opts, availableOption{"agent", "system_files", "[]", "workspace file order for system prompt"})
	}
	if agent.BranchOrientationPrompt == "" && cfg.Sessions.BranchOrientationPrompt == "" {
		opts = append(opts, availableOption{"agent", "branch_orientation_prompt", "\"\"", "prompt file injected into all branch sessions"})
	}
	if agent.TelegramBot == "" {
		opts = append(opts, availableOption{"agent", "telegram_bot", "\"\"", "references key in [telegram.bots] map"})
	}
	if len(agent.MultiballBots) == 0 {
		opts = append(opts, availableOption{"agent", "multiball_bots", "[]", "references keys in [telegram.bots] map"})
	}
	if agent.TTSRate == 0 && cfg.Defaults.TTSRate == 0 {
		opts = append(opts, availableOption{"agent", "tts_rate", "0", "per-agent TTS speech rate override"})
	}
	// Only show agent override options when the global fallback isn't covering them.
	if agent.StartupNotification == nil && !cfg.Telegram.EnableStartupNotify {
		opts = append(opts, availableOption{"agent", "startup_notification", "(global)", "send startup notification (nil = use global)"})
	}
	if agent.ShowToolCalls == nil && !cfg.Telegram.ShowToolCalls {
		opts = append(opts, availableOption{"agent", "show_tool_calls", "(global)", "show tool calls in Telegram (nil = use global)"})
	}
	if agent.Effort == "" && cfg.Defaults.Effort == "" {
		opts = append(opts, availableOption{"agent", "effort", "\"\"", "effort level: low, medium, high (empty = omit)"})
	}
	if agent.ImageSaveDir == "" && cfg.Telegram.ImageSaveDir == "" {
		opts = append(opts, availableOption{"agent", "image_save_dir", "\"\"", "save received images to this directory"})
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
	if cfg.Sessions.SessionResetPrompt == "" {
		opts = append(opts, availableOption{"sessions", "session_reset_prompt", "\"\"", "prompt file before session clear"})
	}
	if cfg.Sessions.BranchOrientationPrompt == "" {
		opts = append(opts, availableOption{"sessions", "branch_orientation_prompt", "\"\"", "prompt file injected into all branch sessions"})
	}

	// Memory fields
	if cfg.Memory.ReindexDebounce == "" || cfg.Memory.ReindexDebounce == "0s" {
		opts = append(opts, availableOption{"memory", "reindex_debounce", "\"0s\"", "delay before reindex"})
	}

	// Logging fields
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

	// Sort by section, then key within section.
	sort.Slice(opts, func(i, j int) bool {
		if opts[i].Section != opts[j].Section {
			return opts[i].Section < opts[j].Section
		}
		return opts[i].Key < opts[j].Key
	})

	// Find max widths for alignment
	maxSec, maxKey, maxDef := 0, 0, 0
	for _, o := range opts {
		if len(o.Section) > maxSec {
			maxSec = len(o.Section)
		}
		if len(o.Key) > maxKey {
			maxKey = len(o.Key)
		}
		if len(o.Default) > maxDef {
			maxDef = len(o.Default)
		}
	}

	var b strings.Builder
	b.WriteString("Unset/default config options:\n\n")
	hdr := fmt.Sprintf("%-*s  %-*s  %-*s  %s", maxSec, "SECTION", maxKey, "KEY", maxDef, "DEFAULT", "DESCRIPTION")
	b.WriteString(hdr)
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("─", len(hdr)))
	b.WriteByte('\n')
	for _, o := range opts {
		fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n", maxSec, o.Section, maxKey, o.Key, maxDef, o.Default, o.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeField(b *strings.Builder, key string, val interface{}) {
	switch v := val.(type) {
	case string:
		fmt.Fprintf(b, "  %s = %q\n", key, v)
	case bool:
		fmt.Fprintf(b, "  %s = %v\n", key, v)
	case int:
		fmt.Fprintf(b, "  %s = %d\n", key, v)
	case float64:
		fmt.Fprintf(b, "  %s = %g\n", key, v)
	case []string:
		fmt.Fprintf(b, "  %s = %v\n", key, v)
	case []int:
		fmt.Fprintf(b, "  %s = %v\n", key, v)
	default:
		fmt.Fprintf(b, "  %s = %v\n", key, v)
	}
}

func redactString(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}
