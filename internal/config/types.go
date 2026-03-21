package config

import (
	"fmt"
	"strconv"
	"strings"
)

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

// InjectionLevel controls whether and what severity of log warnings are injected.
type InjectionLevel string

const (
	InjectionAll  InjectionLevel = "all"  // inject WARN + ERROR
	InjectionErrors InjectionLevel = "errors" // inject ERROR only
	InjectionOff  InjectionLevel = "off"  // disabled
)

// UnmarshalTOML accepts bool (true→"all", false→"off") or string ("all"/"errors"/"off"/"on"/"true"/"false").
func (il *InjectionLevel) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		switch strings.ToLower(val) {
		case "all", "on", "true":
			*il = InjectionAll
			return nil
		case "errors":
			*il = InjectionErrors
			return nil
		case "off", "false":
			*il = InjectionOff
			return nil
		case "":
			*il = ""
			return nil
		default:
			return fmt.Errorf("invalid injection level %q (must be all, errors, off, or bool)", val)
		}
	case bool:
		if val {
			*il = InjectionAll
		} else {
			*il = InjectionOff
		}
		return nil
	default:
		return fmt.Errorf("injection level must be a string (all/errors/off) or bool")
	}
}

// Enabled returns true if injection is active (all or errors).
func (il InjectionLevel) Enabled() bool {
	return il == InjectionAll || il == InjectionErrors
}

// IncludeWarnings returns true if WARN-level entries should be included.
func (il InjectionLevel) IncludeWarnings() bool {
	return il == InjectionAll
}

// ManaConfig holds mana/quota budget and warning settings.
// Embed in Config (global, TOML [mana]) and AgentConfig (per-agent).
// All fields are pointer/slice types for Merge-based resolution.
type ManaConfig struct {
	Name             *string `toml:"name"`              // what to call quota (default "mana")
	Thresholds       []int   `toml:"thresholds"`        // mana percentages to warn at (e.g. [50, 25, 10, 5])
	RestoreThreshold *int    `toml:"restore_threshold"` // inject session notice when mana restores to 100% after being below this (0=disabled)
	InvestInterval   *string `toml:"invest_interval"`   // quiet period after mana reset before spending (default "30m")
}

// AgentMemoryConfig holds per-agent memory sources.
// These are combined with global [memory] sources, with agent-specific
// sources receiving an automatic weight boost.
type AgentMemoryConfig struct {
	Sources []MemorySource `toml:"sources"` // agent-specific memory directories
}

// ContextWindow represents a model's context window size in tokens.
// Accepts plain integers or strings with k/K suffix (1k = 1000 tokens).
type ContextWindow int

// UnmarshalTOML accepts both int (context = 131072) and string (context = "262k").
func (c *ContextWindow) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case int64:
		*c = ContextWindow(val)
		return nil
	case string:
		return c.parse(val)
	default:
		return fmt.Errorf("context: expected int or string, got %T", v)
	}
}

func (c *ContextWindow) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*c = 0
		return nil
	}
	multiplier := 1
	if strings.HasSuffix(s, "k") || strings.HasSuffix(s, "K") {
		multiplier = 1000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "M") {
		multiplier = 1000000
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("context: invalid value %q", s)
	}
	*c = ContextWindow(n * multiplier)
	return nil
}

// ThinkingMode controls whether model thinking/reasoning is enabled.
type ThinkingMode string

// UnmarshalTOML accepts both string ("adaptive"/"off") and bool (true→"adaptive", false→"off").
func (t *ThinkingMode) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		switch strings.ToLower(val) {
		case "adaptive", "on", "true":
			*t = "adaptive"
			return nil
		case "off", "false":
			*t = "off"
			return nil
		case "":
			*t = ""
			return nil
		default:
			return fmt.Errorf("invalid thinking value %q (must be adaptive, off, or bool)", val)
		}
	case bool:
		if val {
			*t = "adaptive"
		} else {
			*t = "off"
		}
		return nil
	default:
		return fmt.Errorf("thinking must be a string (adaptive/off) or bool")
	}
}

