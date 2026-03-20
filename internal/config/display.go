package config

import (
	"bytes"
	"fmt"
	"foci/internal/display"
	"strings"

	"github.com/BurntSushi/toml"
)

// configRow is a single row in the /config table output.
type configRow struct {
	Section string
	Key     string
	Value   string
}

// FormatConfigGrouped returns per-group config tables as markdown pipe tables.
// The first table is "Global" config (all non-agent sections), followed by one
// table for the given agent. Each table is small enough to fit in a single
// Telegram message.
func FormatConfigGrouped(cfg *Config, agent AgentConfig) []string {
	globalRows := collectGlobalConfigRows(cfg)
	annotateGlobalRows(globalRows, cfg, agent)

	var tables []string
	tables = append(tables, "Global\n"+formatTableBySection(globalRows))
	tables = append(tables, "Agent: "+agent.ID+"\n"+formatTableBySection(collectAgentRows(agent)))
	return tables
}

// annotateGlobalRows adds (overridden) and (default) annotations to rows.
// Only defaults/provider fields that can be overridden per-agent get the
// (overridden) tag; all fields get (default) if they weren't explicitly set.
func annotateGlobalRows(rows []configRow, cfg *Config, agent AgentConfig) {
	overrides := map[string]bool{
		"defaults.max_tool_loops":    agent.MaxToolLoops != cfg.Defaults.MaxToolLoops,
		"defaults.max_output_tokens": agent.MaxOutputTokens != cfg.Defaults.MaxOutputTokens,
	}
	for i := range rows {
		path := rows[i].Section + "." + rows[i].Key
		if overrides[path] {
			rows[i].Value += " (overridden)"
		}
		if cfg.DefinedKeys != nil && !cfg.DefinedKeys[path] {
			rows[i].Value += " (default)"
		}
	}
}

