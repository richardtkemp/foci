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
		"agent_loop.max_tool_loops":    DerefInt(agent.AgentLoop.MaxToolLoops) != DerefInt(cfg.AgentLoop.MaxToolLoops),
		"agent_loop.max_output_tokens": DerefInt(agent.AgentLoop.MaxOutputTokens) != DerefInt(cfg.AgentLoop.MaxOutputTokens),
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
	add("groups", "powerful", DerefStr(cfg.Groups.Powerful))
	if DerefStr(cfg.Groups.Fast) != "" {
		add("groups", "fast", DerefStr(cfg.Groups.Fast))
	}
	if DerefStr(cfg.Groups.Cheap) != "" {
		add("groups", "cheap", DerefStr(cfg.Groups.Cheap))
	}

	// agent_loop
	if cfg.AgentLoop.MaxOutputTokens != nil {
		add("agent_loop", "max_output_tokens", *cfg.AgentLoop.MaxOutputTokens)
	}
	if cfg.AgentLoop.MaxToolLoops != nil {
		add("agent_loop", "max_tool_loops", *cfg.AgentLoop.MaxToolLoops)
	}
	if cfg.AgentLoop.DuplicateMessages != nil && *cfg.AgentLoop.DuplicateMessages {
		add("agent_loop", "duplicate_messages", true)
	}
	if cfg.Debug.InjectAgentWarnings != nil && cfg.Debug.InjectAgentWarningsLevel().Enabled() {
		add("debug", "inject_agent_warnings", string(*cfg.Debug.InjectAgentWarnings))
	}
	if cfg.Debug.InjectChatWarnings != nil && cfg.Debug.InjectChatWarningsLevel().Enabled() {
		add("debug", "inject_chat_warnings", string(*cfg.Debug.InjectChatWarnings))
	}
	if cfg.Sessions.FacetNoCompact != nil {
		add("sessions", "facet_no_compact", *cfg.Sessions.FacetNoCompact)
	}
	// Platform display settings
	for _, p := range cfg.Platforms {
		if p.ShowToolCalls != nil {
			add("platforms."+p.ID, "show_tool_calls", string(*p.ShowToolCalls))
		}
		if p.ShowThinking != nil {
			add("platforms."+p.ID, "show_thinking", string(*p.ShowThinking))
		}
	}
	if cfg.Display.InjectedMessageHeader != nil && *cfg.Display.InjectedMessageHeader != "" {
		add("display", "injected_message_header", *cfg.Display.InjectedMessageHeader)
	}
	if len(cfg.System.SystemFiles) > 0 {
		add("system", "system_files", cfg.System.SystemFiles)
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

	// platforms
	for _, p := range cfg.Platforms {
		sec := "platforms." + p.ID
		if len(p.AllowedUsers) > 0 {
			add(sec, "allowed_users", p.AllowedUsers)
		}
		if len(p.FacetBots) > 0 {
			add(sec, "facet_bots", p.FacetBots)
		}
		if p.StartupNotify != nil {
			add(sec, "startup_notify", *p.StartupNotify)
		}
		add(sec, "facet_session_ttl", p.FacetSessionTTL)
		add(sec, "message_queue_size", p.MessageQueueSize)
		if p.ReceivedFilesDir != nil && *p.ReceivedFilesDir != "" {
			add(sec, "received_files_dir", *p.ReceivedFilesDir)
		}
		if p.DisplayWidth != nil {
			add(sec, "display_width", *p.DisplayWidth)
		}
		if p.Telegram != nil {
			add(sec, "long_poll_timeout", p.Telegram.LongPollTimeout)
			if p.Telegram.TableWrapLines != nil {
				add(sec, "table_wrap_lines", *p.Telegram.TableWrapLines)
			}
			if p.Telegram.TableStyle != nil {
				add(sec, "table_style", *p.Telegram.TableStyle)
			}
		}
	}
	if len(cfg.Behavior.StopAliases) > 0 {
		add("behavior", "stop_aliases", cfg.Behavior.StopAliases)
	}
	if cfg.Behavior.EnableStopAliases != nil {
		add("behavior", "enable_stop_aliases", *cfg.Behavior.EnableStopAliases)
	}

	// sessions
	add("sessions", "dir", cfg.Sessions.Dir)
	if cfg.Sessions.CompactionThreshold != nil {
		add("sessions", "compaction_threshold", *cfg.Sessions.CompactionThreshold)
	}
	add("sessions", "compaction_max_tokens", cfg.Sessions.CompactionMaxTokens)
	add("sessions", "compaction_min_messages", cfg.Sessions.CompactionMinMessages)
	if cfg.Sessions.CompactionSummaryPrompt != nil {
		add("sessions", "compaction_summary_prompt", *cfg.Sessions.CompactionSummaryPrompt)
	}
	if cfg.Sessions.CompactionHandoffMsg != nil {
		add("sessions", "compaction_handoff_msg", *cfg.Sessions.CompactionHandoffMsg)
	}
	if cfg.Sessions.CompactionPreserveMessages != nil {
		add("sessions", "compaction_preserve_messages", *cfg.Sessions.CompactionPreserveMessages)
	}
	add("sessions", "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	add("sessions", "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.BranchOrientationFacetPrompt != nil {
		add("sessions", "branch_orientation_facet_prompt", *cfg.Sessions.BranchOrientationFacetPrompt)
	}
	if cfg.Sessions.BranchOrientationHeadlessPrompt != nil {
		add("sessions", "branch_orientation_headless_prompt", *cfg.Sessions.BranchOrientationHeadlessPrompt)
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
	add("debug", "messages_in_log", cfg.Debug.MessagesInLog)
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
	if cfg.Skills.Dir != "" {
		add("skills", "dir", cfg.Skills.Dir)
	}

	// cache
	add("cache", "strategy", cfg.Cache.Strategy)
	add("cache", "ttl", cfg.Cache.TTL)

	// mana
	add("mana", "name", cfg.Mana.Name)
	if len(cfg.Mana.Thresholds) > 0 {
		add("mana", "thresholds", cfg.Mana.Thresholds)
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

	if len(agent.System.SystemFiles) > 0 {
		add("system_files", agent.System.SystemFiles)
	}
	add("duplicate_messages", agent.AgentLoop.DuplicateMessages)
	if agent.Sessions.BranchOrientationFacetPrompt != nil {
		add("branch_orientation_facet_prompt", *agent.Sessions.BranchOrientationFacetPrompt)
	}
	if agent.Sessions.BranchOrientationHeadlessPrompt != nil {
		add("branch_orientation_headless_prompt", *agent.Sessions.BranchOrientationHeadlessPrompt)
	}
	for _, p := range agent.Platforms {
		sec := "platforms." + p.ID
		if p.Bot != "" {
			add(sec+".bot", p.Bot)
		}
		if len(p.FacetBots) > 0 {
			add(sec+".facet_bots", p.FacetBots)
		}
	}
	tg := agent.Platform("telegram")
	if agent.AgentLoop.MaxToolLoops != nil {
		add("max_tool_loops", *agent.AgentLoop.MaxToolLoops)
	}
	if agent.AgentLoop.MaxOutputTokens != nil {
		add("max_output_tokens", *agent.AgentLoop.MaxOutputTokens)
	}
	if agent.Voice.TTS != nil {
		add("tts", *agent.Voice.TTS)
	}
	if agent.Voice.STT != nil {
		add("stt", *agent.Voice.STT)
	}
	if agent.Voice.TTSRate != nil {
		add("tts_rate", *agent.Voice.TTSRate)
	}
	if agent.Debug.InjectAgentWarnings != nil {
		add("inject_agent_warnings", string(*agent.Debug.InjectAgentWarnings))
	}
	if agent.Debug.InjectChatWarnings != nil {
		add("inject_chat_warnings", string(*agent.Debug.InjectChatWarnings))
	}
	if agent.Notify.StartupNotify != nil {
		add("startup_notify", *agent.Notify.StartupNotify)
	}
	if agent.Notify.CompactionNotify != nil {
		add("compaction_notify", *agent.Notify.CompactionNotify)
	}
	if agent.Notify.TaskListNotify != nil {
		add("task_list_notify", *agent.Notify.TaskListNotify)
	}
	if agent.Debug.CompactionDebug != nil {
		add("compaction_debug", *agent.Debug.CompactionDebug)
	}
	if agent.Behavior.SteerMode != nil {
		add("steer_mode", *agent.Behavior.SteerMode)
	}
	if agent.Sessions.FacetNoCompact != nil {
		add("facet_no_compact", *agent.Sessions.FacetNoCompact)
	}
	if agent.Display.ShowToolCalls != nil {
		add("show_tool_calls", string(*agent.Display.ShowToolCalls))
	}
	if agent.Display.ShowThinking != nil {
		add("show_thinking", string(*agent.Display.ShowThinking))
	}
	if tg != nil && tg.DisplayWidth != nil {
		add("platforms.telegram.display_width", *tg.DisplayWidth)
	}
	if tg != nil && tg.Telegram != nil && tg.Telegram.TableWrapLines != nil {
		add("platforms.telegram.table_wrap_lines", *tg.Telegram.TableWrapLines)
	}
	if tg != nil && tg.Telegram != nil && tg.Telegram.TableStyle != nil {
		add("platforms.telegram.table_style", *tg.Telegram.TableStyle)
	}
	if agent.Debug.MessagesInLog != nil {
		add("messages_in_log", *agent.Debug.MessagesInLog)
	}
	if tg != nil && tg.ReceivedFilesDir != nil && *tg.ReceivedFilesDir != "" {
		add("platforms.telegram.received_files_dir", *tg.ReceivedFilesDir)
	}
	if agent.Display.InjectedMessageHeader != nil && *agent.Display.InjectedMessageHeader != "" {
		add("injected_message_header", *agent.Display.InjectedMessageHeader)
	}
	if tg != nil && len(tg.AllowedUsers) > 0 {
		add("platforms.telegram.allowed_users", tg.AllowedUsers)
	}
	_ = tg // suppress unused warning if no telegram-specific fields above
	if agent.Sessions.CompactionPreserveMessages != nil {
		add("compaction_preserve_messages", *agent.Sessions.CompactionPreserveMessages)
	}
	if len(agent.Mana.Thresholds) > 0 {
		add("mana.thresholds", agent.Mana.Thresholds)
	}
	if agent.Groups.Powerful != nil {
		add("groups.powerful", *agent.Groups.Powerful)
	}
	if agent.Groups.Fast != nil {
		add("groups.fast", *agent.Groups.Fast)
	}
	if agent.Groups.Cheap != nil {
		add("groups.cheap", *agent.Groups.Cheap)
	}
	if agent.Keepalive.Enabled != nil {
		add("keepalive.enabled", *agent.Keepalive.Enabled)
	}
	if agent.Keepalive.Interval != nil {
		add("keepalive.interval", *agent.Keepalive.Interval)
	}
	if agent.Keepalive.Prompt != nil {
		add("keepalive.prompt", *agent.Keepalive.Prompt)
	}
	if agent.Background.Enabled != nil {
		add("background.enabled", *agent.Background.Enabled)
	}
	if agent.Background.Interval != nil {
		add("background.interval", *agent.Background.Interval)
	}
	if agent.Background.Prompt != nil {
		add("background.prompt", *agent.Background.Prompt)
	}
	if agent.MemoryFormation.Interval != nil {
		add("memory_formation.interval", *agent.MemoryFormation.Interval)
	}
	if agent.MemoryFormation.IntervalEnabled != nil {
		add("memory_formation.interval_enabled", *agent.MemoryFormation.IntervalEnabled)
	}
	if agent.MemoryFormation.IntervalPrompt != nil {
		add("memory_formation.interval_prompt", *agent.MemoryFormation.IntervalPrompt)
	}
	if agent.MemoryFormation.ConsolidationInterval != nil {
		add("memory_formation.consolidation_interval", *agent.MemoryFormation.ConsolidationInterval)
	}
	if agent.MemoryFormation.ConsolidationEnabled != nil {
		add("memory_formation.consolidation_enabled", *agent.MemoryFormation.ConsolidationEnabled)
	}
	if agent.MemoryFormation.ConsolidationPrompt != nil {
		add("memory_formation.consolidation_prompt", *agent.MemoryFormation.ConsolidationPrompt)
	}
	if agent.MemoryFormation.SessionEndEnabled != nil {
		add("memory_formation.session_end_enabled", *agent.MemoryFormation.SessionEndEnabled)
	}
	if agent.MemoryFormation.SessionEndPrompt != nil {
		add("memory_formation.session_end_prompt", *agent.MemoryFormation.SessionEndPrompt)
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
	Platforms     []PlatformConfig   `toml:"platforms"`
	Sessions      SessionsConfig     `toml:"sessions"`
	Memory        MemoryConfig       `toml:"memory"`
	HTTP          HTTPConfig         `toml:"http"`
	Logging       LoggingConfig      `toml:"logging"`
	Tools         ToolsConfig        `toml:"tools"`
	Environment   EnvironmentConfig  `toml:"environment"`
	Skills        SkillsConfig       `toml:"skills"`
	Cache         CacheConfig        `toml:"cache"`
	Mana          ManaConfig         `toml:"mana"`
	TTS           []TTSConfig        `toml:"tts"`
	STT           []STTConfig        `toml:"stt"`
	Debug         DebugConfig        `toml:"debug"`
	Database      DatabaseConfig     `toml:"database"`
	Anthropic     displayAnthropic   `toml:"anthropic"`
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
		Agent:         agent,
		Platforms:     cfg.Platforms,
		Sessions:      cfg.Sessions,
		Memory:        cfg.Memory,
		HTTP:          cfg.HTTP,
		Logging:       cfg.Logging,
		Tools:         cfg.Tools,
		Environment:   cfg.Environment,
		Skills:        cfg.Skills,
		Cache:         cfg.Cache,
		Mana:          cfg.Mana,
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