// ModelConfig defines a named model with its settings.
// Used in [models.*] TOML sections.
type ModelConfig struct {
	Model           string        `toml:"model"`            // "developer/model_id" (required)
	Endpoint        string        `toml:"endpoint"`         // explicit endpoint override (optional; empty = auto-select from developer)
	Thinking        ThinkingMode  `toml:"thinking"`         // "adaptive", "off", or bool via UnmarshalTOML
	Effort          string        `toml:"effort"`           // "low", "medium", "high"
	Speed           string        `toml:"speed"`            // "fast" or ""
	Context         ContextWindow `toml:"context"`          // context window size in tokens (e.g. 262000 or "262k")
	EnableKeepalive *bool         `toml:"enable_keepalive"` // nil=auto-detect, true/false=explicit
	PromptCacheTTL  string        `toml:"prompt_cache_ttl"` // Go duration, empty=auto-detect
}

// GroupsConfig assigns named models to groups and call sites.
// String fields are pointers for Merge-based per-agent override resolution.
type GroupsConfig struct {
	Powerful  *string           `toml:"powerful"`
	Fast      *string           `toml:"fast"`
	Cheap     *string           `toml:"cheap"`
	Calls     map[string]string `toml:"calls"`
	Fallbacks map[string]string `toml:"fallbacks"`
}

type AgentConfig struct {
	ID        string `toml:"id"`
	Name      string `toml:"name"`      // human-readable name (e.g. "Clutch"); used in voice endpoint agent list
	Emoji     string `toml:"emoji"`     // emoji for agent (e.g. "🥔"); used in voice endpoint agent list
	Workspace string `toml:"workspace"`

	Memory    AgentMemoryConfig `toml:"memory"`    // per-agent memory sources (combined with global [memory])
	Platforms []PlatformConfig  `toml:"platforms"` // per-agent platform configurations

	// Per-agent section overrides — each prefix matches its global TOML section.
	// Resolved via Merge at use time (e.g. config.Merge(acfg.Defaults.DisplayConfig, cfg.Defaults.DisplayConfig)).
	Defaults AgentDefaultsOverride  `toml:"defaults"`          // overrides from [defaults]
	Sessions AgentSessionsOverride  `toml:"sessions"`          // overrides from [sessions]
	Tools    AgentToolsOverride     `toml:"tools"`             // overrides from [tools]
	Debug    DebugConfig            `toml:"debug"`             // overrides from [debug]
	Browser         BrowserConfig         `toml:"browser"`          // overrides from [browser]
	Keepalive       KeepaliveConfig       `toml:"keepalive"`        // overrides from [keepalive]
	Background      BackgroundConfig      `toml:"background"`       // overrides from [background]
	MemoryFormation MemoryFormationConfig `toml:"memory_formation"` // overrides from [memory_formation]
	Mana            ManaConfig            `toml:"mana"`             // overrides from [mana]
	Groups          GroupsConfig          `toml:"groups"`           // overrides from [groups]

	// Per-agent skills and message transforms (empty = use global)
	SkillsDir         string             `toml:"skills_dir"`         // per-agent skills directory (default: $workspace/skills/)
	MessageTransforms []MessageTransform `toml:"message_transforms"` // regex find/replace rules (empty = use global)
	BlockedPaths      []BlockedPath      `toml:"blocked_paths"`      // path prefixes that write/edit tools refuse (empty = use global)
}

// AgentDefaultsOverride groups the 7 config groups whose global home is [defaults].
type AgentDefaultsOverride struct {
	NotifyConfig
	DisplayConfig
	NudgeConfig
	VoiceConfig
	AgentLoopConfig
	BehaviorConfig
	SystemConfig
}

// AgentSessionsOverride groups the config from [sessions] that can be overridden per-agent.
type AgentSessionsOverride struct {
	CompactionConfig
	BranchOrientationFacetPrompt    *string `toml:"branch_orientation_facet_prompt"`
	BranchOrientationHeadlessPrompt *string `toml:"branch_orientation_headless_prompt"`
}

// AgentToolsOverride groups the config groups whose global home is [tools].
type AgentToolsOverride struct {
	ToolConfig
	SummaryConfig
}