// collectGlobalConfigRows returns config rows for all non-agent sections.
func collectGlobalConfigRows(cfg *Config) []configRow {
	var rows []configRow
	add := func(section, key string, val interface{}) {
		rows = append(rows, configRow{section, key, formatValue(val)})
	}

	// groups
	add("groups", "powerful", cfg.Groups.Powerful)
	if cfg.Groups.Fast != "" {
		add("groups", "fast", cfg.Groups.Fast)
	}
	if cfg.Groups.Cheap != "" {
		add("groups", "cheap", cfg.Groups.Cheap)
	}

	// defaults
	add("defaults", "max_output_tokens", cfg.Defaults.MaxOutputTokens)
	add("defaults", "max_tool_loops", cfg.Defaults.MaxToolLoops)
	if cfg.Defaults.DuplicateMessages {
		add("defaults", "duplicate_messages", cfg.Defaults.DuplicateMessages)
	}
	if cfg.Defaults.InjectAgentWarnings.Enabled() {
		add("defaults", "inject_agent_warnings", string(cfg.Defaults.InjectAgentWarnings))
	}
	if cfg.Defaults.InjectChatWarnings.Enabled() {
		add("defaults", "inject_chat_warnings", string(cfg.Defaults.InjectChatWarnings))
	}
	if cfg.Defaults.FacetNoCompact != nil {
		add("defaults", "facet_no_compact", *cfg.Defaults.FacetNoCompact)
	}
	if cfg.Telegram.ShowToolCalls != nil {
		add("telegram", "show_tool_calls", string(*cfg.Telegram.ShowToolCalls))
	}
	if cfg.Telegram.ShowThinking != nil {
		add("telegram", "show_thinking", string(*cfg.Telegram.ShowThinking))
	}
	if cfg.Defaults.InjectedMessageHeader != "" {
		add("defaults", "injected_message_header", cfg.Defaults.InjectedMessageHeader)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		add("defaults", "system_files", cfg.Defaults.SystemFiles)
	}

	// keepalive
	add("keepalive", "enabled", cfg.Keepalive.Enabled)
	add("keepalive", "interval", cfg.Keepalive.Interval)
	add("keepalive", "prompt", cfg.Keepalive.Prompt)

	// background
	add("background", "enabled", cfg.Background.Enabled)
	add("background", "interval", cfg.Background.Interval)
	add("background", "prompt", cfg.Background.Prompt)

	// memory_formation
	add("memory_formation", "interval", cfg.MemoryFormation.Interval)
	add("memory_formation", "consolidation_interval", cfg.MemoryFormation.ConsolidationInterval)
	if cfg.MemoryFormation.IntervalEnabled != nil {
		add("memory_formation", "interval_enabled", *cfg.MemoryFormation.IntervalEnabled)
	}
	if cfg.MemoryFormation.ConsolidationEnabled != nil {
		add("memory_formation", "consolidation_enabled", *cfg.MemoryFormation.ConsolidationEnabled)
	}
	if cfg.MemoryFormation.SessionEndEnabled != nil {
		add("memory_formation", "session_end_enabled", *cfg.MemoryFormation.SessionEndEnabled)
	}

	// telegram
	if len(cfg.Telegram.AllowedUsers) > 0 {
		add("telegram", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.FacetBots) > 0 {
		add("telegram", "facet_bots", cfg.Telegram.FacetBots)
	}
	if len(cfg.Defaults.StopAliases) > 0 {
		add("defaults", "stop_aliases", cfg.Defaults.StopAliases)
	}
	add("defaults", "enable_stop_aliases", cfg.Defaults.EnableStopAliases)
	add("telegram", "startup_notify", cfg.Telegram.StartupNotify)
	add("telegram", "facet_session_ttl", cfg.Telegram.FacetSessionTTL)
	add("telegram", "message_queue_size", cfg.Telegram.MessageQueueSize)
	add("telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	if cfg.Telegram.ReceivedFilesDir != "" {
		add("telegram", "received_files_dir", cfg.Telegram.ReceivedFilesDir)
	}
	if cfg.Telegram.DisplayWidth != nil {
		add("telegram", "display_width", *cfg.Telegram.DisplayWidth)
	}
	if cfg.Telegram.TableWrapLines != nil {
		add("telegram", "table_wrap_lines", *cfg.Telegram.TableWrapLines)
	}
	if cfg.Telegram.TableStyle != nil {
		add("telegram", "table_style", *cfg.Telegram.TableStyle)
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
	add("sessions", "compaction_preserve_messages", cfg.Sessions.CompactionPreserveMessages)
	add("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	add("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.BranchOrientationFacetPrompt != "" {
		add("sessions", "branch_orientation_facet_prompt", cfg.Sessions.BranchOrientationFacetPrompt)
	}
	if cfg.Sessions.BranchOrientationHeadlessPrompt != "" {
		add("sessions", "branch_orientation_headless_prompt", cfg.Sessions.BranchOrientationHeadlessPrompt)
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
	add("tools", "max_summary_input_chars", cfg.Tools.MaxSummaryInputChars)
	add("tools", "max_image_pixels", cfg.Tools.MaxImagePixels)
	add("tools", "auto_summarise", cfg.Tools.AutoSummarise)
	add("tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	add("tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	add("tools", "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	add("tools", "web_search_timeout", cfg.Tools.WebSearchTimeout)
	add("tools", "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	add("tools", "explore_max_depth", cfg.Tools.ExploreMaxDepth)
	add("tools", "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	add("tools", "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	add("tools", "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	add("tools", "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	add("tools", "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)
	add("tools", "tmux_autopilot", cfg.Tools.TmuxAutopilot)
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
	add("cache", "ttl", cfg.Cache.TTL)

	// usage_warnings
	add("usage_warnings", "name", cfg.ManaWarnings.Name)
	if len(cfg.ManaWarnings.Thresholds) > 0 {
		add("usage_warnings", "thresholds", cfg.ManaWarnings.Thresholds)
	}

	// tts
	for i, e := range cfg.TTS {
		prefix := fmt.Sprintf("tts[%d]", i)
		add(prefix, "id", e.ID)
		add(prefix, "format", e.Format)
		if e.Endpoint != "" {
			add(prefix, "endpoint", e.Endpoint)
		}
		if e.Model != "" {
			add(prefix, "model", e.Model)
		}
		if e.Voice != "" {
			add(prefix, "voice", e.Voice)
		}
		if e.Rate != 0 {
			add(prefix, "rate", e.Rate)
		}
	}
	// stt
	for i, e := range cfg.STT {
		prefix := fmt.Sprintf("stt[%d]", i)
		add(prefix, "id", e.ID)
		add(prefix, "format", e.Format)
		if e.Endpoint != "" {
			add(prefix, "endpoint", e.Endpoint)
		}
		if e.Model != "" {
			add(prefix, "model", e.Model)
		}
	}

	// debug
	add("debug", "log_api_key_suffix", cfg.Debug.LogAPIKeySuffix)
	add("debug", "compaction_debug", cfg.Debug.CompactionDebug)

	// database
	add("database", "busy_timeout", cfg.Database.BusyTimeout)

	// anthropic
	add("anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout)
	add("anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)
	add("anthropic", "usage_cache_ttl", cfg.Anthropic.UsageCacheTTL)

	// mana
	add("mana", "invest_interval", cfg.Mana.InvestInterval)

	// message_transforms
	if len(cfg.MessageTransforms) > 0 {
		add("message_transforms", fmt.Sprintf("(%d rules)", len(cfg.MessageTransforms)), "")
	}

	// blocked_paths
	if len(cfg.BlockedPaths) > 0 {
		add("blocked_paths", fmt.Sprintf("(%d paths)", len(cfg.BlockedPaths)), "")
	}

	return rows
}

// collectAgentRows returns the standard set of agent config display rows.
func collectAgentRows(agent AgentConfig) []configRow {
	var rows []configRow
	add := func(key string, val interface{}) {
		rows = append(rows, configRow{"agent", key, formatValue(val)})
	}
	add("id", agent.ID)
	add("workspace", agent.Workspace)

	if len(agent.SystemFiles) > 0 {
		add("system_files", agent.SystemFiles)
	}
	add("duplicate_messages", agent.DuplicateMessages)
	if agent.BranchOrientationFacetPrompt != "" {
		add("branch_orientation_facet_prompt", agent.BranchOrientationFacetPrompt)
	}
	if agent.BranchOrientationHeadlessPrompt != "" {
		add("branch_orientation_headless_prompt", agent.BranchOrientationHeadlessPrompt)
	}
	tg := agent.GetTelegramPlatform()
	if tg != nil && tg.Bot != "" {
		add("platforms.telegram.bot", tg.Bot)
	}
	if tg != nil && len(tg.FacetBots) > 0 {
		add("facet_bots", tg.FacetBots)
	}
	add("max_tool_loops", agent.MaxToolLoops)
	add("max_output_tokens", agent.MaxOutputTokens)
	if agent.Effort != "" {
		add("effort", agent.Effort)
	}
	if agent.TTS != "" {
		add("tts", agent.TTS)
	}
	if agent.STT != "" {
		add("stt", agent.STT)
	}
	if agent.TTSRate != 0 {
		add("tts_rate", agent.TTSRate)
	}
	add("inject_agent_warnings", string(agent.InjectAgentWarnings))
	if agent.InjectChatWarnings.Enabled() {
		add("inject_chat_warnings", string(agent.InjectChatWarnings))
	}
	add("steer_mode", agent.SteerMode)
	if agent.StartupNotify != nil {
		add("startup_notify", *agent.StartupNotify)
	}
	if agent.FacetNoCompact != nil {
		add("facet_no_compact", *agent.FacetNoCompact)
	}
	if agent.ShowToolCalls != nil {
		add("show_tool_calls", string(*agent.ShowToolCalls))
	}
	if agent.ShowThinking != nil {
		add("show_thinking", string(*agent.ShowThinking))
	}
	if tg != nil && tg.DisplayWidth != nil {
		add("platforms.telegram.display_width", *tg.DisplayWidth)
	}
	if tg != nil && tg.TableWrapLines != nil {
		add("platforms.telegram.table_wrap_lines", *tg.TableWrapLines)
	}
	if tg != nil && tg.TableStyle != nil {
		add("platforms.telegram.table_style", *tg.TableStyle)
	}
	if agent.MessagesInLog != nil {
		add("messages_in_log", *agent.MessagesInLog)
	}
	if tg != nil && tg.ReceivedFilesDir != "" {
		add("platforms.telegram.received_files_dir", tg.ReceivedFilesDir)
	}
	if agent.InjectedMessageHeader != "" {
		add("injected_message_header", agent.InjectedMessageHeader)
	}
	if tg != nil && len(tg.AllowedUsers) > 0 {
		add("platforms.telegram.allowed_users", tg.AllowedUsers)
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
	if len(agent.BlockedPaths) > 0 {
		add("blocked_paths", fmt.Sprintf("(%d paths)", len(agent.BlockedPaths)))
	}
	return rows
}

// formatTableBySection groups rows by Section and emits a separate table for
// each group, headed by [section]. Each table has only KEY/VALUE columns.
// Section order is preserved from insertion.
func formatTableBySection(rows []configRow) string {
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

	cols := []display.Column{
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
		parts = append(parts, "["+sec+"]\n"+display.MarkdownTable(cols, tableRows))
	}
	return strings.Join(parts, "\n\n")
}

// formatValue converts a config value to its display string.
func formatValue(val interface{}) string {
	if s, ok := val.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", val)
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
	TTS           []TTSConfig        `toml:"tts"`
	STT           []STTConfig        `toml:"stt"`
	Debug         DebugConfig        `toml:"debug"`
	Database      DatabaseConfig     `toml:"database"`
	Anthropic     displayAnthropic   `toml:"anthropic"`
}

type displayTelegram struct {
	AllowedUsers        []string `toml:"allowed_users,omitempty"`
	FacetBots       []string `toml:"facet_bots,omitempty"`
	StartupNotify       bool     `toml:"startup_notify"`
	FacetSessionTTL string   `toml:"facet_session_ttl"`
	MessageQueueSize    int      `toml:"message_queue_size"`
	LongPollTimeout     string   `toml:"long_poll_timeout"`
	ReceivedFilesDir    string   `toml:"received_files_dir,omitempty"`
	DisplayWidth        *int     `toml:"display_width,omitempty"`
	TableWrapLines      *int     `toml:"table_wrap_lines,omitempty"`
	TableStyle          *string  `toml:"table_style,omitempty"`
}

type displayAnthropic struct {
	HTTPTimeout     string `toml:"http_timeout"`
	UsageAPITimeout string `toml:"usage_api_timeout"`
	UsageCacheTTL   string `toml:"usage_cache_ttl"`
}

// FormatConfigTOML returns a TOML-marshalable representation of the running
// config for the given agent. Secrets are redacted.
func FormatConfigTOML(cfg *Config, agent AgentConfig) string {
	dc := displayConfig{
		Agent: agent,
		Telegram: displayTelegram{
			AllowedUsers:        cfg.Telegram.AllowedUsers,
			FacetBots:       cfg.Telegram.FacetBots,
			StartupNotify:       cfg.Telegram.StartupNotify,
			FacetSessionTTL: cfg.Telegram.FacetSessionTTL,
			MessageQueueSize:    cfg.Telegram.MessageQueueSize,
			LongPollTimeout:     cfg.Telegram.LongPollTimeout,
			ReceivedFilesDir:    cfg.Telegram.ReceivedFilesDir,
			DisplayWidth:        cfg.Telegram.DisplayWidth,
			TableWrapLines:      cfg.Telegram.TableWrapLines,
			TableStyle:          cfg.Telegram.TableStyle,
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
		TTS:           cfg.TTS,
		STT:           cfg.STT,
		Debug:         cfg.Debug,
		Database:      cfg.Database,
		Anthropic: displayAnthropic{
			HTTPTimeout:     cfg.Anthropic.HTTPTimeout,
			UsageAPITimeout: cfg.Anthropic.UsageAPITimeout,
			UsageCacheTTL:   cfg.Anthropic.UsageCacheTTL,
		},
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(dc); err != nil {
		return fmt.Sprintf("# error marshaling config: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}
