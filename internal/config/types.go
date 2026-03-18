package config

import "fmt"

// ToolCallDisplay controls how tool calls are shown in Telegram.
type ToolCallDisplay string

const (
	ToolCallOff     ToolCallDisplay = "off"     // hidden
	ToolCallPreview ToolCallDisplay = "preview" // shown then overwritten by reply
	ToolCallFull    ToolCallDisplay = "full"    // shown and kept; reply is a separate message
)

// UnmarshalTOML accepts both string ("off"/"preview"/"full") and bool (backwards compat).
func (d *ToolCallDisplay) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		switch val {
		case "off", "preview", "full":
			*d = ToolCallDisplay(val)
			return nil
		default:
			return fmt.Errorf("invalid show_tool_calls value %q (must be off, preview, full)", val)
		}
	case bool:
		if val {
			*d = ToolCallPreview
		} else {
			*d = ToolCallOff
		}
		return nil
	default:
		return fmt.Errorf("show_tool_calls must be a string (off/preview/full) or bool")
	}
}

// ShowThinking controls how thinking blocks are displayed in Telegram.
type ShowThinking string

const (
	ShowThinkingOff     ShowThinking = "off"     // thinking stripped, not shown
	ShowThinkingCompact ShowThinking = "compact" // response with "Show thinking" toggle button
	ShowThinkingTrue    ShowThinking = "true"    // thinking prepended to every response
)

// UnmarshalTOML accepts both string ("off"/"compact"/"true") and bool.
func (s *ShowThinking) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		switch val {
		case "off", "compact", "true":
			*s = ShowThinking(val)
			return nil
		default:
			return fmt.Errorf("invalid show_thinking value %q (must be off, compact, true)", val)
		}
	case bool:
		if val {
			*s = ShowThinkingTrue
		} else {
			*s = ShowThinkingOff
		}
		return nil
	default:
		return fmt.Errorf("show_thinking must be a string (off/compact/true) or bool")
	}
}

// AgentUsageWarningsConfig holds per-agent mana warning thresholds.
// When set, completely replaces global [usage_warnings] thresholds.
type AgentUsageWarningsConfig struct {
	Thresholds       []int `toml:"thresholds"`        // mana percentages to warn at (replaces global, not merged)
	RestoreThreshold *int  `toml:"restore_threshold"` // inject session notice when mana restores to 100% (nil=use global, 0=disabled)
}

// AgentMemoryConfig holds per-agent memory sources.
// These are combined with global [memory] sources, with agent-specific
// sources receiving an automatic weight boost.
type AgentMemoryConfig struct {
	Sources []MemorySource `toml:"sources"` // agent-specific memory directories
}

