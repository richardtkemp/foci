package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"foci/log"

	"github.com/BurntSushi/toml"
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

// AgentUsageWarningsConfig holds per-agent mana warning thresholds.
// When set, completely replaces global [usage_warnings] thresholds.
type AgentUsageWarningsConfig struct {
	Thresholds []int `toml:"thresholds"` // mana percentages to warn at (replaces global, not merged)
}

// AgentMemoryConfig holds per-agent memory sources.
// These are combined with global [memory] sources, with agent-specific
// sources receiving an automatic weight boost.
type AgentMemoryConfig struct {
	Sources []MemorySource `toml:"sources"` // agent-specific memory directories
}

type AgentConfig struct {
	ID                      string            `toml:"id"`
	Name                    string            `toml:"name"`  // human-readable name (e.g. "Clutch"); used in voice endpoint agent list
	Emoji                   string            `toml:"emoji"` // emoji for agent (e.g. "🥔"); used in voice endpoint agent list
	Model                   string            `toml:"model"`
	Workspace               string            `toml:"workspace"`
	SystemFiles             []string          `toml:"system_files"`              // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages       bool              `toml:"duplicate_messages"`        // send user text twice per API call (improves instruction following)
	BranchOrientationPrompt string            `toml:"branch_orientation_prompt"` // path to prompt file injected into all branch sessions (multiball, cron, spawn)
	TelegramBot             string            `toml:"telegram_bot"`              // references key in [telegram.bots] map
	MultiballBots           []string          `toml:"multiball_bots"`            // references keys in [telegram.bots] map (optional)
	Memory                  AgentMemoryConfig `toml:"memory"`                    // per-agent memory sources (combined with global [memory])
	MaxToolLoops            int               `toml:"max_tool_loops"`            // max tool iterations per turn (default 25)
	MaxOutputTokens         int               `toml:"max_output_tokens"`         // max tokens in model response (default 8192)
	BraindeadThreshold      int               `toml:"braindead_threshold"`       // consecutive tool loops before warning (0 = disabled, default 10)
	BraindeadPrompt         string            `toml:"braindead_prompt"`          // warning text injected as user message
	Effort                  string            `toml:"effort"`                    // effort level: "low", "medium", "high" (empty = omit from request)
	Thinking                string            `toml:"thinking"`                  // thinking mode: "off" (default), "adaptive"
	TTSRate                 float64           `toml:"tts_rate"`                  // per-agent TTS speech rate override (0 = use global [voice] tts_rate)
	InjectAgentWarnings     bool              `toml:"inject_agent_warnings"`     // inject warnings/errors into agent session (default false)
	StartupNotification     *bool             `toml:"startup_notification"`      // send startup notification (nil = use global enable_startup_notify)
	ShowToolCalls           *ToolCallDisplay  `toml:"show_tool_calls"`           // show tool call messages in Telegram (nil = use global telegram.show_tool_calls)
	ShowThinking            *ShowThinking     `toml:"show_thinking"`             // show thinking blocks in Telegram (nil = use global telegram.show_thinking)
	DisplayWidth            *int              `toml:"display_width"`             // display width for dividers in Telegram (nil = use global telegram.display_width)
	MessagesInLog           *bool             `toml:"messages_in_log"`           // log user message content to event log (nil = use global logging.messages_in_log)
	ReceivedFilesDir        string            `toml:"received_files_dir"`        // save received files to this directory (empty = disabled)
	AllowedUsers            []string          `toml:"allowed_users"`             // per-agent allowed Telegram user IDs (empty = use global [telegram] allowed_users)
	// Per-agent compaction overrides (nil/empty = use global [sessions] value)
	CompactionThreshold        *float64 `toml:"compaction_threshold"`         // compact at this % of context window
	CompactionSummaryPrompt    string   `toml:"compaction_summary_prompt"`    // path to summary prompt file
	CompactionHandoffMsg       string   `toml:"compaction_handoff_msg"`       // handoff message after compaction
	CompactionNotify           *bool    `toml:"compaction_notify"`            // send Telegram notification on compaction
	CompactionDebug            *bool    `toml:"compaction_debug"`             // send compaction summary as Telegram file
	CompactionPreserveMessages *int     `toml:"compaction_preserve_messages"` // preserve last N messages through compaction (nil = use global)
	CompactionEffort           string   `toml:"compaction_effort"`            // effort for compaction API calls (empty = use session effort)
	// Per-agent skills and prompt rules (empty = use global)
	SkillsDirs  []string     `toml:"skills_dirs"`  // skill directories (empty = use global [skills] dirs)
	PromptRules []PromptRule `toml:"prompt_rules"` // regex find/replace rules (empty = use global)
	// Per-agent tool behaviour (0 = use global [tools] value)
	ExecAutoBackground  int    `toml:"exec_auto_background"`  // seconds before auto-backgrounding exec
	MaxConcurrentSpawns int    `toml:"max_concurrent_spawns"` // max concurrent spawn sessions
	MaxUploadFileSize   int64  `toml:"max_upload_file_size"`  // max file size for multipart uploads in bytes
	TmuxBraindead       *bool  `toml:"tmux_braindead"`        // per-agent tmux braindead override (nil = use global)
	TmuxWatchThreshold  string `toml:"tmux_watch_threshold"`  // per-agent watch threshold (empty = use global)
	SummaryContextTurns int    `toml:"summary_context_turns"` // recent turns for auto-summary context (0 = use global)
	SummaryContextChars int    `toml:"summary_context_chars"` // max chars of context for auto-summary (0 = use global)
	SearchProvider      string `toml:"search_provider"`       // "anthropic" or "brave" (empty = use global)
	FetchProvider       string `toml:"fetch_provider"`        // "anthropic" or "builtin" (empty = use global)
	// Per-agent keepalive/background (zero = use global [keepalive]/[background])
	Keepalive       KeepaliveConfig       `toml:"keepalive"`        // per-agent keepalive override
	Background      BackgroundConfig      `toml:"background"`       // per-agent background override
	MemoryFormation MemoryFormationConfig `toml:"memory_formation"` // per-agent memory formation override
	// Per-agent usage warning thresholds (nil = use global [usage_warnings])
	UsageWarnings AgentUsageWarningsConfig `toml:"usage_warnings"` // per-agent mana warning thresholds
}

type AnthropicConfig struct {
	SetupToken      string `toml:"setup_token"` // static setup-token (fallback when OAuth not available)
	BraveAPIKey     string `toml:"brave_api_key"`
	HTTPTimeout     string `toml:"http_timeout"`      // HTTP timeout for API calls (default "600s")
	UsageAPITimeout string `toml:"usage_api_timeout"` // HTTP timeout for usage API calls (default "10s")
}

// TelegramBotConfig defines a named Telegram bot in the [telegram.bots] map.
// Each bot's token is resolved from secrets.toml via the token_secret key.
type TelegramBotConfig struct {
	TokenSecret string `toml:"token_secret"` // key in secrets.toml (e.g., "telegram.primary")
}

type TelegramConfig struct {
	BotToken            string                       `toml:"bot_token"` // legacy single-bot token
	AllowedUsers        []string                     `toml:"allowed_users"`
	MultiballBots       []string                     `toml:"multiball_bots"`        // shared multiball pool: references keys in [telegram.bots] map
	Bots                map[string]TelegramBotConfig `toml:"bots"`                  // named bots for multi-agent
	StopAliases         []string                     `toml:"stop_aliases"`          // aliases for /stop command (e.g., ["stop", "wait"])
	EnableStopAliases   bool                         `toml:"enable_stop_aliases"`   // enable stop command aliases (default true)
	EnableStartupNotify bool                         `toml:"enable_startup_notify"` // send notification on startup (default true)
	MultiballSessionTTL string                       `toml:"multiball_session_ttl"` // idle TTL before a multiball bot can be reclaimed (default "60m", "0" disables)
	MessageQueueSize    int                          `toml:"message_queue_size"`    // outbound message queue buffer size (default 64)
	LongPollTimeout     string                       `toml:"long_poll_timeout"`     // long-poll timeout for getUpdates (default "65s")
	ShowToolCalls       ToolCallDisplay              `toml:"show_tool_calls"`       // show tool call messages in Telegram: "off" (default), "preview", "full"
	ShowThinking        ShowThinking                 `toml:"show_thinking"`         // show thinking blocks in Telegram: "off" (default), "compact", "true"
	DisplayWidth        int                          `toml:"display_width"`         // display width for dividers in Telegram (default 44)
	ReceivedFilesDir    string                       `toml:"received_files_dir"`    // save received files to this directory (empty = disabled, per-agent overrides)
}