// CompactionConfig holds compaction settings. Embed in SessionsConfig (global) and AgentConfig (per-agent).
type CompactionConfig struct {
	CompactionThreshold                     *float64 `toml:"compaction_threshold"`
	CompactionSummaryPrompt                 *string  `toml:"compaction_summary_prompt"`
	CompactionHandoffMsg                    *string  `toml:"compaction_handoff_msg"`
	CompactionPreserveMessages              *int     `toml:"compaction_preserve_messages"`
	CompactionEffort                        *string  `toml:"compaction_effort"`
	FacetNoCompact                          *bool    `toml:"facet_no_compact"`
	AutocompactBeforeManaRefresh            *bool    `toml:"autocompact_before_mana_refresh"`
	AutocompactBeforeManaRefreshThreshold   *string  `toml:"autocompact_before_mana_refresh_threshold"`
	AutocompactBeforeManaRefreshFactor      *float64 `toml:"autocompact_before_mana_refresh_factor"`
	AutocompactBeforeManaRefreshPreserve    *int     `toml:"autocompact_before_mana_refresh_preserve"`
	AutocompactBeforeManaRefreshPreservePct *float64 `toml:"autocompact_before_mana_refresh_preserve_pct"`
}

// NudgeConfig holds nudge system settings. Embed in DefaultsConfig (global) and AgentConfig (per-agent).
type NudgeConfig struct {
	NudgeEnable                     *bool   `toml:"nudge_enable"`
	NudgeAutoExtract                *bool   `toml:"nudge_auto_extract"`
	NudgeCooldown                   *int    `toml:"nudge_cooldown"`
	NudgeMaxPerBatch                *int    `toml:"nudge_max_per_batch"`
	NudgePreAnswerGate              *bool   `toml:"nudge_pre_answer_gate"`
	NudgePreAnswerMinTools          *int    `toml:"nudge_pre_answer_min_tools"`
	NudgeDefaultEnable              *bool   `toml:"nudge_default_enable"`
	NudgeDefaultFrequency           *int    `toml:"nudge_default_frequency"`
	NudgeDefaultScratchpadFrequency *int    `toml:"nudge_default_scratchpad_frequency"`
	NudgeDefaultBraindeadThreshold  *int    `toml:"nudge_default_braindead_threshold"`
	NudgeDefaultBraindeadPrompt     *string `toml:"nudge_default_braindead_prompt"`
}

// SummaryConfig holds tool result summarisation settings.
// Embed in ToolsConfig (global), DefaultsConfig (defaults), and AgentConfig (per-agent).
type SummaryConfig struct {
	MaxResultChars       *int  `toml:"max_result_chars"`
	MaxSummaryChars      *int  `toml:"max_summary_chars"`
	AutoSummarise        *bool `toml:"auto_summarise"`
	SummaryContextTurns  *int  `toml:"summary_context_turns"`
	SummaryContextChars  *int  `toml:"summary_context_chars"`
	MaxSummaryInputChars *int  `toml:"max_summary_input_chars"`
	MaxImagePixels       *int  `toml:"max_image_pixels"`
}

// VoiceConfig holds TTS/STT settings. Embed in DefaultsConfig and AgentConfig.
type VoiceConfig struct {
	TTS             *string           `toml:"tts"`
	STT             *string           `toml:"stt"`
	TTSRate         *float64          `toml:"tts_rate"`
	TTSReplacements map[string]string `toml:"tts_replacements"`
	STTReplacements map[string]string `toml:"stt_replacements"`
}

// AgentLoopConfig holds settings consumed by agent.HandleTurn().
// Embed in DefaultsConfig and AgentConfig.
type AgentLoopConfig struct {
	MaxOutputTokens               *int    `toml:"max_output_tokens"`
	MaxToolLoops                  *int    `toml:"max_tool_loops"`
	DuplicateMessages             *bool   `toml:"duplicate_messages"`
	BatchPartialAssistantMessages *bool   `toml:"batch_partial_assistant_messages"`
	BatchPartialJoiner            *string `toml:"batch_partial_joiner"`
	CacheTTL                      *string `toml:"cache_ttl"`
}

// BehaviorConfig holds agent behavioral settings.
// Embed in DefaultsConfig and AgentConfig.
type BehaviorConfig struct {
	SteerMode             *bool    `toml:"steer_mode"`
	GroupThrottle         *string  `toml:"group_throttle"`
	TurnLockWarnThreshold *string  `toml:"turn_lock_warn_threshold"`
	EnableStopAliases     *bool    `toml:"enable_stop_aliases"`
	StopAliases           []string `toml:"stop_aliases"`
}