type AgentConfig struct {
	ID        string `toml:"id"`
	Name      string `toml:"name"`     // human-readable name (e.g. "Clutch"); used in voice endpoint agent list
	Emoji     string `toml:"emoji"`    // emoji for agent (e.g. "🥔"); used in voice endpoint agent list
	Model     string `toml:"model"`    // "developer/model_id" format (e.g. "google/gemini-2.5-flash") or alias (e.g. "gemini-flash")
	Endpoint  string `toml:"endpoint"` // optional: which endpoint config to use (auto-selected from developer if empty)
	Workspace string `toml:"workspace"`

	SystemFiles                   []string `toml:"system_files"`                     // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages             bool     `toml:"duplicate_messages"`               // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages bool     `toml:"batch_partial_assistant_messages"` // accumulate mid-turn text; send concatenated on turn end (default: false = send immediately)
	BatchPartialJoiner            string   `toml:"batch_partial_joiner"`             // separator between batched partial messages (default: "")

	BranchOrientationFacetPrompt string `toml:"branch_orientation_facet_prompt"` // path to prompt file for user-attached facet branches
	BranchOrientationHeadlessPrompt  string `toml:"branch_orientation_headless_prompt"`  // path to prompt file for headless branches (cron, spawn, keepalive)

	Memory    AgentMemoryConfig `toml:"memory"`    // per-agent memory sources (combined with global [memory])
	Platforms *PlatformsConfig  `toml:"platforms"` // per-agent platform configurations (telegram, discord, etc.)

	MaxToolLoops          int    `toml:"max_tool_loops"`           // max tool iterations per turn (default 25)
	MaxOutputTokens       int    `toml:"max_output_tokens"`        // max tokens in model response (default 16384)
	BraindeadThreshold    int    `toml:"braindead_threshold"`      // consecutive tool loops before warning (0 = disabled, default 10)
	BraindeadPrompt       string `toml:"braindead_prompt"`         // warning text injected as user message
	TurnLockWarnThreshold string `toml:"turn_lock_warn_threshold"` // warn if turn lock wait exceeds this duration (Go duration, default "3m")
	Effort                string `toml:"effort"`                   // effort level: "low" (default), "medium", "high"
	Thinking              string `toml:"thinking"`                 // thinking mode: "adaptive" (default) or "off"
	Speed                 string `toml:"speed"`                    // speed mode: "standard" (default) or "fast" (Opus only, 6x pricing)
	Streaming             *bool  `toml:"streaming"`                // per-agent streaming override (nil = use global anthropic.streaming)
	FacetNoCompact    *bool  `toml:"facet_no_compact"`     // set no_compact on facet sessions (nil = true)

	TTS              string            `toml:"tts"`               // per-agent TTS provider id (empty = default [[tts]] entry)
	STT              string            `toml:"stt"`               // per-agent STT provider id (empty = default [[stt]] entry)
	TTSRate          float64           `toml:"tts_rate"`          // per-agent TTS speech rate multiplier (0 = use entry rate only)
	TTSReplacements  map[string]string `toml:"tts_replacements"`  // per-agent TTS word replacements (merged with [[tts]] entry replacements)
	STTReplacements  map[string]string `toml:"stt_replacements"`  // per-agent STT word replacements (merged with [[stt]] entry replacements)

	InjectAgentWarnings bool  `toml:"inject_agent_warnings"` // inject warnings/errors into agent session (default false)
	StartupNotify       *bool `toml:"startup_notify"`        // send startup notification (nil = use global telegram.startup_notify)
	ShowToolCalls *ToolCallDisplay `toml:"show_tool_calls"` // show tool call messages (nil = use global/default)
	ShowThinking  *ShowThinking   `toml:"show_thinking"`  // show thinking blocks (nil = use global/default)
	MessagesInLog *bool           `toml:"messages_in_log"` // log user message content to event log (nil = use global logging.messages_in_log)
	// Per-agent compaction overrides (nil/empty = use global [sessions] value)
	CompactionThreshold        *float64 `toml:"compaction_threshold"`         // compact at this % of context window
	CompactionSummaryPrompt    string   `toml:"compaction_summary_prompt"`    // path to summary prompt file
	CompactionHandoffMsg       string   `toml:"compaction_handoff_msg"`       // handoff message after compaction
	CompactionNotify           *bool    `toml:"compaction_notify"`            // send Telegram notification on compaction
	TaskListNotify             *bool    `toml:"task_list_notify"`              // send Telegram notification on task list changes (default true)
	CompactionDebug            *bool    `toml:"compaction_debug"`             // send compaction summary as Telegram file
	CompactionPreserveMessages *int     `toml:"compaction_preserve_messages"` // preserve last N messages through compaction (nil = use global)
	CompactionEffort           string   `toml:"compaction_effort"`            // effort for compaction API calls (empty = use session effort)
	AutocompactBeforeManaRefresh          *bool    `toml:"autocompact_before_mana_refresh"`              // master switch (nil = use global)
	AutocompactBeforeManaRefreshThreshold string   `toml:"autocompact_before_mana_refresh_threshold"`    // trigger mana-refresh compact when reset this soon (empty = use global)
	AutocompactBeforeManaRefreshFactor    *float64 `toml:"autocompact_before_mana_refresh_factor"`       // secondary threshold = main threshold × factor (nil = use global)
	AutocompactBeforeManaRefreshPreserve    *int     `toml:"autocompact_before_mana_refresh_preserve"`     // messages to preserve in refresh mode (nil = use global)
	AutocompactBeforeManaRefreshPreservePct *float64 `toml:"autocompact_before_mana_refresh_preserve_pct"` // fraction of messages to preserve in refresh mode (nil = use global)
	// Per-agent skills and message transforms (empty = use global)
	SkillsDirs        []string           `toml:"skills_dirs"`        // skill directories (empty = use global [skills] dirs)
	MessageTransforms []MessageTransform `toml:"message_transforms"` // regex find/replace rules (empty = use global)
	BlockedPaths      []BlockedPath      `toml:"blocked_paths"`      // path prefixes that write/edit tools refuse (empty = use global)
	// Per-agent tool behaviour (0 = use global [tools] value)
	ExecAutoBackground    int    `toml:"exec_auto_background"`    // seconds before auto-backgrounding exec
	MaxConcurrentSpawns   int    `toml:"max_concurrent_spawns"`   // max concurrent spawn sessions
	ExploreMaxDepth       int    `toml:"explore_max_depth"`       // max tool loops for explore spawn mode (0 = use global)
	MaxUploadFileSize     int64  `toml:"max_upload_file_size"`    // max file size for multipart uploads in bytes
	BrowserEnabled        *bool  `toml:"browser_enabled"`         // per-agent browser tool override (nil = use global tools.browser.enabled)
	TmuxAutopilot         *bool  `toml:"tmux_autopilot"`          // per-agent tmux autopilot override (nil = use global)
	TmuxWatchThreshold    string `toml:"tmux_watch_threshold"`    // per-agent watch threshold (empty = use global)
	TmuxSessionTTL        string `toml:"tmux_session_ttl"`        // per-agent session TTL override (empty = use global)
	MaxResultChars        int    `toml:"max_result_chars"`        // max chars before writing to file (0 = use global)
	MaxSummaryChars       int    `toml:"max_summary_chars"`       // max chars to auto-summarise (0 = use global)
	AutoSummarise         *bool  `toml:"auto_summarise"`          // auto-summarise oversized results (nil = use global)
	SummaryContextTurns   int    `toml:"summary_context_turns"`   // recent turns for auto-summary context (0 = use global)
	SummaryContextChars   int    `toml:"summary_context_chars"`   // max chars of context for auto-summary (0 = use global)
	MaxSummaryInputChars  int    `toml:"max_summary_input_chars"` // max chars embedded in summary prompt (0 = use global)
	MaxImagePixels        int    `toml:"max_image_pixels"`        // max pixels (w*h) before downscaling images (0 = use global)
	SearchProvider        string `toml:"search_provider"`         // "anthropic" or "brave" (empty = use global)
	FetchProvider         string `toml:"fetch_provider"`          // "anthropic" or "builtin" (empty = use global)
	TodoFormat            string `toml:"todo_format"`             // "lines" or "table" (empty = use global, default: lines)
	InjectedMessageHeader string `toml:"injected_message_header"` // header prepended to injected messages (empty = use default)
	// Per-agent keepalive/background (zero = use global [keepalive]/[background])
	Keepalive       KeepaliveConfig       `toml:"keepalive"`        // per-agent keepalive override
	Background      BackgroundConfig      `toml:"background"`       // per-agent background override
	MemoryFormation MemoryFormationConfig `toml:"memory_formation"` // per-agent memory formation override
	// Per-agent usage warning thresholds (nil = use global [usage_warnings])
	UsageWarnings        AgentUsageWarningsConfig `toml:"usage_warnings"`         // per-agent mana warning thresholds
	SteerMode            bool                     `toml:"steer_mode"`             // inject user messages between tool calls (default true)

	// Nudge system: mid-turn behavioral reminders extracted from character files
	NudgeEnable            bool `toml:"nudge_enable"`              // enable the nudge system (default true)
	NudgeAutoExtract       bool `toml:"nudge_auto_extract"`        // auto-extract rules from character files via LLM (default true)
	NudgeCooldown          int  `toml:"nudge_cooldown"`            // min tool calls between repeating same reminder (default 5)
	NudgeMaxPerBatch       int  `toml:"nudge_max_per_batch"`       // max reminders injected per tool batch (default 1)
	NudgePreAnswerGate     bool `toml:"nudge_pre_answer_gate"`     // enable pre-answer verification gate (default false)
	NudgePreAnswerMinTools int  `toml:"nudge_pre_answer_min_tools"` // min tool calls before gate fires (default 2)

	CacheTTL string `toml:"cache_ttl"` // default Anthropic prompt cache TTL: "5m" or "1h" (empty = use [cache] ttl)

	StopAliases       []string `toml:"stop_aliases"`        // per-agent stop aliases (empty = use global)
	EnableStopAliases bool     `toml:"enable_stop_aliases"` // per-agent enable stop aliases (inherited from defaults)

	Webhooks map[string]string `toml:"webhooks"` // webhook hook ID → prompt path (per-agent overrides global entirely)
}