type SessionsConfig struct {
	Dir                        string  `toml:"dir"`
	CompactionThreshold        float64 `toml:"compaction_threshold"`          // compact at this % of context window (default 0.8)
	CompactionMaxTokens        int     `toml:"compaction_max_tokens"`         // max output tokens for summary (default 4096)
	CompactionMinMessages      int     `toml:"compaction_min_messages"`       // min messages before compacting (default 4)
	CompactionSummaryPrompt    string  `toml:"compaction_summary_prompt"`     // path to summary prompt file
	CompactionHandoffMsg       string  `toml:"compaction_handoff_msg"`        // handoff message after compaction
	CompactionNotify           *bool   `toml:"compaction_notify"`             // send Telegram notification on compaction (default true)
	MaxSystemPromptFile        int     `toml:"max_system_prompt_chars_file"`  // per-file char threshold for warnings (default 20000)
	MaxSystemPromptTotal       int     `toml:"max_system_prompt_chars_total"` // total system prompt char threshold (default 80000)
	CompactionDebug            bool    `toml:"compaction_debug"`              // send compaction summary as Telegram file attachment (default false)
	CompactionPreserveMessages int     `toml:"compaction_preserve_messages"`  // preserve last N messages through compaction (default 25, 0 disables)
	BranchOrientationPrompt    string  `toml:"branch_orientation_prompt"`     // path to prompt file injected into all branch sessions
}

type MemorySource struct {
	Name   string  `toml:"name"`   // unique identifier (e.g., "canonical", "code", "docs")
	Dir    string  `toml:"dir"`    // directory path to index
	Weight float64 `toml:"weight"` // weight multiplier: 0.0-1.0 (1.0 = highest priority)
}

type MemoryConfig struct {
	Dir                string         `toml:"dir"`                 // backward compat: single directory
	Sources            []MemorySource `toml:"sources"`             // new: multiple sources with weights
	ReindexDebounce    string         `toml:"reindex_debounce"`    // delay before reindex (e.g., "500ms", "2s"), default "0s"
	ConversationWeight float64        `toml:"conversation_weight"` // weight multiplier for conversation search results (default 0.1)
	SearchLimit        int            `toml:"search_limit"`        // max search results to return (default 20)
}

type DatabaseConfig struct {
	BusyTimeout string `toml:"busy_timeout"` // SQLite busy timeout for concurrent access (default "5s")
}

type HTTPConfig struct {
	Port                    int    `toml:"port"`
	Bind                    string `toml:"bind"`
	GracefulShutdownTimeout string `toml:"graceful_shutdown_timeout"` // time to wait for in-flight requests on shutdown (default "30s")
}

type LoggingConfig struct {
	Level                 string `toml:"level"`
	EventFile             string `toml:"event_file"`
	APIFile               string `toml:"api_file"`
	ConversationFile      string `toml:"conversation_file"`
	FullPayload           bool   `toml:"full_payload"`            // write full API payloads to api-payload.jsonl
	PayloadFile           string `toml:"payload_file"`            // path to api-payload.jsonl (default: api-payload.jsonl)
	CacheBustDetect       bool   `toml:"cache_bust_detect"`       // alert when cache_read drops >50% vs previous request
	CacheBustIdleMinutes  int    `toml:"cache_bust_idle_minutes"` // suppress cache bust alert if session idle > N minutes (default 10)
	WarningMaxPerWindow              int    `toml:"warning_max_per_window"`               // max identical warnings per window before suppression (default 3)
	WarningWindowDuration            string `toml:"warning_window_duration"`              // time window for warning dedup (default "5m")
	WarningProactiveActiveInterval   string `toml:"warning_proactive_active_interval"`    // min interval between proactive warning turns when user is active (default "5m")
	WarningProactiveInactiveInterval string `toml:"warning_proactive_inactive_interval"`  // min interval when user is inactive (default "1h")
	WarningProactiveActivityThreshold string `toml:"warning_proactive_activity_threshold"` // user is "active" if last message within this window (default "10m")
	LogRotation           bool   `toml:"log_rotation"`            // enable built-in log rotation (default true)
	RotationPeriod        string `toml:"rotation_period"`         // how often to rotate (default "24h")
	RetentionPeriod       string `toml:"retention_period"`        // keep lines newer than this (default "48h")
	RotationMaxLineSize   string `toml:"rotation_max_line_size"`  // max line size for scanner buffer (default "64MB")
	ArchiveDir            string `toml:"archive_dir"`             // gzip archive directory (default: log_dir/archive/)
	MessagesInLog         bool   `toml:"messages_in_log"`         // log user message content to event log (default false for privacy)
}

type VoiceConfig struct {
	// STT (speech-to-text) — provider is always Whisper API (OpenAI-compatible)
	STTEndpoint string `toml:"stt_endpoint"` // default: Groq
	STTModel    string `toml:"stt_model"`    // default: whisper-large-v3

	// TTS (text-to-speech) — configurable provider
	TTSProvider string  `toml:"tts_provider"` // "edge-tts" (default) or "openai"
	TTSEndpoint string  `toml:"tts_endpoint"` // for openai provider
	TTSModel    string  `toml:"tts_model"`    // for openai provider, e.g. "openai/tts-1-mini"
	TTSVoice    string  `toml:"tts_voice"`    // voice name (provider-specific)
	TTSRate     float64 `toml:"tts_rate"`     // speech rate multiplier: 1.0 = normal, 1.3 = 30% faster, 0.8 = 20% slower

	// WebSocket voice endpoint
	WSEnabled bool `toml:"ws_enabled"` // enable /voice WebSocket endpoint (default false)
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
}

type ManaWarningsConfig struct {
	Name       string `toml:"name"`       // what to call quota (default "mana")
	Thresholds []int  `toml:"thresholds"` // mana percentages to warn at (e.g. [50, 25, 10, 5])
}

type EnvironmentConfig struct {
	Enabled  bool   `toml:"enabled"`   // inject environment block as first system block (default true)
	DocsPath string `toml:"docs_path"` // path to platform docs directory; relative paths resolve against $HOME
}

type SkillsConfig struct {
	Dirs []string `toml:"dirs"` // directories to scan for skill subdirectories
}

type ResourcesConfig struct {
	MemoryGuardEnabled          bool    `toml:"memory_guard_enabled"`           // enable system memory guard (default true)
	MemoryGuardInterval         string  `toml:"memory_guard_interval"`          // check interval (default "60s")
	MemoryWarnPercent           int     `toml:"memory_warn_percent"`            // warn threshold as % of total RAM (default 25)
	MemoryKillPercent           int     `toml:"memory_kill_percent"`            // kill threshold as % of total RAM (default 40)
	MemoryPressureThreshold     float64 `toml:"memory_pressure_threshold"`      // PSI avg10 threshold to require before acting (default 10.0)
}