// SystemConfig holds system-level agent settings.
// Embed in DefaultsConfig and AgentConfig.
type SystemConfig struct {
	SystemFiles []string          `toml:"system_files"`
	Webhooks    map[string]string `toml:"webhooks"`
}

// ToolConfig holds per-agent tool behavioral overrides.
// Embed in ToolsConfig (global home) and AgentConfig (per-agent).
type ToolConfig struct {
	ExecAutoBackground  *int    `toml:"exec_auto_background"`
	MaxConcurrentSpawns *int    `toml:"max_concurrent_spawns"`
	ExploreMaxDepth     *int    `toml:"explore_max_depth"`
	MaxUploadFileSize   *int64  `toml:"max_upload_file_size"`
	TmuxAutopilot       *bool   `toml:"tmux_autopilot"`
	TmuxWatchThreshold  *string `toml:"tmux_watch_threshold"`
	TmuxSessionTTL      *string `toml:"tmux_session_ttl"`
	SearchProvider      *string `toml:"search_provider"`
	FetchProvider       *string `toml:"fetch_provider"`
	TodoFormat          *string `toml:"todo_format"`
}

type GeminiConfig struct {
	HTTPTimeout string `toml:"http_timeout"` // HTTP timeout for API calls (default "120s")
	CacheTTL    string `toml:"cache_ttl"`    // context cache TTL (default "1h", "0" disables)
}

type OpenAIConfig struct {
	BaseURL     string `toml:"base_url"`     // API base URL (default: "https://api.openai.com", override for OpenRouter/Together/etc.)
	HTTPTimeout string `toml:"http_timeout"` // HTTP timeout for API calls (default "120s")
}

type AnthropicConfig struct {
	HTTPTimeout       string `toml:"http_timeout"`       // HTTP timeout for API calls (default "600s")
	UsageAPITimeout   string `toml:"usage_api_timeout"`  // HTTP timeout for usage API calls (default "10s")
	UsageCacheTTL     string `toml:"usage_cache_ttl"`    // cache TTL for usage API responses (default "10m")
	CCExpiryThreshold string `toml:"cc_expiry_threshold"` // how far before expiry to trigger proactive token refresh (default "5m")
	UseSDK            bool   `toml:"use_sdk"`            // use SDK transport (default true; false = raw HTTP)
	Streaming         bool   `toml:"streaming"`          // use streaming API (default false; requires use_sdk)
}

// DisplayConfig holds display-related settings that can be set at any level
// of the configuration cascade. All fields are pointer types so Merge can
// distinguish "not set" from "set to zero value".
type DisplayConfig struct {
	ShowToolCalls        *ToolCallDisplay `toml:"show_tool_calls"`        // tool call display: off, preview, full
	ShowThinking         *ShowThinking    `toml:"show_thinking"`          // thinking display: off, compact, true
	StreamOutput         *bool            `toml:"stream_output"`          // stream model output in real-time
	StreamInterval       *string          `toml:"stream_interval"`        // duration between message edits during streaming
	Streaming            *bool            `toml:"streaming"`              // use streaming API (requires use_sdk)
	DisplayWidth         *int             `toml:"display_width"`          // display width for dividers
	ReceivedFilesDir     *string          `toml:"received_files_dir"`     // save received files to this directory
	InjectedMessageHeader *string         `toml:"injected_message_header"` // header prepended to injected messages
}

// AccessConfig holds access control settings that can be set at any level
// of the configuration cascade.
type AccessConfig struct {
	AllowedUsers   []string `toml:"allowed_users"`   // platform-specific user IDs allowed to interact
	RequireMention *bool    `toml:"require_mention"`  // require @mention in group chats
}