type GeminiConfig struct {
	HTTPTimeout string `toml:"http_timeout"` // HTTP timeout for API calls (default "120s")
	CacheTTL    string `toml:"cache_ttl"`    // context cache TTL (default "1h", "0" disables)
	Thinking    string `toml:"thinking"`     // thinking mode: "adaptive" (default) or "off"
}

type OpenAIConfig struct {
	BaseURL     string `toml:"base_url"`     // API base URL (default: "https://api.openai.com", override for OpenRouter/Together/etc.)
	HTTPTimeout string `toml:"http_timeout"` // HTTP timeout for API calls (default "120s")
}

type AnthropicConfig struct {
	HTTPTimeout               string `toml:"http_timeout"`                 // HTTP timeout for API calls (default "600s")
	UsageAPITimeout           string `toml:"usage_api_timeout"`            // HTTP timeout for usage API calls (default "10s")
	UsageCacheTTL             string `toml:"usage_cache_ttl"`              // cache TTL for usage API responses (default "10m")
	CCExpiryThreshold string `toml:"cc_expiry_threshold"` // how far before expiry to trigger proactive token refresh (default "5m")
	UseSDK                    bool   `toml:"use_sdk"`                      // use SDK transport (default true; false = raw HTTP)
	Streaming                 bool   `toml:"streaming"`                    // use streaming API (default false; requires use_sdk)
	Effort                    string `toml:"effort"`                       // effort level: "low" (default), "medium", "high"
	Thinking                  string `toml:"thinking"`                     // thinking mode: "adaptive" (default) or "off"
	Speed                     string `toml:"speed"`                        // speed mode: "standard" (default) or "fast" (Opus only, 6x pricing)
}

type DiscordConfig struct {
	AllowedUsers         []string         `toml:"allowed_users"`          // Discord user ID snowflakes
	GuildID              string           `toml:"guild_id"`               // optional restriction to a single guild
	RequireMention       bool             `toml:"require_mention"`        // require @mention in guild channels (default true)
	AutoThread           bool             `toml:"auto_thread"`            // create threads for facet sessions (default true)
	StartupNotify        bool             `toml:"startup_notify"`         // send notification on startup (default true)
	FacetSessionTTL      string           `toml:"facet_session_ttl"`      // idle TTL before a facet thread can be reclaimed (default "60m")
	MessageQueueSize     int              `toml:"message_queue_size"`     // inbound message queue buffer size (default 64)
	ReceivedFilesDir     string           `toml:"received_files_dir"`     // save received files to this directory (empty = disabled)
	ShowToolCalls        *ToolCallDisplay `toml:"show_tool_calls"`        // default show_tool_calls (default: "off")
	ShowThinking         *ShowThinking    `toml:"show_thinking"`          // default show_thinking (default: "off")
	StreamOutput         bool             `toml:"stream_output"`          // default stream_output (default: false)
	StreamUpdateInterval string           `toml:"stream_update_interval"` // default stream_update_interval (default: "1200ms")
	DisplayWidth         *int             `toml:"display_width"`          // display width for dividers (default 60)
}

type TelegramConfig struct {
	AllowedUsers        []string `toml:"allowed_users"`
	FacetBots       []string `toml:"facet_bots"`        // shared facet pool: bot names (tokens via "telegram.<name>" secrets)
	StartupNotify       bool     `toml:"startup_notify"`        // send notification on startup (default true)
	FacetSessionTTL string   `toml:"facet_session_ttl"` // idle TTL before a facet bot can be reclaimed (default "60m", "0" disables)
	MessageQueueSize    int      `toml:"message_queue_size"`    // outbound message queue buffer size (default 64)
	LongPollTimeout     string   `toml:"long_poll_timeout"`     // long-poll timeout for getUpdates (default "65s")
	ReceivedFilesDir    string   `toml:"received_files_dir"`    // save received files to this directory (empty = disabled, per-agent overrides)
	ShowToolCalls       *ToolCallDisplay `toml:"show_tool_calls"` // default show_tool_calls (default: "off")
	ShowThinking        *ShowThinking    `toml:"show_thinking"`   // default show_thinking (default: "off")
	StreamOutput         bool    `toml:"stream_output"`          // default stream_output (default: false)
	StreamUpdateInterval string  `toml:"stream_update_interval"` // default stream_update_interval (default: "250ms")
	DisplayWidth        *int     `toml:"display_width"`         // display width for dividers (default 44)
	TableWrapLines      *int     `toml:"table_wrap_lines"`      // max wrapped lines per table cell (default 5)
	TableStyle          *string  `toml:"table_style"`           // table style: "pretty" (default) or "markdown"
}

// TelegramPlatformConfig holds per-agent Telegram platform settings.
type TelegramPlatformConfig struct {
	Bot              string           `toml:"bot"`                // bot name; token resolved via "telegram.<bot>" secret
	BotSecret        string           `toml:"bot_secret"`         // override secret key for bot token (default: "telegram.<bot>")
	FacetBots    []string         `toml:"facet_bots"`     // additional bot names for facet (optional)
	AllowedUsers     []string         `toml:"allowed_users"`      // per-agent allowed Telegram user IDs (empty = use global)
	ShowToolCalls    *ToolCallDisplay `toml:"show_tool_calls"`    // show tool call messages (nil = use global/default)
	ShowThinking     *ShowThinking    `toml:"show_thinking"`      // show thinking blocks (nil = use global/default)
	DisplayWidth     *int             `toml:"display_width"`      // display width for dividers (nil = use global)
	TableWrapLines   *int             `toml:"table_wrap_lines"`   // max wrapped lines per table cell (nil = use global)
	TableStyle       *string          `toml:"table_style"`        // table style: "pretty" or "markdown" (nil = use global)
	ReceivedFilesDir string           `toml:"received_files_dir"` // save received files to this directory (empty = disabled)
	StreamOutput     *bool            `toml:"stream_output"`      // stream model output to Telegram in real-time (nil = use default)
	StreamInterval   string           `toml:"stream_interval"`    // duration between Telegram message edits during streaming
}