type ToolsConfig struct {
	MaxResultChars          int    `toml:"max_result_chars"`           // max chars before writing result to file (default 15000)
	TempDir                 string `toml:"temp_dir"`                   // where to write large tool results (default /tmp/foci-tool-results)
	TmuxCols                int    `toml:"tmux_cols"`                  // tmux window columns on start (default 300)
	TmuxRows                int    `toml:"tmux_rows"`                  // tmux window rows on start (default 30)
	ExecAutoBackground      int    `toml:"exec_auto_background"`       // seconds before auto-backgrounding exec (default 10, 0 disables)
	ExecDefaultTimeout      int    `toml:"exec_default_timeout"`       // default timeout for exec commands in seconds (default 30)
	ExecMaxOutputChars      int    `toml:"exec_max_output_chars"`      // max chars in exec output before truncation (default 100000)
	TmuxCommandTimeout      string `toml:"tmux_command_timeout"`       // timeout for tmux control commands (default "5s")
	WebFetchTimeout         string `toml:"web_fetch_timeout"`          // HTTP timeout for web fetch (default "30s")
	WebFetchMaxBytes        int    `toml:"web_fetch_max_bytes"`        // max bytes to read from web fetch (default 1048576 = 1MB)
	WebFetchMaxChars        int    `toml:"web_fetch_max_chars"`        // max chars in web fetch output before truncation (default 50000)
	WebSearchTimeout        string `toml:"web_search_timeout"`         // HTTP timeout for web search (default "15s")
	MaxConcurrentSpawns     int    `toml:"max_concurrent_spawns"`      // max concurrent spawn inherit sessions per agent (default 3)
	ToolCallPreviewChars    int    `toml:"tool_call_preview_chars"`    // max chars for tool call param preview in Telegram (default 450)
	TmuxMemoryCheckInterval string `toml:"tmux_memory_check_interval"` // how often to check tmux RSS (default "5m", "0" disables)
	TmuxMemoryWarn          string `toml:"tmux_memory_warn"`           // warn threshold as % of RAM or absolute (default "10%")
	TmuxMemoryCritical      string `toml:"tmux_memory_critical"`       // critical threshold (default "20%")
	TmuxMemoryKill          string `toml:"tmux_memory_kill"`           // kill threshold (default "30%")
	TmuxBraindead           bool   `toml:"tmux_braindead"`             // auto-unwatch on inactivity, auto-watch on send (default true)
	TmuxWatchThreshold      string `toml:"tmux_watch_threshold"`       // default watch threshold duration (default "30s")
	MaxUploadFileSize       int64  `toml:"max_upload_file_size"`       // max file size for multipart uploads in bytes (default 52428800 = 50MB)
	SummaryContextTurns        int      `toml:"summary_context_turns"`         // recent turns for auto-summary context (default 5)
	SummaryContextChars        int      `toml:"summary_context_chars"`         // max chars of context for auto-summary (default 6000)
	SearchProvider             string   `toml:"search_provider"`               // "anthropic" (default) or "brave"
	FetchProvider              string   `toml:"fetch_provider"`                // "anthropic" (default) or "builtin"
	WebSearchMaxUses           int      `toml:"web_search_max_uses"`           // max searches per API call (0 = unlimited)
	WebSearchAllowedDomains    []string `toml:"web_search_allowed_domains"`    // domain whitelist (mutually exclusive with blocked)
	WebSearchBlockedDomains    []string `toml:"web_search_blocked_domains"`    // domain blacklist
	WebFetchMaxUses            int      `toml:"web_fetch_max_uses"`            // max fetches per API call (0 = unlimited)
	WebFetchAllowedDomains     []string `toml:"web_fetch_allowed_domains"`     // domain whitelist
	WebFetchBlockedDomains     []string `toml:"web_fetch_blocked_domains"`     // domain blacklist
}

type PromptRule struct {
	Find    string `toml:"find"`    // regex pattern to match
	Replace string `toml:"replace"` // replacement string (supports $1, $2, etc.)
}

type CommandConfig struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Script      string `toml:"script"`
	Timeout     int    `toml:"timeout"` // seconds, default 10
}

// DefaultsConfig provides global defaults for agent-specific fields.
// Agents inherit these unless they override them explicitly.
type DefaultsConfig struct {
	Model               string           `toml:"model"`                 // default model (default: claude-haiku-4-5)
	DuplicateMessages   bool             `toml:"duplicate_messages"`    // default duplicate_messages (default: false)
	InjectAgentWarnings bool             `toml:"inject_agent_warnings"` // default inject_agent_warnings (default: false)
	MaxToolLoops        int              `toml:"max_tool_loops"`        // default max_tool_loops (default: 25)
	MaxOutputTokens     int              `toml:"max_output_tokens"`     // default max_output_tokens (default: 8192)
	BraindeadThreshold  int              `toml:"braindead_threshold"`   // default braindead threshold (default: 10)
	BraindeadPrompt     string           `toml:"braindead_prompt"`      // default braindead prompt
	Effort              string           `toml:"effort"`                // default effort level: "low", "medium", "high" (empty = omit)
	Thinking            string           `toml:"thinking"`              // default thinking mode: "off" (default), "adaptive"
	TTSRate             float64          `toml:"tts_rate"`              // default TTS speech rate (default: 0 = voice config)
	ShowToolCalls       *ToolCallDisplay `toml:"show_tool_calls"`       // default show_tool_calls (nil = use telegram.show_tool_calls)
	ShowThinking        *ShowThinking    `toml:"show_thinking"`         // default show_thinking (nil = use telegram.show_thinking)
	DisplayWidth        *int             `toml:"display_width"`         // default display_width (nil = use telegram.display_width)
	SystemFiles         []string         `toml:"system_files"`          // default system file list
	CompactionEffort    string           `toml:"compaction_effort"`     // default compaction effort (empty = use session effort)
	SummaryContextTurns int              `toml:"summary_context_turns"` // default summary_context_turns (default 5)
	SummaryContextChars int              `toml:"summary_context_chars"` // default summary_context_chars (default 6000)
	SearchProvider      string           `toml:"search_provider"`       // default search provider: "anthropic" (default) or "brave"
	FetchProvider       string           `toml:"fetch_provider"`        // default fetch provider: "anthropic" (default) or "builtin"
}

// ModelsConfig holds model-related configuration.
type ModelsConfig struct {
	Aliases map[string]string `toml:"aliases"` // shorthand → full model ID (e.g., "opus" → "claude-opus-4-6")
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
}

// BackgroundConfig controls the mana-gated background work timer.
type BackgroundConfig struct {
	Enabled        bool   `toml:"enabled"`         // enable background work timer (default: false)
	Interval       string `toml:"interval"`        // time since last interaction before firing (default: "5m")
	Prompt         string `toml:"prompt"`          // prompt file path ("" = embedded default, "none" = disabled, "default" = embedded)
	InvestInterval string `toml:"invest_interval"` // quiet period after mana reset to let cache invest (default: "30m")
}

type Config struct {
	DataDir            string                `toml:"data_dir"` // directory for databases, sessions, state (default: $HOME/data)
	Defaults           DefaultsConfig        `toml:"defaults"` // global defaults for agent-specific fields
	Models             ModelsConfig          `toml:"models"`   // model aliases and related config
	Agent              AgentConfig           `toml:"agent"`    // legacy: single agent
	Agents             []AgentConfig         `toml:"agents"`   // multi-agent: array of agents
	Anthropic          AnthropicConfig       `toml:"anthropic"`
	Telegram           TelegramConfig        `toml:"telegram"`
	Sessions           SessionsConfig        `toml:"sessions"`
	Memory             MemoryConfig          `toml:"memory"`
	Database           DatabaseConfig        `toml:"database"`
	HTTP               HTTPConfig            `toml:"http"`
	Logging            LoggingConfig         `toml:"logging"`
	Voice              VoiceConfig           `toml:"voice"`
	Bitwarden          BitwardenConfig       `toml:"bitwarden"`
	Cache              CacheConfig           `toml:"cache"`
	ManaWarnings       ManaWarningsConfig    `toml:"usage_warnings"`
	Environment        EnvironmentConfig     `toml:"environment"`
	Skills             SkillsConfig          `toml:"skills"`
	Resources          ResourcesConfig       `toml:"resources"`
	Tools              ToolsConfig           `toml:"tools"`
	Keepalive          KeepaliveConfig       `toml:"keepalive"`
	Background         BackgroundConfig      `toml:"background"`
	MemoryFormation    MemoryFormationConfig `toml:"memory_formation"`
	Commands           []CommandConfig       `toml:"commands"`
	PromptRules        []PromptRule          `toml:"prompt_rules"`         // regex find/replace rules applied to inbound messages
	WelcomeFile        string                `toml:"welcome_file"`         // path to welcome/changelog file injected on startup (e.g. /home/foci/WELCOME.md)
	SkipSecurityChecks bool                  `toml:"skip_security_checks"` // if true, skip startup security checks for secrets.toml
	DefinedKeys        map[string]bool       `toml:"-"`                    // keys explicitly set in TOML file (populated by Load)
}