// NotifyConfig holds notification/warning settings that can be configured at
// any scope level. Resolution follows the 5-level cascade via Merge.
// All fields are nillable so nil means "not set, inherit from wider scope."
type NotifyConfig struct {
	InjectAgentWarnings *InjectionLevel `toml:"inject_agent_warnings"` // inject warnings/errors into agent session
	InjectChatWarnings  *InjectionLevel `toml:"inject_chat_warnings"`  // send warnings/errors as chat notifications
	StartupNotify       *bool           `toml:"startup_notify"`        // send startup notification
	CompactionNotify    *bool           `toml:"compaction_notify"`     // send notification on compaction
	TaskListNotify      *bool           `toml:"task_list_notify"`      // send notification on task list changes
	CompactionDebug     *bool           `toml:"compaction_debug"`      // send compaction summary as file attachment
}

// StartupNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) StartupNotifyEnabled() bool {
	if n.StartupNotify != nil {
		return *n.StartupNotify
	}
	return true
}

// CompactionNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) CompactionNotifyEnabled() bool {
	if n.CompactionNotify != nil {
		return *n.CompactionNotify
	}
	return true
}

// TaskListNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) TaskListNotifyEnabled() bool {
	if n.TaskListNotify != nil {
		return *n.TaskListNotify
	}
	return true
}

// CompactionDebugEnabled returns the resolved value (default: false).
func (n NotifyConfig) CompactionDebugEnabled() bool {
	if n.CompactionDebug != nil {
		return *n.CompactionDebug
	}
	return false
}

// InjectAgentWarningsLevel returns the resolved InjectionLevel (default: off).
func (n NotifyConfig) InjectAgentWarningsLevel() InjectionLevel {
	if n.InjectAgentWarnings != nil {
		return *n.InjectAgentWarnings
	}
	return InjectionOff
}

// InjectChatWarningsLevel returns the resolved InjectionLevel (default: off).
func (n NotifyConfig) InjectChatWarningsLevel() InjectionLevel {
	if n.InjectChatWarnings != nil {
		return *n.InjectChatWarnings
	}
	return InjectionOff
}

// PlatformConfig is the unified platform configuration used for both global
// [[platforms]] entries and per-agent [[agents.platforms]] overrides.
type PlatformConfig struct {
	ID string `toml:"id"`

	// Embedded config groups (cascade via Merge)
	NotifyConfig  `toml:",inline"`
	DisplayConfig `toml:",inline"`
	AccessConfig  `toml:",inline"`

	// Shared platform fields
	Bot              string   `toml:"bot"`
	BotSecret        string   `toml:"bot_secret"`
	FacetBots        []string `toml:"facet_bots"`
	FacetSessionTTL  string   `toml:"facet_session_ttl"`
	MessageQueueSize int      `toml:"message_queue_size"`

	// Platform-specific subsections (at most one non-nil, must match ID)
	Telegram *TelegramSpecific `toml:"telegram"`
	Discord  *DiscordSpecific  `toml:"discord"`
}

// Platform returns the PlatformConfig with the given ID, or nil.
func (c *Config) Platform(id string) *PlatformConfig {
	for i := range c.Platforms {
		if c.Platforms[i].ID == id {
			return &c.Platforms[i]
		}
	}
	return nil
}

// Platform returns the PlatformConfig with the given ID from agent platforms, or nil.
func (a *AgentConfig) Platform(id string) *PlatformConfig {
	for i := range a.Platforms {
		if a.Platforms[i].ID == id {
			return &a.Platforms[i]
		}
	}
	return nil
}

// SafeNotify returns the NotifyConfig from a *PlatformConfig, or zero if nil.
func (p *PlatformConfig) SafeNotify() NotifyConfig {
	if p == nil {
		return NotifyConfig{}
	}
	return p.NotifyConfig
}

// SafeDisplay returns the DisplayConfig from a *PlatformConfig, or zero if nil.
func (p *PlatformConfig) SafeDisplay() DisplayConfig {
	if p == nil {
		return DisplayConfig{}
	}
	return p.DisplayConfig
}

// TelegramSpecific holds Telegram-only config fields.
type TelegramSpecific struct {
	LongPollTimeout string  `toml:"long_poll_timeout"` // default "65s"
	TableWrapLines  *int    `toml:"table_wrap_lines"`  // default 5
	TableStyle      *string `toml:"table_style"`       // default "pretty"
}

// DiscordSpecific holds Discord-only config fields.
type DiscordSpecific struct {
	AutoThread *bool  `toml:"auto_thread"` // default true
	GuildID    string `toml:"guild_id"`
}