func (t *TelegramPlatformConfig) getShowToolCalls() *ToolCallDisplay { return t.ShowToolCalls }
func (t *TelegramPlatformConfig) setShowToolCalls(v *ToolCallDisplay) { t.ShowToolCalls = v }
func (t *TelegramPlatformConfig) getShowThinking() *ShowThinking      { return t.ShowThinking }
func (t *TelegramPlatformConfig) setShowThinking(v *ShowThinking)     { t.ShowThinking = v }

// DiscordPlatformConfig holds per-agent Discord platform settings.
type DiscordPlatformConfig struct {
	Bot            string           `toml:"bot"`              // bot name; token resolved via "discord.<bot>" secret
	BotSecret      string           `toml:"bot_secret"`       // override secret key for bot token (default: "discord.<bot>")
	AllowedUsers   []string         `toml:"allowed_users"`    // per-agent allowed Discord user IDs (empty = use global)
	GuildID        string           `toml:"guild_id"`         // restrict to this guild (empty = all guilds)
	ShowToolCalls  *ToolCallDisplay `toml:"show_tool_calls"`  // show tool call messages (nil = use global/default)
	ShowThinking   *ShowThinking    `toml:"show_thinking"`    // show thinking blocks (nil = use global/default)
	DisplayWidth   *int             `toml:"display_width"`    // display width for dividers (nil = use global)
	StreamOutput   *bool            `toml:"stream_output"`    // stream model output in real-time (nil = use default)
	StreamInterval string           `toml:"stream_interval"`  // duration between Discord message edits during streaming
	RequireMention *bool            `toml:"require_mention"`  // require @mention in guild channels (nil = use global, default true)
	AutoThread     *bool            `toml:"auto_thread"`      // create threads for facet sessions (nil = use global, default true)
	ReceivedFilesDir string         `toml:"received_files_dir"` // save received files to this directory (empty = disabled)
}

func (d *DiscordPlatformConfig) getShowToolCalls() *ToolCallDisplay { return d.ShowToolCalls }
func (d *DiscordPlatformConfig) setShowToolCalls(v *ToolCallDisplay) { d.ShowToolCalls = v }
func (d *DiscordPlatformConfig) getShowThinking() *ShowThinking      { return d.ShowThinking }
func (d *DiscordPlatformConfig) setShowThinking(v *ShowThinking)     { d.ShowThinking = v }

// PlatformsConfig holds per-agent platform configurations.
// Each platform (telegram, discord, etc.) has its own config section.
type PlatformsConfig struct {
	Telegram *TelegramPlatformConfig `toml:"telegram"`
	Discord  *DiscordPlatformConfig  `toml:"discord"`
}

// GetTelegramPlatform returns the Telegram platform config for this agent, or nil.
func (a *AgentConfig) GetTelegramPlatform() *TelegramPlatformConfig {
	if a.Platforms == nil {
		return nil
	}
	return a.Platforms.Telegram
}

// GetDiscordPlatform returns the Discord platform config for this agent, or nil.
func (a *AgentConfig) GetDiscordPlatform() *DiscordPlatformConfig {
	if a.Platforms == nil {
		return nil
	}
	return a.Platforms.Discord
}

type SessionsConfig struct {
	Dir string `toml:"dir"`

	CompactionThreshold        float64 `toml:"compaction_threshold"`          // compact at this % of context window (default 0.8)
	CompactionMaxTokens        int     `toml:"compaction_max_tokens"`         // max output tokens for summary (default 4096)
	CompactionMinMessages      int     `toml:"compaction_min_messages"`       // min messages before compacting (default 4)
	CompactionSummaryPrompt    string  `toml:"compaction_summary_prompt"`     // path to summary prompt file
	CompactionHandoffMsg       string  `toml:"compaction_handoff_msg"`        // handoff message after compaction
	CompactionNotify           *bool   `toml:"compaction_notify"`             // send Telegram notification on compaction (default true)
	MaxSystemPromptFile        int     `toml:"max_system_prompt_chars_file"`  // per-file char threshold for warnings (default 20000)
	MaxSystemPromptTotal       int     `toml:"max_system_prompt_chars_total"` // total system prompt char threshold (default 80000)
	CompactionPreserveMessages int     `toml:"compaction_preserve_messages"`  // preserve last N messages through compaction (default 25, 0 disables)

	AutocompactBeforeManaRefresh          bool     `toml:"autocompact_before_mana_refresh"`              // master switch (default true)
	AutocompactBeforeManaRefreshThreshold string   `toml:"autocompact_before_mana_refresh_threshold"`    // trigger mana-refresh compact when reset this soon (default "5m")
	AutocompactBeforeManaRefreshFactor    float64  `toml:"autocompact_before_mana_refresh_factor"`       // secondary threshold = main threshold × factor (default 0.5)
	AutocompactBeforeManaRefreshPreserve    *int     `toml:"autocompact_before_mana_refresh_preserve"`     // messages to preserve in refresh mode (nil = use percentage)
	AutocompactBeforeManaRefreshPreservePct *float64 `toml:"autocompact_before_mana_refresh_preserve_pct"` // fraction of messages to preserve in refresh mode (default 0.5)

	BranchOrientationFacetPrompt string `toml:"branch_orientation_facet_prompt"` // path to prompt file for user-attached facet branches
	BranchOrientationHeadlessPrompt  string `toml:"branch_orientation_headless_prompt"`  // path to prompt file for headless branches (cron, spawn, keepalive)

	ArchiveAfter string `toml:"archive_after"` // gzip idle sessions after this duration (default "24h")
}

type MemorySource struct {
	Name   string  `toml:"name"`   // unique identifier (e.g., "canonical", "code", "docs")
	Dir    string  `toml:"dir"`    // directory path to index
	Weight float64 `toml:"weight"` // weight multiplier: 0.0-1.0 (1.0 = highest priority)
}

type MemoryConfig struct {
	Sources            []MemorySource `toml:"sources"`
	SearchBackends     []string       `toml:"search_backends"`     // active search backends: "fts5", "bleve" (default ["bleve"])
	ReindexDebounce    string         `toml:"reindex_debounce"`    // delay before reindex (e.g., "500ms", "2s"), default "0s"
	ConversationWeight float64        `toml:"conversation_weight"` // weight multiplier for conversation search results (default 0.1)
	SearchLimit        int            `toml:"search_limit"`        // max search results to return (default 20)
	SweepInterval      string         `toml:"sweep_interval"`      // periodic full reindex interval (default "1h", "0" disables)
}