// validate checks semantic validity of config values after parsing and defaults.
// Returns an error describing the first invalid value found.
func validate(cfg *Config) error {
	// Sessions
	if cfg.Sessions.CompactionThreshold < 0 || cfg.Sessions.CompactionThreshold > 1.0 {
		return fmt.Errorf("[sessions] compaction_threshold = %g: must be between 0.0 and 1.0", cfg.Sessions.CompactionThreshold)
	}
	if cfg.Sessions.CompactionMaxTokens < 0 {
		return fmt.Errorf("[sessions] compaction_max_tokens = %d: must not be negative", cfg.Sessions.CompactionMaxTokens)
	}
	if cfg.Sessions.CompactionMinMessages < 0 {
		return fmt.Errorf("[sessions] compaction_min_messages = %d: must not be negative", cfg.Sessions.CompactionMinMessages)
	}
	if cfg.Sessions.CompactionPreserveMessages < 0 {
		return fmt.Errorf("[sessions] compaction_preserve_messages = %d: must not be negative", cfg.Sessions.CompactionPreserveMessages)
	}

	// HTTP
	if cfg.HTTP.Port < 1 || cfg.HTTP.Port > 65535 {
		return fmt.Errorf("[http] port = %d: must be between 1 and 65535", cfg.HTTP.Port)
	}

	// Logging
	validLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	levelUpper := strings.ToUpper(strings.TrimSpace(cfg.Logging.Level))
	if !validLevels[levelUpper] {
		return fmt.Errorf("[logging] level = %q: must be one of DEBUG, INFO, WARN, ERROR", cfg.Logging.Level)
	}
	if _, err := time.ParseDuration(cfg.Logging.WarningWindowDuration); err != nil {
		return fmt.Errorf("[logging] warning_window_duration = %q: %w", cfg.Logging.WarningWindowDuration, err)
	}
	if _, err := time.ParseDuration(cfg.Logging.WarningProactiveActiveInterval); err != nil {
		return fmt.Errorf("[logging] warning_proactive_active_interval = %q: %w", cfg.Logging.WarningProactiveActiveInterval, err)
	}
	if _, err := time.ParseDuration(cfg.Logging.WarningProactiveInactiveInterval); err != nil {
		return fmt.Errorf("[logging] warning_proactive_inactive_interval = %q: %w", cfg.Logging.WarningProactiveInactiveInterval, err)
	}
	if _, err := time.ParseDuration(cfg.Logging.WarningProactiveActivityThreshold); err != nil {
		return fmt.Errorf("[logging] warning_proactive_activity_threshold = %q: %w", cfg.Logging.WarningProactiveActivityThreshold, err)
	}
	if _, err := time.ParseDuration(cfg.Logging.RotationPeriod); err != nil {
		return fmt.Errorf("[logging] rotation_period = %q: %w", cfg.Logging.RotationPeriod, err)
	}
	if _, err := time.ParseDuration(cfg.Logging.RetentionPeriod); err != nil {
		return fmt.Errorf("[logging] retention_period = %q: %w", cfg.Logging.RetentionPeriod, err)
	}
	if _, err := ParseByteSize(cfg.Logging.RotationMaxLineSize); err != nil {
		return fmt.Errorf("[logging] rotation_max_line_size = %q: %w", cfg.Logging.RotationMaxLineSize, err)
	}

	// Bitwarden
	if cfg.Bitwarden.Enabled {
		if _, err := time.ParseDuration(cfg.Bitwarden.RefreshInterval); err != nil {
			return fmt.Errorf("[bitwarden] refresh_interval = %q: %w", cfg.Bitwarden.RefreshInterval, err)
		}
		if _, err := time.ParseDuration(cfg.Bitwarden.SecretTTL); err != nil {
			return fmt.Errorf("[bitwarden] secret_ttl = %q: %w", cfg.Bitwarden.SecretTTL, err)
		}
		if _, err := time.ParseDuration(cfg.Bitwarden.CleanupInterval); err != nil {
			return fmt.Errorf("[bitwarden] cleanup_interval = %q: %w", cfg.Bitwarden.CleanupInterval, err)
		}
	}

	// Cache
	validStrategies := map[string]bool{"auto": true, "explicit": true}
	if !validStrategies[cfg.Cache.Strategy] {
		return fmt.Errorf("[cache] strategy = %q: must be \"auto\" or \"explicit\"", cfg.Cache.Strategy)
	}

	// Memory sources
	for i, src := range cfg.Memory.Sources {
		if src.Weight < 0 || src.Weight > 1.0 {
			return fmt.Errorf("[memory] sources[%d] (%s) weight = %g: must be between 0.0 and 1.0", i, src.Name, src.Weight)
		}
	}
	if cfg.Memory.ConversationWeight < 0 || cfg.Memory.ConversationWeight > 1.0 {
		return fmt.Errorf("[memory] conversation_weight = %g: must be between 0.0 and 1.0", cfg.Memory.ConversationWeight)
	}

	// Mana warnings thresholds
	for i, t := range cfg.ManaWarnings.Thresholds {
		if t < 0 || t > 100 {
			return fmt.Errorf("[usage_warnings] thresholds[%d] = %d: must be between 0 and 100", i, t)
		}
	}
	for _, a := range cfg.Agents {
		for i, t := range a.UsageWarnings.Thresholds {
			if t < 0 || t > 100 {
				return fmt.Errorf("agent %q [usage_warnings] thresholds[%d] = %d: must be between 0 and 100", a.ID, i, t)
			}
		}
	}

	// Database
	if _, err := time.ParseDuration(cfg.Database.BusyTimeout); err != nil {
		return fmt.Errorf("[database] busy_timeout = %q: %w", cfg.Database.BusyTimeout, err)
	}

	// Anthropic
	if _, err := time.ParseDuration(cfg.Anthropic.HTTPTimeout); err != nil {
		return fmt.Errorf("[anthropic] http_timeout = %q: %w", cfg.Anthropic.HTTPTimeout, err)
	}
	if _, err := time.ParseDuration(cfg.Anthropic.UsageAPITimeout); err != nil {
		return fmt.Errorf("[anthropic] usage_api_timeout = %q: %w", cfg.Anthropic.UsageAPITimeout, err)
	}

	// Tools
	if _, err := time.ParseDuration(cfg.Tools.TmuxCommandTimeout); err != nil {
		return fmt.Errorf("[tools] tmux_command_timeout = %q: %w", cfg.Tools.TmuxCommandTimeout, err)
	}
	if _, err := time.ParseDuration(cfg.Tools.WebFetchTimeout); err != nil {
		return fmt.Errorf("[tools] web_fetch_timeout = %q: %w", cfg.Tools.WebFetchTimeout, err)
	}
	if _, err := time.ParseDuration(cfg.Tools.WebSearchTimeout); err != nil {
		return fmt.Errorf("[tools] web_search_timeout = %q: %w", cfg.Tools.WebSearchTimeout, err)
	}
	if cfg.Tools.TmuxMemoryCheckInterval != "0" {
		if _, err := time.ParseDuration(cfg.Tools.TmuxMemoryCheckInterval); err != nil {
			return fmt.Errorf("[tools] tmux_memory_check_interval = %q: %w", cfg.Tools.TmuxMemoryCheckInterval, err)
		}
	}
	for _, kv := range []struct{ k, v string }{
		{"tmux_memory_warn", cfg.Tools.TmuxMemoryWarn},
		{"tmux_memory_critical", cfg.Tools.TmuxMemoryCritical},
		{"tmux_memory_kill", cfg.Tools.TmuxMemoryKill},
	} {
		if err := ValidateMemoryThreshold(kv.v); err != nil {
			return fmt.Errorf("[tools] %s = %q: %w", kv.k, kv.v, err)
		}
	}

	// Resources
	if _, err := time.ParseDuration(cfg.Resources.MemoryGuardInterval); err != nil {
		return fmt.Errorf("[resources] memory_guard_interval = %q: %w", cfg.Resources.MemoryGuardInterval, err)
	}
	if cfg.Resources.MemoryWarnPercent < 0 || cfg.Resources.MemoryWarnPercent > 100 {
		return fmt.Errorf("[resources] memory_warn_percent = %d: must be between 0 and 100", cfg.Resources.MemoryWarnPercent)
	}
	if cfg.Resources.MemoryKillPercent < 0 || cfg.Resources.MemoryKillPercent > 100 {
		return fmt.Errorf("[resources] memory_kill_percent = %d: must be between 0 and 100", cfg.Resources.MemoryKillPercent)
	}
	if cfg.Resources.MemoryPressureThreshold < 0 {
		return fmt.Errorf("[resources] memory_pressure_threshold = %g: must not be negative", cfg.Resources.MemoryPressureThreshold)
	}

	// Telegram
	if _, err := time.ParseDuration(cfg.Telegram.LongPollTimeout); err != nil {
		return fmt.Errorf("[telegram] long_poll_timeout = %q: %w", cfg.Telegram.LongPollTimeout, err)
	}
	if _, err := time.ParseDuration(cfg.Telegram.MultiballSessionTTL); err != nil {
		return fmt.Errorf("[telegram] multiball_session_ttl = %q: %w", cfg.Telegram.MultiballSessionTTL, err)
	}

	// HTTP
	if _, err := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout); err != nil {
		return fmt.Errorf("[http] graceful_shutdown_timeout = %q: %w", cfg.HTTP.GracefulShutdownTimeout, err)
	}

	return nil
}

