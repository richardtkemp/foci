package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clod/log"

	"github.com/BurntSushi/toml"
)

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
	ID                  string            `toml:"id"`
	Model               string            `toml:"model"`
	Workspace           string            `toml:"workspace"`
	SystemFiles         []string          `toml:"system_files"`          // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages   bool              `toml:"duplicate_messages"`    // send user text twice per API call (improves instruction following)
	ForkPrompt               string            `toml:"fork_prompt"`                // DEPRECATED: use branch_orientation_prompt
	BranchOrientationPrompt  string            `toml:"branch_orientation_prompt"`  // path to prompt file injected into all branch sessions (multiball, cron, spawn)
	TelegramBot         string            `toml:"telegram_bot"`          // references key in [telegram.bots] map
	MultiballBot        string            `toml:"multiball_bot"`         // DEPRECATED: use multiball_bots. References key in [telegram.bots] map (optional)
	MultiballBots       []string          `toml:"multiball_bots"`        // references keys in [telegram.bots] map (optional)
	Memory              AgentMemoryConfig `toml:"memory"`                // per-agent memory sources (combined with global [memory])
	MaxToolLoops        int               `toml:"max_tool_loops"`        // max tool iterations per turn (default 25)
	MaxOutputTokens     int               `toml:"max_output_tokens"`     // max tokens in model response (default 8192)
	Effort              string            `toml:"effort"`                // effort level: "low", "medium", "high" (empty = omit from request)
	TTSRate             float64           `toml:"tts_rate"`              // per-agent TTS speech rate override (0 = use global [voice] tts_rate)
	InjectAgentWarnings bool              `toml:"inject_agent_warnings"` // inject warnings/errors into agent session (default false)
	StartupNotification *bool             `toml:"startup_notification"`  // send startup notification (nil = use global enable_startup_notify)
	ShowToolCalls       *bool             `toml:"show_tool_calls"`       // show tool call messages in Telegram (nil = use global telegram.show_tool_calls)
	ImageSaveDir        string            `toml:"image_save_dir"`        // save received images to this directory (empty = disabled)
	AllowedUsers        []string          `toml:"allowed_users"`         // per-agent allowed Telegram user IDs (empty = use global [telegram] allowed_users)
	// Per-agent compaction overrides (nil/empty = use global [sessions] value)
	CompactionThreshold     *float64 `toml:"compaction_threshold"`      // compact at this % of context window
	CompactionSummaryPrompt string   `toml:"compaction_summary_prompt"` // path to summary prompt file
	CompactionHandoffMsg    string   `toml:"compaction_handoff_msg"`    // handoff message after compaction
	CompactionNotify           *bool `toml:"compaction_notify"`              // send Telegram notification on compaction
	CompactionDebug            *bool `toml:"compaction_debug"`               // send compaction summary as Telegram file
	CompactionPreserveMessages *int  `toml:"compaction_preserve_messages"`   // preserve last N messages through compaction (nil = use global)
	SessionResetPrompt         string `toml:"session_reset_prompt"`           // path to prompt fired before session clear
	// Per-agent skills and prompt rules (empty = use global)
	SkillsDirs  []string     `toml:"skills_dirs"`  // skill directories (empty = use global [skills] dirs)
	PromptRules []PromptRule `toml:"prompt_rules"` // regex find/replace rules (empty = use global)
	// Per-agent tool behaviour (0 = use global [tools] value)
	ExecAutoBackground  int `toml:"exec_auto_background"`  // seconds before auto-backgrounding exec
	MaxConcurrentSpawns int `toml:"max_concurrent_spawns"` // max concurrent spawn sessions
	// Per-agent usage warning thresholds (nil = use global [usage_warnings])
	UsageWarnings AgentUsageWarningsConfig `toml:"usage_warnings"` // per-agent mana warning thresholds
}

type AnthropicConfig struct {
	Token           string `toml:"token"`
	OAuthToken      string `toml:"oauth_token"` // OAuth access token for usage API (legacy, static)
	BraveAPIKey     string `toml:"brave_api_key"`
	CredentialsFile string `toml:"credentials_file"`  // path to Claude Code credentials.json (default ~/.claude/.credentials.json)
	HTTPTimeout     string `toml:"http_timeout"`      // HTTP timeout for API calls (default "120s")
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
	ShowToolCalls       bool                         `toml:"show_tool_calls"`       // show tool call messages in Telegram (default true)
	ImageSaveDir        string                       `toml:"image_save_dir"`        // save received images to this directory (empty = disabled, per-agent overrides)
}