// HasBackend reports whether the given search backend is enabled.
func (m MemoryConfig) HasBackend(name string) bool {
	for _, b := range m.SearchBackends {
		if b == name {
			return true
		}
	}
	return false
}

type DatabaseConfig struct {
	BusyTimeout string `toml:"busy_timeout"` // SQLite busy timeout for concurrent access (default "5s")
}

type HTTPConfig struct {
	Port                    int    `toml:"port"`
	Bind                    string `toml:"bind"`
	GracefulShutdownTimeout string `toml:"graceful_shutdown_timeout"` // time to wait for in-flight requests on shutdown (default "30s")
	WSEnabled               bool   `toml:"ws_enabled"`                // enable /voice WebSocket endpoint (default false)
}

type LoggingConfig struct {
	Level            string `toml:"level"`
	EventFile        string `toml:"event_file"`
	APIFile          string `toml:"api_file"`
	APIDB            string `toml:"api_db"` // SQLite API call log path (empty = disabled, default: {data_dir}/api.db)
	ConversationFile string `toml:"conversation_file"`

	FullPayload          bool   `toml:"full_payload"`            // write full API payloads to api-payload.jsonl
	PayloadFile          string `toml:"payload_file"`            // path to api-payload.jsonl (default: api-payload.jsonl)
	CacheBustDetect      bool   `toml:"cache_bust_detect"`       // alert when cache_read drops >50% vs previous request
	CacheBustIdleMinutes int    `toml:"cache_bust_idle_minutes"` // suppress cache bust alert if session idle > N minutes (default 10)

	WarningMaxPerWindow               int    `toml:"warning_max_per_window"`               // max identical warnings per window before suppression (default 3)
	WarningWindowDuration             string `toml:"warning_window_duration"`              // time window for warning dedup (default "5m")
	WarningProactiveActiveInterval    string `toml:"warning_proactive_active_interval"`    // min interval between proactive warning turns when user is active (default "5m")
	WarningProactiveInactiveInterval  string `toml:"warning_proactive_inactive_interval"`  // min interval when user is inactive (default "1h")
	WarningProactiveActivityThreshold string `toml:"warning_proactive_activity_threshold"` // user is "active" if last message within this window (default "10m")

	LogRotation         bool   `toml:"log_rotation"`           // enable built-in log rotation (default true)
	RotationPeriod      string `toml:"rotation_period"`        // how often to rotate (default "24h")
	RetentionPeriod     string `toml:"retention_period"`       // keep lines newer than this (default "48h")
	RotationMaxLineSize string `toml:"rotation_max_line_size"` // max line size for scanner buffer (default "64MB")
	ArchiveDir          string `toml:"archive_dir"`            // gzip archive directory (default: log_dir/archive/)
	LogFileMode         string `toml:"log_file_mode"`          // octal file permissions for log files (default "0600")

	MessagesInLog bool `toml:"messages_in_log"` // log user message content to event log (default false for privacy)
}

// TTSConfig describes a text-to-speech provider entry.
// Multiple entries are supported via [[tts]]; first entry is the default.
type TTSConfig struct {
	ID             string  `toml:"id"`              // lookup key for agent overrides
	Format         string  `toml:"format"`          // "openai" or "edge-tts"
	Endpoint       string  `toml:"endpoint"`        // API URL (ignored for edge-tts)
	Model          string  `toml:"model"`           // model name (ignored for edge-tts)
	Voice          string  `toml:"voice"`           // voice name (format-specific)
	Rate           float64 `toml:"rate"`            // speed multiplier: 1.0 = normal, 0 = omit
	Secret         string  `toml:"secret"`          // secret name in secrets.toml (optional, fallback: hostname)
	Command        string            `toml:"command"`         // binary for edge-tts (default: "edge-tts")
	ResponseFormat string            `toml:"response_format"` // audio format: "mp3", "wav", etc. (default: "wav")
	Replacements   map[string]string `toml:"replacements"`    // word replacements applied before synthesis (e.g. "foci" = "foki")
}

// STTConfig describes a speech-to-text provider entry.
// Multiple entries are supported via [[stt]]; first entry is the default.
type STTConfig struct {
	ID       string `toml:"id"`       // lookup key for agent overrides
	Format   string `toml:"format"`   // "openai" (only supported format currently)
	Endpoint string `toml:"endpoint"` // API URL
	Model    string `toml:"model"`    // model name
	Secret       string            `toml:"secret"`       // secret name in secrets.toml (optional, fallback: hostname)
	Replacements map[string]string `toml:"replacements"` // word replacements applied after transcription (e.g. "foki" = "foci")
}

type BitwardenConfig struct {
	Enabled         bool   `toml:"enabled"`
	RefreshInterval string `toml:"refresh_interval"` // how often to refresh item metadata (default "15m")
	SecretTTL       string `toml:"secret_ttl"`       // how long unlocked values stay cached (default "30m")
	SessionFile     string `toml:"session_file"`     // path to BW session file read by bitwarden user (default "/home/bitwarden/.bw_session")
	CleanupInterval string `toml:"cleanup_interval"` // how often to purge expired values (default "1m")
}

type CacheConfig struct {
	Strategy string `toml:"strategy"` // "auto" (top-level, default) or "explicit" (manual breakpoints)
	TTL      string `toml:"ttl"`      // Anthropic prompt cache TTL: "5m" or "1h" (default "1h")
}

type ManaWarningsConfig struct {
	Name             string `toml:"name"`              // what to call quota (default "mana")
	Thresholds       []int  `toml:"thresholds"`        // mana percentages to warn at (e.g. [50, 25, 10, 5])
	RestoreThreshold int    `toml:"restore_threshold"` // inject session notice when mana restores to 100% after being below this (0=disabled)
}

type EnvironmentConfig struct {
	Enabled  bool   `toml:"enabled"`   // inject environment block as first system block (default true)
	DocsPath string `toml:"docs_path"` // path to platform docs directory; relative paths resolve against $HOME
}

type SkillsConfig struct {
	Dirs []string `toml:"dirs"` // directories to scan for skill subdirectories
}