// boolKeyLineRe matches a TOML key = "on"/"off"/"true"/"false" line,
// capturing the key name, the equals sign, the quoted value, and trailing comment.
var boolKeyLineRe = regexp.MustCompile(`(?m)^(\s*(\w+)\s*=\s*)"(?i)(on|off|true|false)"(\s*(?:#.*)?)$`)

// boolKeys is the set of TOML keys that are bool-typed in the config structs.
// Only these keys have their quoted string values normalized to native bools.
var boolKeys = map[string]bool{
	"duplicate_messages":    true,
	"inject_agent_warnings": true,
	"startup_notification":  true,
	"messages_in_log":       true,
	"compaction_notify":     true,
	"compaction_debug":      true,
	"tmux_braindead":        true,
	"auto_refresh":          true,
	"enable_stop_aliases":   true,
	"enable_startup_notify": true,
	"full_payload":          true,
	"cache_bust_detect":     true,
	"log_rotation":          true,
	"ws_enabled":            true,
	"enabled":               true,
	"skip_security_checks":  true,
	"interval_enabled":      true,
	"consolidation_enabled": true,
	"session_end_enabled":   true,
}

// normalizeBoolStrings preprocesses TOML content to convert quoted bool-like
// strings ("on"/"off"/"true"/"false") to native TOML booleans for known bool
// keys. This allows users to write `enabled = "on"` as an alias for
// `enabled = true`. Only applies to keys in the boolKeys set — string fields
// like `thinking = "off"` are not affected.
func normalizeBoolStrings(data string) string {
	return boolKeyLineRe.ReplaceAllStringFunc(data, func(match string) string {
		sub := boolKeyLineRe.FindStringSubmatch(match)
		if len(sub) < 5 {
			return match
		}
		prefix := sub[1] // "  key = " including whitespace
		key := sub[2]    // the key name
		val := sub[3]    // on/off/true/false
		trail := sub[4]  // trailing comment

		if !boolKeys[key] {
			return match // not a bool key, leave as-is
		}

		switch strings.ToLower(val) {
		case "on", "true":
			return prefix + "true" + trail
		case "off", "false":
			return prefix + "false" + trail
		default:
			return match
		}
	})
}

// agentDefinedFields parses TOML metadata keys to determine which fields each
// [[agents]] array element explicitly defines. Returns a slice (one entry per
// agent) of sets of TOML field names.
func agentDefinedFields(md toml.MetaData) []map[string]bool {
	var result []map[string]bool
	var current map[string]bool

	for _, key := range md.Keys() {
		parts := []string(key)
		if len(parts) == 0 || parts[0] != "agents" {
			continue
		}
		if len(parts) == 1 {
			// Start of a new [[agents]] block
			current = make(map[string]bool)
			result = append(result, current)
			continue
		}
		if current != nil {
			current[parts[1]] = true
		}
	}
	return result
}

// applyDefaultsToAgent copies fields from defaults to agent where the agent
// field is zero-value and was not explicitly set in the TOML file.
// Fields are matched by TOML tag name between DefaultsConfig and AgentConfig.
func applyDefaultsToAgent(agent *AgentConfig, defaults *DefaultsConfig, defined map[string]bool) {
	dv := reflect.ValueOf(defaults).Elem()
	dt := dv.Type()
	av := reflect.ValueOf(agent).Elem()
	at := av.Type()

	// Build AgentConfig field index by TOML tag
	agentFieldByTag := make(map[string]int, at.NumField())
	for i := 0; i < at.NumField(); i++ {
		tag := at.Field(i).Tag.Get("toml")
		if tag != "" && tag != "-" {
			agentFieldByTag[tag] = i
		}
	}

	for i := 0; i < dt.NumField(); i++ {
		tag := dt.Field(i).Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}

		ai, ok := agentFieldByTag[tag]
		if !ok {
			continue // DefaultsConfig field has no matching AgentConfig field
		}

		af := av.Field(ai)
		df := dv.Field(i)

		// Skip if agent explicitly defined this field in TOML
		if defined[tag] {
			continue
		}

		// Skip if agent value is already non-zero
		if !af.IsZero() {
			continue
		}

		// Skip if default is also zero (nothing to copy)
		if df.IsZero() {
			continue
		}

		af.Set(df)
	}
}

// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	md, err := toml.Decode(normalizeBoolStrings(string(data)), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Check for unknown config keys and warn about them
	checkUnknownKeys(path, md)

	// Record which keys were explicitly set in the TOML file.
	cfg.DefinedKeys = make(map[string]bool)
	for _, key := range md.Keys() {
		cfg.DefinedKeys[strings.Join(key, ".")] = true
	}

	// Populate [defaults] section with hardcoded fallbacks
	if cfg.Defaults.Model == "" {
		cfg.Defaults.Model = "claude-haiku-4-5"
	}
	if cfg.Defaults.MaxToolLoops == 0 {
		cfg.Defaults.MaxToolLoops = 25
	}
	if cfg.Defaults.MaxOutputTokens == 0 {
		cfg.Defaults.MaxOutputTokens = 8192
	}
	if cfg.Defaults.BraindeadThreshold == 0 && !md.IsDefined("defaults", "braindead_threshold") {
		cfg.Defaults.BraindeadThreshold = 10
	}

	// Backward compat: [agent] (singular) → single-element Agents array
	if len(cfg.Agents) == 0 && cfg.Agent.ID != "" {
		cfg.Agents = []AgentConfig{cfg.Agent}
	}

	// Apply [defaults] to all agents (agent value > global default > hardcoded).
	// Uses reflect to iterate DefaultsConfig fields and copy to matching
	// AgentConfig fields when the agent value is zero and wasn't explicitly
	// set in the TOML file. This means adding new fields to DefaultsConfig
	// with matching TOML tags in AgentConfig "just works" — no new if-blocks.
	perAgentDefined := agentDefinedFields(md)
	for i := range cfg.Agents {
		var defined map[string]bool
		if i < len(perAgentDefined) {
			defined = perAgentDefined[i]
		}
		applyDefaultsToAgent(&cfg.Agents[i], &cfg.Defaults, defined)

		if cfg.Agents[i].BranchOrientationPrompt != "" {
			cfg.Agents[i].BranchOrientationPrompt = ResolvePath(cfg.Agents[i].BranchOrientationPrompt)
		}
	}

	// Keep cfg.Agent in sync (points to first agent for legacy code paths)
	if len(cfg.Agents) > 0 {
		cfg.Agent = cfg.Agents[0]
	}

	// Legacy agent defaults (in case nothing is configured at all)
	if cfg.Agent.Model == "" {
		cfg.Agent.Model = "claude-haiku-4-5"
	}

	// Model aliases defaults (if not configured)
	if len(cfg.Models.Aliases) == 0 {
		cfg.Models.Aliases = map[string]string{
			"opus":   "claude-opus-4-6",
			"sonnet": "claude-sonnet-4-6",
			"haiku":  "claude-haiku-4-5",
		}
	}

	if cfg.Sessions.CompactionThreshold == 0 {
		cfg.Sessions.CompactionThreshold = 0.8
	}
	if cfg.Sessions.CompactionMaxTokens == 0 {
		cfg.Sessions.CompactionMaxTokens = 4096
	}
	if cfg.Sessions.CompactionMinMessages == 0 {
		cfg.Sessions.CompactionMinMessages = 4
	}
	if cfg.Sessions.CompactionPreserveMessages == 0 && !md.IsDefined("sessions", "compaction_preserve_messages") {
		cfg.Sessions.CompactionPreserveMessages = 25
	}
	if cfg.HTTP.Port == 0 {
		cfg.HTTP.Port = 18791
	}
	if cfg.HTTP.Bind == "" {
		cfg.HTTP.Bind = "127.0.0.1"
	}
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, "data")
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "INFO"
	}
	if cfg.Logging.EventFile == "" {
		cfg.Logging.EventFile = "logs/foci.log"
	}
	if cfg.Logging.APIFile == "" {
		cfg.Logging.APIFile = "logs/api.jsonl"
	}
	if cfg.Logging.FullPayload && cfg.Logging.PayloadFile == "" {
		cfg.Logging.PayloadFile = "logs/api-payload.jsonl"
	}
	if cfg.Logging.CacheBustIdleMinutes == 0 && !md.IsDefined("logging", "cache_bust_idle_minutes") {
		cfg.Logging.CacheBustIdleMinutes = 10
	}
	if cfg.Logging.WarningMaxPerWindow == 0 && !md.IsDefined("logging", "warning_max_per_window") {
		cfg.Logging.WarningMaxPerWindow = 3
	}
	if cfg.Logging.WarningWindowDuration == "" {
		cfg.Logging.WarningWindowDuration = "5m"
	}
	if cfg.Logging.WarningProactiveActiveInterval == "" {
		cfg.Logging.WarningProactiveActiveInterval = "5m"
	}
	if cfg.Logging.WarningProactiveInactiveInterval == "" {
		cfg.Logging.WarningProactiveInactiveInterval = "1h"
	}
	if cfg.Logging.WarningProactiveActivityThreshold == "" {
		cfg.Logging.WarningProactiveActivityThreshold = "10m"
	}
	if !md.IsDefined("logging", "log_rotation") {
		cfg.Logging.LogRotation = true
	}
	if cfg.Logging.RotationPeriod == "" {
		cfg.Logging.RotationPeriod = "24h"
	}
	if cfg.Logging.RetentionPeriod == "" {
		cfg.Logging.RetentionPeriod = "48h"
	}
	if cfg.Logging.RotationMaxLineSize == "" {
		cfg.Logging.RotationMaxLineSize = "64MB"
	}
	// Resources defaults
	if !md.IsDefined("resources", "memory_guard_enabled") {
		cfg.Resources.MemoryGuardEnabled = true
	}
	if cfg.Resources.MemoryGuardInterval == "" {
		cfg.Resources.MemoryGuardInterval = "60s"
	}
	if cfg.Resources.MemoryWarnPercent == 0 && !md.IsDefined("resources", "memory_warn_percent") {
		cfg.Resources.MemoryWarnPercent = 25
	}
	if cfg.Resources.MemoryKillPercent == 0 && !md.IsDefined("resources", "memory_kill_percent") {
		cfg.Resources.MemoryKillPercent = 40
	}
	if cfg.Resources.MemoryPressureThreshold == 0 && !md.IsDefined("resources", "memory_pressure_threshold") {
		cfg.Resources.MemoryPressureThreshold = 10.0
	}
	// Bitwarden defaults
	if cfg.Bitwarden.SessionFile == "" {
		cfg.Bitwarden.SessionFile = "/home/bitwarden/.bw_session"
	}
	if cfg.Bitwarden.RefreshInterval == "" {
		cfg.Bitwarden.RefreshInterval = "15m"
	}
	if cfg.Bitwarden.SecretTTL == "" {
		cfg.Bitwarden.SecretTTL = "30m"
	}
	if cfg.Bitwarden.CleanupInterval == "" {
		cfg.Bitwarden.CleanupInterval = "1m"
	}

	if cfg.Cache.Strategy == "" {
		cfg.Cache.Strategy = "auto"
	}
	if cfg.ManaWarnings.Name == "" {
		cfg.ManaWarnings.Name = "mana"
	}
	if cfg.Tools.MaxResultChars == 0 {
		cfg.Tools.MaxResultChars = 15000
	}
	if cfg.Tools.TempDir == "" {
		cfg.Tools.TempDir = "/tmp/foci-tool-results"
	}
	if cfg.Tools.TmuxCols == 0 {
		cfg.Tools.TmuxCols = 300
	}
	if cfg.Tools.TmuxRows == 0 {
		cfg.Tools.TmuxRows = 30
	}
	if cfg.Tools.ExecAutoBackground == 0 && !md.IsDefined("tools", "exec_auto_background") {
		cfg.Tools.ExecAutoBackground = 10
	}
	if !md.IsDefined("tools", "tmux_braindead") {
		cfg.Tools.TmuxBraindead = true
	}
	if cfg.Tools.TmuxWatchThreshold == "" {
		cfg.Tools.TmuxWatchThreshold = "30s"
	}
	if cfg.Tools.SearchProvider == "" {
		cfg.Tools.SearchProvider = "anthropic"
	}
	if cfg.Tools.FetchProvider == "" {
		cfg.Tools.FetchProvider = "anthropic"
	}
	if len(cfg.Telegram.StopAliases) == 0 {
		cfg.Telegram.StopAliases = []string{"stop", "wait"}
	}
	if cfg.WelcomeFile == "" {
		cfg.WelcomeFile = "data/WELCOME.md"
	}
	if cfg.Memory.ConversationWeight == 0 {
		cfg.Memory.ConversationWeight = 0.1
	}
	if cfg.Memory.SearchLimit == 0 {
		cfg.Memory.SearchLimit = 20
	}

	// Database defaults
	if cfg.Database.BusyTimeout == "" {
		cfg.Database.BusyTimeout = "5s"
	}

	// Anthropic defaults
	if cfg.Anthropic.HTTPTimeout == "" {
		cfg.Anthropic.HTTPTimeout = "600s" // 10 min — thinking responses can take several minutes
	}
	if cfg.Anthropic.UsageAPITimeout == "" {
		cfg.Anthropic.UsageAPITimeout = "10s"
	}

	// Tools defaults
	if cfg.Tools.ExecDefaultTimeout == 0 {
		cfg.Tools.ExecDefaultTimeout = 30
	}
	if cfg.Tools.ExecMaxOutputChars == 0 {
		cfg.Tools.ExecMaxOutputChars = 100000
	}
	if cfg.Tools.TmuxCommandTimeout == "" {
		cfg.Tools.TmuxCommandTimeout = "5s"
	}
	if cfg.Tools.WebFetchTimeout == "" {
		cfg.Tools.WebFetchTimeout = "30s"
	}
	if cfg.Tools.WebFetchMaxBytes == 0 {
		cfg.Tools.WebFetchMaxBytes = 1048576 // 1MB
	}
	if cfg.Tools.WebFetchMaxChars == 0 {
		cfg.Tools.WebFetchMaxChars = 50000
	}
	if cfg.Tools.WebSearchTimeout == "" {
		cfg.Tools.WebSearchTimeout = "15s"
	}
	if cfg.Tools.MaxConcurrentSpawns == 0 {
		cfg.Tools.MaxConcurrentSpawns = 3
	}
	if cfg.Tools.MaxUploadFileSize == 0 {
		cfg.Tools.MaxUploadFileSize = 50 * 1024 * 1024 // 50MB
	}
	if cfg.Tools.ToolCallPreviewChars == 0 {
		cfg.Tools.ToolCallPreviewChars = 450
	}
	if cfg.Tools.TmuxMemoryCheckInterval == "" {
		cfg.Tools.TmuxMemoryCheckInterval = "5m"
	}
	if cfg.Tools.TmuxMemoryWarn == "" {
		cfg.Tools.TmuxMemoryWarn = "10%"
	}
	if cfg.Tools.TmuxMemoryCritical == "" {
		cfg.Tools.TmuxMemoryCritical = "20%"
	}
	if cfg.Tools.TmuxMemoryKill == "" {
		cfg.Tools.TmuxMemoryKill = "30%"
	}
	if cfg.Tools.SummaryContextTurns == 0 {
		cfg.Tools.SummaryContextTurns = 5
	}
	if cfg.Tools.SummaryContextChars == 0 {
		cfg.Tools.SummaryContextChars = 6000
	}

	// Telegram defaults
	if cfg.Telegram.MessageQueueSize == 0 {
		cfg.Telegram.MessageQueueSize = 64
	}
	if cfg.Telegram.LongPollTimeout == "" {
		cfg.Telegram.LongPollTimeout = "65s"
	}
	if cfg.Telegram.MultiballSessionTTL == "" {
		cfg.Telegram.MultiballSessionTTL = "60m"
	}

	// HTTP defaults
	if cfg.HTTP.GracefulShutdownTimeout == "" {
		cfg.HTTP.GracefulShutdownTimeout = "30s"
	}

	// Bool defaults: default to true unless explicitly set to false in config.
	// We use md.IsDefined because Go's zero value for bool is false,
	// so we can't distinguish "not set" from "set to false" otherwise.
	if !md.IsDefined("environment", "enabled") {
		cfg.Environment.Enabled = true
	}
	if !md.IsDefined("telegram", "enable_stop_aliases") {
		cfg.Telegram.EnableStopAliases = true
	}
	if !md.IsDefined("telegram", "enable_startup_notify") {
		cfg.Telegram.EnableStartupNotify = true
	}
	if !md.IsDefined("telegram", "show_tool_calls") {
		cfg.Telegram.ShowToolCalls = ToolCallOff
	}
	if !md.IsDefined("telegram", "show_thinking") {
		cfg.Telegram.ShowThinking = ShowThinkingOff
	}
	if cfg.Telegram.DisplayWidth == 0 {
		cfg.Telegram.DisplayWidth = 44
	}

	// Keepalive/background defaults
	if cfg.Keepalive.Interval == "" {
		cfg.Keepalive.Interval = "55m"
	}
	// Keepalive.Prompt: empty = use embedded default (via prompts.ResolvePrompt)
	if cfg.Background.Interval == "" {
		cfg.Background.Interval = "5m"
	}
	// Background.Prompt: empty = use embedded default (via prompts.ResolvePrompt)
	if cfg.Background.InvestInterval == "" {
		cfg.Background.InvestInterval = "30m"
	}

	// Memory formation defaults
	if cfg.MemoryFormation.Interval == "" {
		cfg.MemoryFormation.Interval = "1h"
	}
	if cfg.MemoryFormation.ConsolidationInterval == "" {
		cfg.MemoryFormation.ConsolidationInterval = "20h"
	}
	// IntervalEnabled/ConsolidationEnabled/SessionEndEnabled: nil = true (resolved at runtime)

	// Per-agent keepalive/background: inherit from global when not set per-agent.
	// If no fields were defined (zero-value struct), copy the entire global config.
	// If any fields were defined, fill gaps from global defaults.
	for i := range cfg.Agents {
		ka := &cfg.Agents[i].Keepalive
		if *ka == (KeepaliveConfig{}) {
			cfg.Agents[i].Keepalive = cfg.Keepalive
		} else {
			if ka.Interval == "" {
				ka.Interval = cfg.Keepalive.Interval
			}
			if ka.Prompt == "" {
				ka.Prompt = cfg.Keepalive.Prompt
			}
		}
		bg := &cfg.Agents[i].Background
		if *bg == (BackgroundConfig{}) {
			cfg.Agents[i].Background = cfg.Background
		} else {
			if bg.Interval == "" {
				bg.Interval = cfg.Background.Interval
			}
			if bg.Prompt == "" {
				bg.Prompt = cfg.Background.Prompt
			}
			if bg.InvestInterval == "" {
				bg.InvestInterval = cfg.Background.InvestInterval
			}
		}
		// Memory formation cascade
		mf := &cfg.Agents[i].MemoryFormation
		if *mf == (MemoryFormationConfig{}) {
			cfg.Agents[i].MemoryFormation = cfg.MemoryFormation
		} else {
			if mf.IntervalEnabled == nil && cfg.MemoryFormation.IntervalEnabled != nil {
				mf.IntervalEnabled = cfg.MemoryFormation.IntervalEnabled
			}
			if mf.Interval == "" {
				mf.Interval = cfg.MemoryFormation.Interval
			}
			if mf.IntervalPrompt == "" {
				mf.IntervalPrompt = cfg.MemoryFormation.IntervalPrompt
			}
			if mf.ConsolidationEnabled == nil && cfg.MemoryFormation.ConsolidationEnabled != nil {
				mf.ConsolidationEnabled = cfg.MemoryFormation.ConsolidationEnabled
			}
			if mf.ConsolidationInterval == "" {
				mf.ConsolidationInterval = cfg.MemoryFormation.ConsolidationInterval
			}
			if mf.ConsolidationPrompt == "" {
				mf.ConsolidationPrompt = cfg.MemoryFormation.ConsolidationPrompt
			}
			if mf.SessionEndEnabled == nil && cfg.MemoryFormation.SessionEndEnabled != nil {
				mf.SessionEndEnabled = cfg.MemoryFormation.SessionEndEnabled
			}
			if mf.SessionEndPrompt == "" {
				mf.SessionEndPrompt = cfg.MemoryFormation.SessionEndPrompt
			}
		}
		// ShowToolCalls: defaults.show_tool_calls → agent fallback
		if cfg.Agents[i].ShowToolCalls == nil && cfg.Defaults.ShowToolCalls != nil {
			cfg.Agents[i].ShowToolCalls = cfg.Defaults.ShowToolCalls
		}
		// ShowThinking: defaults.show_thinking → agent fallback
		if cfg.Agents[i].ShowThinking == nil && cfg.Defaults.ShowThinking != nil {
			cfg.Agents[i].ShowThinking = cfg.Defaults.ShowThinking
		}
		// DisplayWidth: defaults.display_width → agent fallback
		if cfg.Agents[i].DisplayWidth == nil && cfg.Defaults.DisplayWidth != nil {
			cfg.Agents[i].DisplayWidth = cfg.Defaults.DisplayWidth
		}
	}

	// Apply convention-based defaults before path resolution.
	for i := range cfg.Agents {
		// Workspace default: $HOME/$id
		if cfg.Agents[i].Workspace == "" {
			home, _ := os.UserHomeDir()
			cfg.Agents[i].Workspace = filepath.Join(home, cfg.Agents[i].ID)
		}
		// TelegramBot default: agent ID if a matching bot key exists
		if cfg.Agents[i].TelegramBot == "" {
			if _, ok := cfg.Telegram.Bots[cfg.Agents[i].ID]; ok {
				cfg.Agents[i].TelegramBot = cfg.Agents[i].ID
			}
		}
		// ReceivedFilesDir default: $workspace/received_files
		if cfg.Agents[i].ReceivedFilesDir == "" {
			cfg.Agents[i].ReceivedFilesDir = filepath.Join(cfg.Agents[i].Workspace, "received_files")
		}
		// Name default: capitalised ID (e.g. "clutch" → "Clutch")
		if cfg.Agents[i].Name == "" && cfg.Agents[i].ID != "" {
			r := []rune(cfg.Agents[i].ID)
			r[0] = unicode.ToUpper(r[0])
			cfg.Agents[i].Name = string(r)
		}
		// Memory sources default: single source at $workspace/memory
		if len(cfg.Agents[i].Memory.Sources) == 0 {
			cfg.Agents[i].Memory.Sources = []MemorySource{{
				Name:   cfg.Agents[i].ID,
				Dir:    filepath.Join(cfg.Agents[i].Workspace, "memory"),
				Weight: 1.0,
			}}
		}
	}

	// Bot token_secret default: "telegram.<bot-key-name>"
	for name, bot := range cfg.Telegram.Bots {
		if bot.TokenSecret == "" {
			bot.TokenSecret = "telegram." + name
			cfg.Telegram.Bots[name] = bot
		}
	}

	cfg.ResolveAllPaths()

	// Keepalive/background validation warnings
	if cfg.Background.Enabled && cfg.Keepalive.Enabled {
		bgInt, _ := time.ParseDuration(cfg.Background.Interval)
		kaInt, _ := time.ParseDuration(cfg.Keepalive.Interval)
		if bgInt > 0 && kaInt > 0 && bgInt > kaInt {
			log.Warnf("config", "[background] interval %s > [keepalive] interval %s — keepalive resets cache timer, background work may never trigger", cfg.Background.Interval, cfg.Keepalive.Interval)
		}
	}
	if cfg.Keepalive.Enabled {
		kaInt, _ := time.ParseDuration(cfg.Keepalive.Interval)
		if kaInt > time.Hour {
			log.Warnf("config", "[keepalive] interval %s > 1h — Anthropic cache TTL is 1 hour, cache may expire between keepalives", cfg.Keepalive.Interval)
		}
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// SecretGetter is the interface main.go uses to look up secrets.
type SecretGetter interface {
	Get(key string) (string, bool)
}

// ResolveBotToken resolves a Telegram bot token for the given bot name.
// It checks the [telegram.bots] map first (token_secret → secrets store),
// then falls back to the legacy telegram.bot_token path.
func (c *Config) ResolveBotToken(botName string, secrets SecretGetter) string {
	// New path: [telegram.bots.<name>].token_secret → secrets store
	if bot, ok := c.Telegram.Bots[botName]; ok && bot.TokenSecret != "" {
		if v, ok := secrets.Get(bot.TokenSecret); ok {
			return v
		}
	}

	// Legacy path: [telegram].bot_token or secrets.telegram.bot_token
	if v, ok := secrets.Get("telegram.bot_token"); ok {
		return v
	}
	return c.Telegram.BotToken
}

// ResolvePath resolves a path. Absolute paths are returned as-is.
// Relative paths are resolved against os.UserHomeDir().
func ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warnf("config", "resolve home dir for %q: %v", p, err)
		return p
	}
	return filepath.Join(home, p)
}