type SessionsConfig struct {
	Dir                     string  `toml:"dir"`
	CompactionThreshold     float64 `toml:"compaction_threshold"`          // compact at this % of context window (default 0.8)
	CompactionMaxTokens     int     `toml:"compaction_max_tokens"`         // max output tokens for summary (default 4096)
	CompactionMinMessages   int     `toml:"compaction_min_messages"`       // min messages before compacting (default 4)
	CompactionSummaryPrompt string  `toml:"compaction_summary_prompt"`     // path to summary prompt file
	CompactionHandoffMsg    string  `toml:"compaction_handoff_msg"`        // handoff message after compaction
	CompactionNotify        *bool   `toml:"compaction_notify"`             // send Telegram notification on compaction (default true)
	MaxSystemPromptFile     int     `toml:"max_system_prompt_chars_file"`  // per-file char threshold for warnings (default 20000)
	MaxSystemPromptTotal    int     `toml:"max_system_prompt_chars_total"` // total system prompt char threshold (default 80000)
	CompactionDebug             bool    `toml:"compaction_debug"`              // send compaction summary as Telegram file attachment (default false)
	CompactionPreserveMessages int     `toml:"compaction_preserve_messages"`  // preserve last N messages through compaction (default 25, 0 disables)
	SessionResetPrompt         string  `toml:"session_reset_prompt"`          // path to prompt file fired before session clear (/reset or reclaim)
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
	WarningMaxPerWindow   int    `toml:"warning_max_per_window"`  // max identical warnings per window before suppression (default 3)
	WarningWindowDuration string `toml:"warning_window_duration"` // time window for warning dedup (default "5m")
	LogRotation           bool   `toml:"log_rotation"`            // enable built-in log rotation (default true)
	RotationPeriod        string `toml:"rotation_period"`         // how often to rotate (default "24h")
	RetentionPeriod       string `toml:"retention_period"`        // keep lines newer than this (default "48h")
	RotationMaxLineSize   string `toml:"rotation_max_line_size"`  // max line size for scanner buffer (default "64MB")
	ArchiveDir            string `toml:"archive_dir"`             // gzip archive directory (default: log_dir/archive/)
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

type ToolsConfig struct {
	MaxResultChars          int    `toml:"max_result_chars"`           // max chars before writing result to file (default 15000)
	TempDir                 string `toml:"temp_dir"`                   // where to write large tool results (default /tmp/clod-tool-results)
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
	Model               string   `toml:"model"`                 // default model (default: claude-haiku-4-5)
	DuplicateMessages   bool     `toml:"duplicate_messages"`    // default duplicate_messages (default: false)
	InjectAgentWarnings bool     `toml:"inject_agent_warnings"` // default inject_agent_warnings (default: false)
	MaxToolLoops        int      `toml:"max_tool_loops"`        // default max_tool_loops (default: 25)
	MaxOutputTokens     int      `toml:"max_output_tokens"`     // default max_output_tokens (default: 8192)
	Effort              string   `toml:"effort"`                // default effort level: "low", "medium", "high" (empty = omit)
	TTSRate             float64  `toml:"tts_rate"`               // default TTS speech rate (default: 0 = voice config)
	SystemFiles         []string `toml:"system_files"`          // default system file list
}

type Config struct {
	DataDir            string             `toml:"data_dir"` // directory for databases, sessions, state (default: $HOME/data)
	Defaults           DefaultsConfig     `toml:"defaults"` // global defaults for agent-specific fields
	Agent              AgentConfig        `toml:"agent"`    // legacy: single agent
	Agents             []AgentConfig      `toml:"agents"`   // multi-agent: array of agents
	Anthropic          AnthropicConfig    `toml:"anthropic"`
	Telegram           TelegramConfig     `toml:"telegram"`
	Sessions           SessionsConfig     `toml:"sessions"`
	Memory             MemoryConfig       `toml:"memory"`
	Database           DatabaseConfig     `toml:"database"`
	HTTP               HTTPConfig         `toml:"http"`
	Logging            LoggingConfig      `toml:"logging"`
	Voice              VoiceConfig        `toml:"voice"`
	Bitwarden          BitwardenConfig    `toml:"bitwarden"`
	Cache              CacheConfig        `toml:"cache"`
	ManaWarnings       ManaWarningsConfig `toml:"usage_warnings"`
	Environment        EnvironmentConfig  `toml:"environment"`
	Skills             SkillsConfig       `toml:"skills"`
	Tools              ToolsConfig        `toml:"tools"`
	Commands           []CommandConfig    `toml:"commands"`
	PromptRules        []PromptRule       `toml:"prompt_rules"`         // regex find/replace rules applied to inbound messages
	WelcomeFile        string             `toml:"welcome_file"`         // path to welcome/changelog file injected on startup (e.g. /home/clod/WELCOME.md)
	SkipSecurityChecks bool               `toml:"skip_security_checks"` // if true, skip startup security checks for secrets.toml
	DefinedKeys        map[string]bool    `toml:"-"`                    // keys explicitly set in TOML file (populated by Load)
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

// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
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

	// Backward compat: [agent] (singular) → single-element Agents array
	if len(cfg.Agents) == 0 && cfg.Agent.ID != "" {
		cfg.Agents = []AgentConfig{cfg.Agent}
	}

	// Apply [defaults] to all agents (agent value > global default > hardcoded)
	for i := range cfg.Agents {
		if cfg.Agents[i].Model == "" {
			cfg.Agents[i].Model = cfg.Defaults.Model
		}
		if cfg.Agents[i].MaxToolLoops == 0 {
			cfg.Agents[i].MaxToolLoops = cfg.Defaults.MaxToolLoops
		}
		if cfg.Agents[i].MaxOutputTokens == 0 {
			cfg.Agents[i].MaxOutputTokens = cfg.Defaults.MaxOutputTokens
		}
		if cfg.Agents[i].Effort == "" {
			cfg.Agents[i].Effort = cfg.Defaults.Effort
		}
		if cfg.Agents[i].TTSRate == 0 {
			cfg.Agents[i].TTSRate = cfg.Defaults.TTSRate
		}
		if len(cfg.Agents[i].SystemFiles) == 0 && len(cfg.Defaults.SystemFiles) > 0 {
			cfg.Agents[i].SystemFiles = cfg.Defaults.SystemFiles
		}
		if !cfg.Agents[i].DuplicateMessages && cfg.Defaults.DuplicateMessages {
			cfg.Agents[i].DuplicateMessages = true
		}
		if !cfg.Agents[i].InjectAgentWarnings && cfg.Defaults.InjectAgentWarnings {
			cfg.Agents[i].InjectAgentWarnings = true
		}
		if cfg.Agents[i].ForkPrompt != "" {
			cfg.Agents[i].ForkPrompt = ResolvePath(cfg.Agents[i].ForkPrompt)
		}
		if cfg.Agents[i].BranchOrientationPrompt != "" {
			cfg.Agents[i].BranchOrientationPrompt = ResolvePath(cfg.Agents[i].BranchOrientationPrompt)
		}
		if cfg.Agents[i].ForkPrompt != "" && cfg.Agents[i].BranchOrientationPrompt != "" {
			log.Warnf("config", "agent %q: both fork_prompt and branch_orientation_prompt set; branch_orientation_prompt takes precedence",
				cfg.Agents[i].ID)
		}
		// Deprecated alias: multiball_bot (singular) → multiball_bots (plural)
		if cfg.Agents[i].MultiballBot != "" && len(cfg.Agents[i].MultiballBots) == 0 {
			log.Warnf("config", "agent %q: multiball_bot is deprecated, use multiball_bots = [\"%s\"]",
				cfg.Agents[i].ID, cfg.Agents[i].MultiballBot)
			cfg.Agents[i].MultiballBots = []string{cfg.Agents[i].MultiballBot}
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
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "INFO"
	}
	if cfg.Logging.EventFile == "" {
		cfg.Logging.EventFile = "logs/clod.log"
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
	if cfg.Anthropic.CredentialsFile == "" {
		cfg.Anthropic.CredentialsFile = "~/.claude/.credentials.json"
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
		cfg.Tools.TempDir = "/tmp/clod-tool-results"
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
		cfg.Anthropic.HTTPTimeout = "120s"
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
		cfg.Telegram.ShowToolCalls = true
	}

	cfg.ResolveAllPaths()

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
	if c.Sessions.SessionResetPrompt != "" {
		c.Sessions.SessionResetPrompt = ResolvePath(c.Sessions.SessionResetPrompt)
	}
	if c.Sessions.BranchOrientationPrompt != "" {
		c.Sessions.BranchOrientationPrompt = ResolvePath(c.Sessions.BranchOrientationPrompt)
	}
	if c.Sessions.CompactionSummaryPrompt != "" {
		c.Sessions.CompactionSummaryPrompt = ResolvePath(c.Sessions.CompactionSummaryPrompt)
	}
	c.WelcomeFile = ResolvePath(c.WelcomeFile)
	if c.Environment.DocsPath != "" {
		c.Environment.DocsPath = ResolvePath(c.Environment.DocsPath)
	}
	if c.Telegram.ImageSaveDir != "" {
		c.Telegram.ImageSaveDir = ResolvePath(c.Telegram.ImageSaveDir)
	}
	for i := range c.Agents {
		if c.Agents[i].ImageSaveDir != "" {
			c.Agents[i].ImageSaveDir = ResolvePath(c.Agents[i].ImageSaveDir)
		}
	}
}

// ParseFlags returns the config file path from command-line flags.
func ParseFlags() string {
	path := flag.String("config", "clod.toml", "path to config file")
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