type ResourcesConfig struct {
	MemoryGuardEnabled      bool    `toml:"memory_guard_enabled"`      // enable system memory guard (default true)
	MemoryGuardInterval     string  `toml:"memory_guard_interval"`     // check interval (default "60s")
	MemoryWarnPercent       int     `toml:"memory_warn_percent"`       // warn threshold as % of total RAM (default 25)
	MemoryKillPercent       int     `toml:"memory_kill_percent"`       // kill threshold as % of total RAM (default 40)
	MemoryPressureThreshold float64 `toml:"memory_pressure_threshold"` // PSI avg10 threshold to require before acting (default 10.0)
	GoroutineMonitorInterval  string `toml:"goroutine_monitor_interval"`  // goroutine count check interval (default "60s")
	GoroutineMonitorThreshold int    `toml:"goroutine_monitor_threshold"` // warn when goroutine count exceeds this (0 = auto: 35 × agent count)
}

// BrowserConfig holds configuration for the browser automation tool.
type BrowserConfig struct {
	Enabled        bool    `toml:"enabled"`          // enable browser tool (default true)
	Headless       bool    `toml:"headless"`          // run headless (default true)
	TimeoutSec     int     `toml:"timeout_sec"`       // page operation timeout in seconds (default 30)
	UserDataDir    string  `toml:"user_data_dir"`     // Chrome user data dir (empty = temp profile)
	ExecutablePath string  `toml:"executable_path"`   // Chrome executable path (empty = auto-detect)
	Incognito      bool    `toml:"incognito"`          // use incognito mode (default true)
	DOMStableSec   float64 `toml:"dom_stable_sec"`    // DOM stability wait interval in seconds (default 1)
	DOMStableDiff  float64 `toml:"dom_stable_diff"`   // DOM stability diff threshold (default 0.2)
}

type ToolsConfig struct {
	MaxResultChars          int      `toml:"max_result_chars"`           // max chars before writing result to file (default 15000)
	TempDir                 string   `toml:"temp_dir"`                   // where to write large tool results (default /tmp/foci/tool-results)
	TmuxCols                int      `toml:"tmux_cols"`                  // tmux window columns on start (default 300)
	TmuxRows                int      `toml:"tmux_rows"`                  // tmux window rows on start (default 30)
	ExecAutoBackground      int      `toml:"exec_auto_background"`       // seconds before auto-backgrounding exec (default 10, 0 disables)
	ExecDefaultTimeout      int      `toml:"exec_default_timeout"`       // default timeout for exec commands in seconds (default 30)
	MaxSummaryChars         int      `toml:"max_summary_chars"`          // max chars to auto-summarise (default 300000; larger results skip cheap model)
	AutoSummarise           bool     `toml:"auto_summarise"`             // auto-summarise oversized results via cheap model (default true)
	TmuxCommandTimeout      string   `toml:"tmux_command_timeout"`       // timeout for tmux control commands (default "5s")
	WebFetchTimeout         string   `toml:"web_fetch_timeout"`          // HTTP timeout for web fetch (default "30s")
	WebFetchMaxBytes        int      `toml:"web_fetch_max_bytes"`        // max bytes to read from web fetch (default 1048576 = 1MB)
	WebSearchTimeout        string   `toml:"web_search_timeout"`         // HTTP timeout for web search (default "15s")
	MaxConcurrentSpawns     int      `toml:"max_concurrent_spawns"`      // max concurrent spawn inherit sessions per agent (default 3)
	ExploreMaxDepth         int      `toml:"explore_max_depth"`          // max tool loops for explore spawn mode (default 100)
	ToolCallPreviewChars    int      `toml:"tool_call_preview_chars"`    // max chars for tool call param preview in Telegram (default 450)
	TmuxMemoryCheckInterval string   `toml:"tmux_memory_check_interval"` // how often to check tmux RSS (default "5m", "0" disables)
	TmuxMemoryWarn          string   `toml:"tmux_memory_warn"`           // warn threshold as % of RAM or absolute (default "10%")
	TmuxMemoryCritical      string   `toml:"tmux_memory_critical"`       // critical threshold (default "20%")
	TmuxMemoryKill          string   `toml:"tmux_memory_kill"`           // kill threshold (default "30%")
	TmuxAutopilot           bool     `toml:"tmux_autopilot"`             // auto-unwatch on inactivity, auto-watch on send (default true)
	TmuxWatchThreshold      string   `toml:"tmux_watch_threshold"`       // default watch threshold duration (default "30s")
	TmuxSessionTTL          string   `toml:"tmux_session_ttl"`           // auto-kill idle tmux sessions after this duration (default "24h", "0" disables)
	MaxUploadFileSize       int64    `toml:"max_upload_file_size"`       // max file size for multipart uploads in bytes (default 52428800 = 50MB)
	SummaryContextTurns     int      `toml:"summary_context_turns"`      // recent turns for auto-summary context (default 5)
	SummaryContextChars     int      `toml:"summary_context_chars"`      // max chars of context for auto-summary (default 6000)
	MaxSummaryInputChars    int      `toml:"max_summary_input_chars"`    // max chars of tool result embedded in summary prompt (default 100000)
	MaxImagePixels          int      `toml:"max_image_pixels"`           // max pixels (w*h) before downscaling images (default 2073600)
	SearchProvider          string   `toml:"search_provider"`            // "brave" (default) or "anthropic"
	FetchProvider           string   `toml:"fetch_provider"`             // "anthropic" (default) or "builtin"
	WebSearchMaxUses        int      `toml:"web_search_max_uses"`        // max searches per API call (0 = unlimited)
	WebSearchAllowedDomains []string `toml:"web_search_allowed_domains"` // domain whitelist (mutually exclusive with blocked)
	WebSearchBlockedDomains []string `toml:"web_search_blocked_domains"` // domain blacklist
	WebFetchMaxUses         int      `toml:"web_fetch_max_uses"`         // max fetches per API call (0 = unlimited)
	WebFetchAllowedDomains  []string      `toml:"web_fetch_allowed_domains"`  // domain whitelist
	WebFetchBlockedDomains  []string      `toml:"web_fetch_blocked_domains"`  // domain blacklist
	Browser                 BrowserConfig `toml:"browser"`                    // browser automation tool config
}

type MessageTransform struct {
	Find    string `toml:"find"`    // regex pattern to match
	Replace string `toml:"replace"` // replacement string (supports $1, $2, etc.)
}

type BlockedPath struct {
	Path   string `toml:"path"`   // directory or file prefix to block
	Rebuke string `toml:"rebuke"` // message returned when write/edit is attempted
}