// ApplyDefaults fills zero-value fields from the given defaults.
func (p *PlatformConfig) ApplyDefaults(defaults PlatformConfig) {
	p.NotifyConfig = Merge(p.NotifyConfig, defaults.NotifyConfig)
	p.DisplayConfig = Merge(p.DisplayConfig, defaults.DisplayConfig)
	p.AccessConfig = Merge(p.AccessConfig, defaults.AccessConfig)
	if p.FacetSessionTTL == "" {
		p.FacetSessionTTL = defaults.FacetSessionTTL
	}
	if p.MessageQueueSize == 0 {
		p.MessageQueueSize = defaults.MessageQueueSize
	}
	// Platform-specific: merge only if same platform
	if p.Telegram != nil && defaults.Telegram != nil {
		if p.Telegram.LongPollTimeout == "" {
			p.Telegram.LongPollTimeout = defaults.Telegram.LongPollTimeout
		}
		if p.Telegram.TableWrapLines == nil {
			p.Telegram.TableWrapLines = defaults.Telegram.TableWrapLines
		}
		if p.Telegram.TableStyle == nil {
			p.Telegram.TableStyle = defaults.Telegram.TableStyle
		}
	}
	if p.Discord != nil && defaults.Discord != nil {
		if p.Discord.AutoThread == nil {
			p.Discord.AutoThread = defaults.Discord.AutoThread
		}
		if p.Discord.GuildID == "" {
			p.Discord.GuildID = defaults.Discord.GuildID
		}
	}
}