// DataPath resolves the path for a data file (database, state, etc.).
// If DataDir is set, the file is placed there (resolved via ResolvePath).
// Otherwise, defaults to $HOME/data/<filename>.
func (c *Config) DataPath(filename string) string {
	if c.DataDir != "" {
		return filepath.Join(ResolvePath(c.DataDir), filename)
	}
	return filepath.Join(ResolvePath("data"), filename)
}

// ResolveAllPaths resolves all path config fields in one place.
// Called at the end of Load(), before validate().
func (c *Config) ResolveAllPaths() {
	c.Logging.EventFile = ResolvePath(c.Logging.EventFile)
	c.Logging.APIFile = ResolvePath(c.Logging.APIFile)
	if c.Logging.PayloadFile != "" {
		c.Logging.PayloadFile = ResolvePath(c.Logging.PayloadFile)
	}
	if c.Logging.ArchiveDir != "" {
		c.Logging.ArchiveDir = ResolvePath(c.Logging.ArchiveDir)
	}
	if c.Logging.ConversationFile == "" {
		c.Logging.ConversationFile = c.DataPath("conversation.db")
	} else {
		c.Logging.ConversationFile = ResolvePath(c.Logging.ConversationFile)
	}
	if c.Sessions.Dir == "" {
		c.Sessions.Dir = c.DataPath("sessions")
	} else {
		c.Sessions.Dir = ResolvePath(c.Sessions.Dir)
	}
	if c.Sessions.BranchOrientationPrompt != "" {
		c.Sessions.BranchOrientationPrompt = ResolvePath(c.Sessions.BranchOrientationPrompt)
	}
	if c.Sessions.CompactionSummaryPrompt != "" {
		c.Sessions.CompactionSummaryPrompt = ResolvePath(c.Sessions.CompactionSummaryPrompt)
	}
	// Keepalive.Prompt and Background.Prompt: path resolution handled by prompts.ResolvePrompt at runtime.
	c.WelcomeFile = ResolvePath(c.WelcomeFile)
	if c.Environment.DocsPath != "" {
		c.Environment.DocsPath = ResolvePath(c.Environment.DocsPath)
	}
	if c.Telegram.ReceivedFilesDir != "" {
		c.Telegram.ReceivedFilesDir = ResolvePath(c.Telegram.ReceivedFilesDir)
	}
	for i := range c.Agents {
		if c.Agents[i].ReceivedFilesDir != "" {
			c.Agents[i].ReceivedFilesDir = ResolvePath(c.Agents[i].ReceivedFilesDir)
		}
	}
}