type CommandConfig struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Script      string `toml:"script"`
	Timeout     int    `toml:"timeout"` // seconds, default 10
}

// DefaultsConfig provides global defaults for agent-specific fields.
// Agents inherit these unless they override them explicitly.
// LLMConfig holds LLM-specific settings that apply globally.
// Per-agent overrides use the matching fields on AgentConfig.
type LLMConfig struct {
	Model           string `toml:"model"`            // default model: "developer/model_id" or alias (default: "anthropic/claude-haiku-4-5-20251001")
	MaxOutputTokens int    `toml:"max_output_tokens"` // default max_output_tokens (default: 16384)
}

type DefaultsConfig struct {
	DuplicateMessages             bool   `toml:"duplicate_messages"`               // default duplicate_messages (default: false)
	BatchPartialAssistantMessages bool   `toml:"batch_partial_assistant_messages"` // default batch_partial_assistant_messages (default: false)
	BatchPartialJoiner            string `toml:"batch_partial_joiner"`             // default separator between batched partial messages (default: "")

	InjectAgentWarnings   bool   `toml:"inject_agent_warnings"`    // default inject_agent_warnings (default: false)
	MaxToolLoops          int    `toml:"max_tool_loops"`           // default max_tool_loops (default: 25)
	BraindeadThreshold    int    `toml:"braindead_threshold"`      // default braindead threshold (default: 10)
	BraindeadPrompt       string `toml:"braindead_prompt"`         // default braindead prompt
	TurnLockWarnThreshold string `toml:"turn_lock_warn_threshold"` // default turn lock warn threshold (default: "3m")

	Streaming   *bool    `toml:"streaming"`    // default streaming (nil = use global anthropic.streaming)
	SystemFiles []string `toml:"system_files"` // default system file list
	CompactionEffort string           `toml:"compaction_effort"` // default compaction effort (empty = use session effort)

	MaxResultChars       int   `toml:"max_result_chars"`        // default max_result_chars (default 15000)
	MaxSummaryChars      int   `toml:"max_summary_chars"`       // default max_summary_chars (default 300000)
	AutoSummarise        *bool `toml:"auto_summarise"`          // default auto_summarise (nil = use [tools] value)
	SummaryContextTurns  int   `toml:"summary_context_turns"`   // default summary_context_turns (default 5)
	SummaryContextChars  int   `toml:"summary_context_chars"`   // default summary_context_chars (default 6000)
	MaxSummaryInputChars int   `toml:"max_summary_input_chars"` // default max_summary_input_chars (default 100000)
	MaxImagePixels       int   `toml:"max_image_pixels"`        // default max_image_pixels (default 2073600 = 1920*1080)

	SearchProvider        string `toml:"search_provider"`         // default search provider: "brave" (default) or "anthropic"
	FetchProvider         string `toml:"fetch_provider"`          // default fetch provider: "anthropic" (default) or "builtin"
	TodoFormat            string `toml:"todo_format"`             // default todo list format: "lines" (default) or "table"
	InjectedMessageHeader string `toml:"injected_message_header"` // header prepended to injected (system) messages in Telegram (default: "[[ System message ]]", empty disables)

	TTS                  string            `toml:"tts"`                    // default TTS provider id
	STT                  string            `toml:"stt"`                    // default STT provider id
	TTSRate              float64           `toml:"tts_rate"`               // default TTS speech rate multiplier
	TTSReplacements      map[string]string `toml:"tts_replacements"`       // default TTS word replacements (merged with [[tts]] entry replacements)
	STTReplacements      map[string]string `toml:"stt_replacements"`       // default STT word replacements (merged with [[stt]] entry replacements)
	SteerMode           bool   `toml:"steer_mode"`            // default steer_mode (default: true)
	FacetNoCompact   *bool  `toml:"facet_no_compact"`   // set no_compact on facet sessions (nil = true)
	CacheTTL             string `toml:"cache_ttl"`              // default Anthropic prompt cache TTL: "5m" or "1h" (empty = use [cache] ttl)

	// Nudge system: mid-turn behavioral reminders extracted from character files
	NudgeEnable            bool `toml:"nudge_enable"`              // enable the nudge system (default true)
	NudgeAutoExtract       bool `toml:"nudge_auto_extract"`        // auto-extract rules from character files via LLM (default true)
	NudgeCooldown          int  `toml:"nudge_cooldown"`            // min tool calls between repeating same reminder (default 5)
	NudgeMaxPerBatch       int  `toml:"nudge_max_per_batch"`       // max reminders injected per tool batch (default 1)
	NudgePreAnswerGate     bool `toml:"nudge_pre_answer_gate"`     // enable pre-answer verification gate (default false)
	NudgePreAnswerMinTools int  `toml:"nudge_pre_answer_min_tools"` // min tool calls before gate fires (default 2)

	StopAliases       []string `toml:"stop_aliases"`        // aliases for /stop command (e.g., ["stop", "wait"])
	EnableStopAliases bool     `toml:"enable_stop_aliases"` // enable stop command aliases (default true)

	Webhooks map[string]string `toml:"webhooks"` // webhook hook ID → prompt path (per-agent overrides global entirely)
}

// ModelsConfig holds model-related configuration.
type ModelsConfig struct {
	Aliases  map[string]string `toml:"aliases"`  // shorthand → full model ID (e.g., "opus" → "anthropic:claude-opus-4-6")
	Powerful string            `toml:"powerful"` // model for the powerful group (enables group mode when set)
	Fast     string            `toml:"fast"`     // model for the fast group (defaults to powerful if unset)
	Cheap    string            `toml:"cheap"`    // model for the cheap group (defaults to powerful if unset)
	Calls    map[string]string `toml:"calls"`    // call site overrides: call name → group name
}

// EndpointConfig describes a model API endpoint.
type EndpointConfig struct {
	// Single-format endpoints:
	Format string `toml:"format"` // "anthropic", "openai", or "gemini"
	URL    string `toml:"url"`    // base URL (empty = SDK/library default)

	// Multi-format endpoints (overrides Format+URL when set):
	AnthropicURL string `toml:"anthropic_url"`
	OpenAIURL    string `toml:"openai_url"`
	GeminiURL    string `toml:"gemini_url"`

	// Shared:
	APIKey      string `toml:"api_key"`      // secret name in secrets store (e.g. "openrouter.api_key")
	HTTPTimeout string `toml:"http_timeout"` // Go duration (default "120s")
}