type SessionsConfig struct {
	Dir string `toml:"dir"`

	CompactionConfig  // compaction settings (global defaults, overridable per-agent)
	CompactionMaxTokens   int `toml:"compaction_max_tokens"`         // max output tokens for summary (default 4096)
	CompactionMinMessages int `toml:"compaction_min_messages"`       // min messages before compacting (default 4)
	MaxSystemPromptFile   int `toml:"max_system_prompt_chars_file"`  // per-file char threshold for warnings (default 20000)
	MaxSystemPromptTotal  int `toml:"max_system_prompt_chars_total"` // total system prompt char threshold (default 80000)

	BranchOrientationFacetPrompt    *string `toml:"branch_orientation_facet_prompt"`    // path to prompt file for user-attached facet branches
	BranchOrientationHeadlessPrompt *string `toml:"branch_orientation_headless_prompt"` // path to prompt file for headless branches (cron, spawn, keepalive)

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
	SocketPath              string `toml:"socket_path"`               // Unix socket path for same-user auth (default: auto-resolved to data dir)
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


type EnvironmentConfig struct {
	Enabled  bool   `toml:"enabled"`   // inject environment block as first system block (default true)
	DocsPath string `toml:"docs_path"` // path to platform docs directory; relative paths resolve against $HOME
}

type SkillsConfig struct {
	Dir string `toml:"dir"` // shared skills directory (default: $home/shared/skills/)
}

type ResourcesConfig struct {
	MemoryGuardEnabled      bool    `toml:"memory_guard_enabled"`      // enable system memory guard (default true)
	MemoryGuardInterval     string  `toml:"memory_guard_interval"`     // check interval (default "60s")
	MemoryWarnPercent       int     `toml:"memory_warn_percent"`       // warn threshold as % of total RAM (default 25)
	MemoryKillPercent       int     `toml:"memory_kill_percent"`       // kill threshold as % of total RAM (default 40)
	MemoryPressureThreshold float64 `toml:"memory_pressure_threshold"` // PSI avg10 threshold to require before acting (default 10.0)
	GoroutineMonitorInterval  string `toml:"goroutine_monitor_interval"`  // goroutine count check interval (default "60s")
	GoroutineMonitorThreshold int    `toml:"goroutine_monitor_threshold"` // warn when goroutine count exceeds this (0 = auto: 30 + 25×agents + 5×telegram_bots)
}

// BrowserConfig holds configuration for the browser automation tool.
// All fields are pointer types for Merge-based resolution (per-agent → global).
// TOML: [browser] globally, [[agents]].browser per-agent.
type BrowserConfig struct {
	Enabled        *bool    `toml:"enabled"`          // enable browser tool (default true)
	Headless       *bool    `toml:"headless"`          // run headless (default true)
	TimeoutSec     *int     `toml:"timeout_sec"`       // page operation timeout in seconds (default 30)
	UserDataDir    *string  `toml:"user_data_dir"`     // Chrome user data dir (empty = temp profile)
	ExecutablePath *string  `toml:"executable_path"`   // Chrome executable path (empty = auto-detect)
	Incognito      *bool    `toml:"incognito"`          // use incognito mode (default true)
	DOMStableSec   *float64 `toml:"dom_stable_sec"`    // DOM stability wait interval in seconds (default 1)
	DOMStableDiff  *float64 `toml:"dom_stable_diff"`   // DOM stability diff threshold (default 0.2)
}

type ToolsConfig struct {
	SummaryConfig // global summary/tool-result defaults (resolved via Merge with per-agent)
	ToolConfig    // global tool behavioral defaults (resolved via Merge with per-agent)

	TempDir                 string   `toml:"temp_dir"`                   // where to write large tool results (default /tmp/foci/tool-results)
	TmuxCols                int      `toml:"tmux_cols"`                  // tmux window columns on start (default 300)
	TmuxRows                int      `toml:"tmux_rows"`                  // tmux window rows on start (default 30)
	ExecDefaultTimeout      int      `toml:"exec_default_timeout"`       // default timeout for exec commands in seconds (default 30)
	TmuxCommandTimeout      string   `toml:"tmux_command_timeout"`       // timeout for tmux control commands (default "5s")
	WebFetchTimeout         string   `toml:"web_fetch_timeout"`          // HTTP timeout for web fetch (default "30s")
	WebFetchMaxBytes        int      `toml:"web_fetch_max_bytes"`        // max bytes to read from web fetch (default 1048576 = 1MB)
	WebSearchTimeout        string   `toml:"web_search_timeout"`         // HTTP timeout for web search (default "15s")
	ToolCallPreviewChars    int      `toml:"tool_call_preview_chars"`    // max chars for tool call param preview in Telegram (default 450)
	TmuxMemoryCheckInterval string   `toml:"tmux_memory_check_interval"` // how often to check tmux RSS (default "5m", "0" disables)
	TmuxMemoryWarn          string   `toml:"tmux_memory_warn"`           // warn threshold as % of RAM or absolute (default "10%")
	TmuxMemoryCritical      string   `toml:"tmux_memory_critical"`       // critical threshold (default "20%")
	TmuxMemoryKill          string   `toml:"tmux_memory_kill"`           // kill threshold (default "30%")
	WebSearchMaxUses        int      `toml:"web_search_max_uses"`        // max searches per API call (0 = unlimited)
	WebSearchAllowedDomains []string `toml:"web_search_allowed_domains"` // domain whitelist (mutually exclusive with blocked)
	WebSearchBlockedDomains []string `toml:"web_search_blocked_domains"` // domain blacklist
	WebFetchMaxUses         int      `toml:"web_fetch_max_uses"`         // max fetches per API call (0 = unlimited)
	WebFetchAllowedDomains  []string `toml:"web_fetch_allowed_domains"`  // domain whitelist
	WebFetchBlockedDomains  []string `toml:"web_fetch_blocked_domains"`  // domain blacklist
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
// All embedded config groups use Merge[T] for resolution at use time.
type DefaultsConfig struct {
	NotifyConfig    // notification defaults
	DisplayConfig   // display defaults
	NudgeConfig     // nudge system defaults
	VoiceConfig     // TTS/STT defaults
	AgentLoopConfig // agent loop defaults
	BehaviorConfig  // behavioral defaults
	SystemConfig    // system defaults
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
// All fields are pointer types for Merge-based resolution (per-agent → global).
type KeepaliveConfig struct {
	Enabled  *bool   `toml:"enabled"`  // enable keepalive timer (default: false)
	Interval *string `toml:"interval"` // time since cache last warmed before firing (default: "55m")
	Prompt   *string `toml:"prompt"`   // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
}

// MemoryFormationConfig controls automatic memory capture and consolidation.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type MemoryFormationConfig struct {
	IntervalEnabled       *bool   `toml:"interval_enabled"`       // periodic capture on timer (nil = true)
	Interval              *string `toml:"interval"`               // time between captures (default "1h")
	IntervalPrompt        *string `toml:"interval_prompt"`        // prompt override (nil = embedded, "none" = disabled)
	ConsolidationEnabled  *bool   `toml:"consolidation_enabled"`  // curate MEMORY.md periodically (nil = true)
	ConsolidationInterval *string `toml:"consolidation_interval"` // min time between consolidations (default "20h")
	ConsolidationPrompt   *string `toml:"consolidation_prompt"`   // prompt override (nil = embedded, "none" = disabled)
	SessionEndEnabled     *bool   `toml:"session_end_enabled"`    // capture on /reset and reclaim (nil = true)
	SessionEndPrompt      *string `toml:"session_end_prompt"`     // prompt override (nil = embedded, "none" = disabled)
	CompactionEnabled     *bool   `toml:"compaction_enabled"`     // capture before compaction (nil = true)
	CompactionPrompt      *string `toml:"compaction_prompt"`      // prompt override (nil = embedded, "none" = disabled)
}

// BackgroundConfig controls the mana-gated background work timer.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type BackgroundConfig struct {
	Enabled  *bool   `toml:"enabled"`  // enable background work timer (default: false)
	Interval *string `toml:"interval"` // time since last interaction before firing (default: "15m")
	Prompt   *string `toml:"prompt"`   // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
}

// DebugConfig holds developer/debugging knobs.
// Embed in Config (global) and AgentConfig (per-agent) for Merge-based resolution.
type DebugConfig struct {
	LogAPIKeySuffix *bool `toml:"log_api_key_suffix"` // log last 4 chars of API keys on each provider call (default false)
	MessagesInLog   *bool `toml:"messages_in_log"`    // log user message content to event log (default false for privacy)
}

type Config struct {
	DataDir            string                       `toml:"data_dir"`  // directory for databases, sessions, state (default: $HOME/data)
	Defaults           DefaultsConfig               `toml:"defaults"`  // global defaults for agent-specific fields
	Groups             GroupsConfig                  `toml:"groups"`    // model group assignments and fallbacks
	Models             map[string]ModelConfig        `toml:"models"`    // named model definitions with per-model settings
	Endpoints          map[string]EndpointConfig     `toml:"endpoints"` // named API endpoints (built-in: anthropic, gemini, openai, openrouter)
	Agents             []AgentConfig             `toml:"agents"`    // multi-agent: array of agents
	Anthropic          AnthropicConfig           `toml:"anthropic"`
	Gemini             GeminiConfig              `toml:"gemini"`
	OpenAI             OpenAIConfig              `toml:"openai"`
	Platforms          []PlatformConfig          `toml:"platforms"`
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
	Environment        EnvironmentConfig         `toml:"environment"`
	Skills             SkillsConfig              `toml:"skills"`
	Resources          ResourcesConfig           `toml:"resources"`
	Debug              DebugConfig               `toml:"debug"`
	Tools              ToolsConfig               `toml:"tools"`
	Browser            BrowserConfig             `toml:"browser"`
	Keepalive          KeepaliveConfig           `toml:"keepalive"`
	Background         BackgroundConfig          `toml:"background"`
	MemoryFormation    MemoryFormationConfig     `toml:"memory_formation"`
	Commands           []CommandConfig           `toml:"commands"`
	MessageTransforms  []MessageTransform        `toml:"message_transforms"`   // regex find/replace rules applied to inbound messages
	BlockedPaths       []BlockedPath             `toml:"blocked_paths"`        // path prefixes that write/edit tools refuse (with rebuke message)
	WelcomeFile        string                    `toml:"welcome_file"`         // path to welcome/changelog file injected on startup (e.g. /home/foci/WELCOME.md)
	SkipSecurityChecks bool                      `toml:"skip_security_checks"` // if true, skip startup security checks for secrets.toml
	DefinedKeys        map[string]bool           `toml:"-"`                    // keys explicitly set in TOML file (populated by Load)
	UndefinedKeys      []string                  `toml:"-"`                    // unrecognised TOML keys (populated by Load, logged by caller)
}