// ParseFlags returns the config file path from command-line flags.
func ParseFlags() string {
	path := flag.String("config", "foci.toml", "path to config file")
	flag.Parse()
	return *path
}

// UnknownKeys returns the list of unrecognised key names from the TOML metadata.
// Exported for testing; Load() calls this internally and logs warnings.
func UnknownKeys(md toml.MetaData) []string {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, key := range undecoded {
		keys[i] = strings.Join(key, ".")
	}
	return keys
}

func checkUnknownKeys(path string, md toml.MetaData) {
	keys := UnknownKeys(md)
	if len(keys) == 0 {
		return
	}
	log.Warnf("config", "unknown config keys in %s: %v", path, keys)
}

// ValidateMemoryThreshold checks that a memory threshold string is in a valid
// format: "N%" (percentage of RAM), "Nmb" (megabytes), or "Ngb" (gigabytes).
func ValidateMemoryThreshold(s string) error {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return fmt.Errorf("empty threshold")
	}
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return fmt.Errorf("invalid percentage %q: %w", s, err)
		}
		if v <= 0 || v > 100 {
			return fmt.Errorf("percentage %q must be between 0 and 100", s)
		}
		return nil
	}
	if strings.HasSuffix(s, "gb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return fmt.Errorf("invalid gigabytes %q: %w", s, err)
		}
		if v <= 0 {
			return fmt.Errorf("gigabytes %q must be positive", s)
		}
		return nil
	}
	if strings.HasSuffix(s, "mb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return fmt.Errorf("invalid megabytes %q: %w", s, err)
		}
		if v <= 0 {
			return fmt.Errorf("megabytes %q must be positive", s)
		}
		return nil
	}
	return fmt.Errorf("unknown format %q: use \"N%%\", \"Nmb\", or \"Ngb\"", s)
}

// ParseByteSize parses a human-readable byte size string like "64MB", "1GB",
// "512KB", or a plain number (bytes). Case-insensitive.
func ParseByteSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	var suffix string
	var multiplier int
	for _, pair := range []struct {
		suffix string
		mult   int
	}{
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
	} {
		if strings.HasSuffix(upper, pair.suffix) {
			suffix = pair.suffix
			multiplier = pair.mult
			break
		}
	}
	numStr := strings.TrimSpace(s[:len(s)-len(suffix)])
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", s)
	}
	if multiplier > 0 {
		return n * multiplier, nil
	}
	return n, nil
}