// SupportsFormat reports whether this endpoint supports the given wire format.
func (e EndpointConfig) SupportsFormat(f string) bool {
	switch f {
	case "anthropic":
		return e.AnthropicURL != "" || e.Format == "anthropic"
	case "openai":
		return e.OpenAIURL != "" || e.Format == "openai"
	case "gemini":
		return e.GeminiURL != "" || e.Format == "gemini"
	}
	return false
}

// URLForFormat returns the base URL for the given wire format, or empty for SDK default.
func (e EndpointConfig) URLForFormat(f string) string {
	switch f {
	case "anthropic":
		if e.AnthropicURL != "" {
			return e.AnthropicURL
		}
	case "openai":
		if e.OpenAIURL != "" {
			return e.OpenAIURL
		}
	case "gemini":
		if e.GeminiURL != "" {
			return e.GeminiURL
		}
	}
	return e.URL
}

// KeepaliveConfig controls the cache keepalive timer.
type KeepaliveConfig struct {
	Enabled  bool   `toml:"enabled"`  // enable keepalive timer (default: false)
	Interval string `toml:"interval"` // time since cache last warmed before firing (default: "55m")
	Prompt   string `toml:"prompt"`   // prompt file path ("" = embedded default, "none" = disabled, "default" = embedded)
}

// MemoryFormationConfig controls automatic memory capture and consolidation.
type MemoryFormationConfig struct {
	IntervalEnabled       *bool  `toml:"interval_enabled"`       // periodic capture on timer (nil = true)
	Interval              string `toml:"interval"`               // time between captures (default "1h")
	IntervalPrompt        string `toml:"interval_prompt"`        // prompt override ("" = embedded, "none" = disabled)
	ConsolidationEnabled  *bool  `toml:"consolidation_enabled"`  // curate MEMORY.md periodically (nil = true)
	ConsolidationInterval string `toml:"consolidation_interval"` // min time between consolidations (default "20h")
	ConsolidationPrompt   string `toml:"consolidation_prompt"`   // prompt override ("" = embedded, "none" = disabled)
	SessionEndEnabled     *bool  `toml:"session_end_enabled"`    // capture on /reset and reclaim (nil = true)
	SessionEndPrompt      string `toml:"session_end_prompt"`     // prompt override ("" = embedded, "none" = disabled)
	CompactionEnabled     *bool  `toml:"compaction_enabled"`     // capture before compaction (nil = true)
	CompactionPrompt      string `toml:"compaction_prompt"`      // prompt override ("" = embedded, "none" = disabled)
}

// BackgroundConfig controls the mana-gated background work timer.
type BackgroundConfig struct {
	Enabled  bool   `toml:"enabled"`  // enable background work timer (default: false)
	Interval string `toml:"interval"` // time since last interaction before firing (default: "15m")
	Prompt   string `toml:"prompt"`   // prompt file path ("" = embedded default, "none" = disabled, "default" = embedded)
}

// ManaConfig controls mana budget behavior.
type ManaConfig struct {
	InvestInterval string `toml:"invest_interval"` // quiet period after mana reset before spending (default: "30m")
}

// DebugConfig holds developer/debugging knobs that are off by default.
type DebugConfig struct {
	LogAPIKeySuffix bool `toml:"log_api_key_suffix"` // log last 4 chars of API keys on each provider call (default false)
	CompactionDebug bool `toml:"compaction_debug"`   // send compaction summary as Telegram file attachment (default false)
}

type Config struct {
	DataDir            string                    `toml:"data_dir"`  // directory for databases, sessions, state (default: $HOME/data)
	LLM                LLMConfig                 `toml:"llm"`       // LLM-specific settings (model, max_output_tokens, summary model/endpoint)
	Defaults           DefaultsConfig            `toml:"defaults"`  // global defaults for agent-specific fields
	Models             ModelsConfig              `toml:"models"`    // model aliases and related config
	Endpoints          map[string]EndpointConfig `toml:"endpoints"` // named API endpoints (built-in: anthropic, gemini, openai, openrouter)
	Agents             []AgentConfig             `toml:"agents"`    // multi-agent: array of agents
	Anthropic          AnthropicConfig           `toml:"anthropic"`
	Gemini             GeminiConfig              `toml:"gemini"`
	OpenAI             OpenAIConfig              `toml:"openai"`
	Telegram           TelegramConfig            `toml:"telegram"`
	Discord            DiscordConfig             `toml:"discord"`
	Sessions           SessionsConfig            `toml:"sessions"`
	Memory             MemoryConfig              `toml:"memory"`
	Database           DatabaseConfig            `toml:"database"`
	HTTP               HTTPConfig                `toml:"http"`
	Logging            LoggingConfig             `toml:"logging"`
	TTS                []TTSConfig               `toml:"tts"`
	STT                []STTConfig               `toml:"stt"`
	Bitwarden          BitwardenConfig           `toml:"bitwarden"`
	Cache              CacheConfig               `toml:"cache"`
	Mana               ManaConfig                `toml:"mana"`
	ManaWarnings       ManaWarningsConfig        `toml:"usage_warnings"`
	Environment        EnvironmentConfig         `toml:"environment"`
	Skills             SkillsConfig              `toml:"skills"`
	Resources          ResourcesConfig           `toml:"resources"`
	Debug              DebugConfig               `toml:"debug"`
	Tools              ToolsConfig               `toml:"tools"`
	Keepalive          KeepaliveConfig           `toml:"keepalive"`
	Background         BackgroundConfig          `toml:"background"`
	MemoryFormation    MemoryFormationConfig     `toml:"memory_formation"`
	Commands           []CommandConfig           `toml:"commands"`
	MessageTransforms  []MessageTransform        `toml:"message_transforms"`   // regex find/replace rules applied to inbound messages
	BlockedPaths       []BlockedPath             `toml:"blocked_paths"`        // path prefixes that write/edit tools refuse (with rebuke message)
	WelcomeFile        string                    `toml:"welcome_file"`         // path to welcome/changelog file injected on startup (e.g. /home/foci/WELCOME.md)
	SkipSecurityChecks bool                      `toml:"skip_security_checks"` // if true, skip startup security checks for secrets.toml
	DefinedKeys        map[string]bool           `toml:"-"`                    // keys explicitly set in TOML file (populated by Load)
}
