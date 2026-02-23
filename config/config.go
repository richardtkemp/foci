package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clod/log"

	"github.com/BurntSushi/toml"
)

// AgentMemoryConfig holds per-agent memory sources.
// These are combined with global [memory] sources, with agent-specific
// sources receiving an automatic weight boost.
type AgentMemoryConfig struct {
	Sources []MemorySource `toml:"sources"` // agent-specific memory directories
}

type AgentConfig struct {
	ID                string            `toml:"id"`
	Model             string            `toml:"model"`
	Workspace         string            `toml:"workspace"`
	HeartbeatInterval string            `toml:"heartbeat_interval"`
	SystemFiles       []string          `toml:"system_files"`       // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages bool              `toml:"duplicate_messages"` // send user text twice per API call (improves instruction following)
	ForkPrompt        string            `toml:"fork_prompt"`        // injected as context when a multiball session is forked
	TelegramBot       string            `toml:"telegram_bot"`       // references key in [telegram.bots] map
	MultiballBot      string            `toml:"multiball_bot"`      // references key in [telegram.bots] map (optional)
	Memory            AgentMemoryConfig `toml:"memory"`             // per-agent memory sources (combined with global [memory])
	MaxToolLoops      int               `toml:"max_tool_loops"`     // max tool iterations per turn (default 25)
	MaxOutputTokens   int               `toml:"max_output_tokens"`  // max tokens in model response (default 8192)
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
	SecondaryBots       []string                     `toml:"secondary_bots"`        // legacy: tokens for secondary bots (multiball)
	Bots                map[string]TelegramBotConfig `toml:"bots"`                  // named bots for multi-agent
	StopAliases         []string                     `toml:"stop_aliases"`          // aliases for /stop command (e.g., ["stop", "wait"])
	EnableStopAliases   bool                         `toml:"enable_stop_aliases"`   // enable stop command aliases (default true)
	EnableStartupNotify  bool                         `toml:"enable_startup_notify"`  // send notification on startup (default true)
	MultiballSessionTTL  string                       `toml:"multiball_session_ttl"` // idle TTL before a multiball bot can be reclaimed (default "60m", "0" disables)
	MessageQueueSize     int                          `toml:"message_queue_size"`    // outbound message queue buffer size (default 64)
	LongPollTimeout      string                       `toml:"long_poll_timeout"`     // long-poll timeout for getUpdates (default "65s")
}

type SessionsConfig struct {
	Dir                     string  `toml:"dir"`
	CompactionThreshold     float64 `toml:"compaction_threshold"`      // compact at this % of context window (default 0.8)
	CompactionModel         string  `toml:"compaction_model"`          // model to use for summarization (default: agent model)
	CompactionMaxTokens     int     `toml:"compaction_max_tokens"`     // max output tokens for summary (default 4096)
	CompactionMinMessages   int     `toml:"compaction_min_messages"`   // min messages before compacting (default 4)
	CompactionSummaryPrompt string  `toml:"compaction_summary_prompt"` // custom summary prompt
	CompactionHandoffMsg    string  `toml:"compaction_handoff_msg"`    // handoff message after compaction
	CompactionSystemPrompt  string  `toml:"compaction_system_prompt"`  // extra system prompt injected only during compaction (saves tokens on regular turns)
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
	InjectAgentWarnings   bool   `toml:"inject_agent_warnings"`   // inject warnings/errors into agent session (default false)
	WarningMaxPerWindow   int    `toml:"warning_max_per_window"`  // max identical warnings per window before suppression (default 3)
	WarningWindowDuration string `toml:"warning_window_duration"` // time window for warning dedup (default "5m")
}

type VoiceConfig struct {
	// STT (speech-to-text) — provider is always Whisper API (OpenAI-compatible)
	STTEndpoint string `toml:"stt_endpoint"` // default: Groq
	STTModel    string `toml:"stt_model"`    // default: whisper-large-v3

	// TTS (text-to-speech) — configurable provider
	TTSProvider string `toml:"tts_provider"` // "edge-tts" (default) or "openai"
	TTSEndpoint string `toml:"tts_endpoint"` // for openai provider
	TTSModel    string `toml:"tts_model"`    // for openai provider, e.g. "openai/tts-1-mini"
	TTSVoice    string `toml:"tts_voice"`    // voice name (provider-specific)
}

type CacheConfig struct {
	Strategy string `toml:"strategy"` // "auto" (top-level, default) or "explicit" (manual breakpoints)
}

type ManaWarningsConfig struct {
	Name       string `toml:"name"`       // what to call quota (default "mana")
	Thresholds []int  `toml:"thresholds"` // mana percentages to warn at (e.g. [50, 25, 10, 5])
}

type SkillsConfig struct {
	Dirs []string `toml:"dirs"` // directories to scan for skill subdirectories
}

type ToolsConfig struct {
	MaxResultChars     int    `toml:"max_result_chars"`      // max chars before writing result to file (default 10000)
	TempDir            string `toml:"temp_dir"`              // where to write large tool results (default /tmp/clod-tool-results)
	TmuxCols           int    `toml:"tmux_cols"`             // tmux window columns on start (default 300)
	TmuxRows           int    `toml:"tmux_rows"`             // tmux window rows on start (default 30)
	ExecAutoBackground int    `toml:"exec_auto_background"`  // seconds before auto-backgrounding exec (default 10, 0 disables)
	ExecDefaultTimeout int    `toml:"exec_default_timeout"`  // default timeout for exec commands in seconds (default 30)
	ExecMaxOutputChars int    `toml:"exec_max_output_chars"` // max chars in exec output before truncation (default 100000)
	TmuxCommandTimeout string `toml:"tmux_command_timeout"`  // timeout for tmux control commands (default "5s")
	WebFetchTimeout    string `toml:"web_fetch_timeout"`     // HTTP timeout for web fetch (default "30s")
	WebFetchMaxBytes   int    `toml:"web_fetch_max_bytes"`   // max bytes to read from web fetch (default 1048576 = 1MB)
	WebFetchMaxChars   int    `toml:"web_fetch_max_chars"`   // max chars in web fetch output before truncation (default 50000)
	WebSearchTimeout   string `toml:"web_search_timeout"`    // HTTP timeout for web search (default "15s")
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

type Config struct {
	DataDir      string             `toml:"data_dir"` // directory for databases, sessions, state (default: same as config file)
	Agent        AgentConfig        `toml:"agent"`    // legacy: single agent
	Agents       []AgentConfig      `toml:"agents"`   // multi-agent: array of agents
	Anthropic    AnthropicConfig    `toml:"anthropic"`
	Telegram     TelegramConfig     `toml:"telegram"`
	Sessions     SessionsConfig     `toml:"sessions"`
	Memory       MemoryConfig       `toml:"memory"`
	Database     DatabaseConfig     `toml:"database"`
	HTTP         HTTPConfig         `toml:"http"`
	Logging      LoggingConfig      `toml:"logging"`
	Voice        VoiceConfig        `toml:"voice"`
	Cache        CacheConfig        `toml:"cache"`
	ManaWarnings ManaWarningsConfig `toml:"usage_warnings"`
	Skills       SkillsConfig       `toml:"skills"`
	Tools        ToolsConfig        `toml:"tools"`
	Commands     []CommandConfig    `toml:"commands"`
	PromptRules  []PromptRule       `toml:"prompt_rules"` // regex find/replace rules applied to inbound messages
	WelcomeFile  string             `toml:"welcome_file"` // path to welcome/changelog file injected on startup (e.g. /home/clod/WELCOME.md)
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

	// Cache
	validStrategies := map[string]bool{"auto": true, "explicit": true}
	if !validStrategies[cfg.Cache.Strategy] {
		return fmt.Errorf("[cache] strategy = %q: must be \"auto\" or \"explicit\"", cfg.Cache.Strategy)
	}

	// Agents
	for i, acfg := range cfg.Agents {
		if _, err := time.ParseDuration(acfg.HeartbeatInterval); err != nil {
			return fmt.Errorf("agent[%d] (%s) heartbeat_interval = %q: %w", i, acfg.ID, acfg.HeartbeatInterval, err)
		}
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

	// Backward compat: [agent] (singular) → single-element Agents array
	if len(cfg.Agents) == 0 && cfg.Agent.ID != "" {
		cfg.Agents = []AgentConfig{cfg.Agent}
	}

	// Apply defaults to all agents
	for i := range cfg.Agents {
		if cfg.Agents[i].Model == "" {
			cfg.Agents[i].Model = "claude-haiku-4-5"
		}
		if cfg.Agents[i].HeartbeatInterval == "" {
			cfg.Agents[i].HeartbeatInterval = "45m"
		}
		if cfg.Agents[i].MaxToolLoops == 0 {
			cfg.Agents[i].MaxToolLoops = 25
		}
		if cfg.Agents[i].MaxOutputTokens == 0 {
			cfg.Agents[i].MaxOutputTokens = 8192
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
	if cfg.Agent.HeartbeatInterval == "" {
		cfg.Agent.HeartbeatInterval = "45m"
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
		cfg.Logging.EventFile = "clod.log"
	}
	if cfg.Logging.APIFile == "" {
		cfg.Logging.APIFile = "api.jsonl"
	}
	if cfg.Logging.ConversationFile == "" {
		cfg.Logging.ConversationFile = "conversation.db"
	}
	if cfg.Logging.FullPayload && cfg.Logging.PayloadFile == "" {
		cfg.Logging.PayloadFile = "api-payload.jsonl"
	}
	if cfg.Logging.WarningMaxPerWindow == 0 && !md.IsDefined("logging", "warning_max_per_window") {
		cfg.Logging.WarningMaxPerWindow = 3
	}
	if cfg.Logging.WarningWindowDuration == "" {
		cfg.Logging.WarningWindowDuration = "5m"
	}
	if cfg.Anthropic.CredentialsFile == "" {
		cfg.Anthropic.CredentialsFile = "~/.claude/.credentials.json"
	}
	if cfg.Cache.Strategy == "" {
		cfg.Cache.Strategy = "auto"
	}
	if cfg.ManaWarnings.Name == "" {
		cfg.ManaWarnings.Name = "mana"
	}
	if cfg.Tools.MaxResultChars == 0 {
		cfg.Tools.MaxResultChars = 10000
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
		cfg.WelcomeFile = "WELCOME.md" // relative to working directory (usually $CLOD_HOME)
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
	if !md.IsDefined("telegram", "enable_stop_aliases") {
		cfg.Telegram.EnableStopAliases = true
	}
	if !md.IsDefined("telegram", "enable_startup_notify") {
		cfg.Telegram.EnableStartupNotify = true
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

// DataPath resolves the path for a data file (database, state, etc.).
// If DataDir is set and absolute, the file is placed there.
// Otherwise, it falls back to configDir (the directory containing clod.toml).
func (c *Config) DataPath(configDir, filename string) string {
	if c.DataDir != "" && filepath.IsAbs(c.DataDir) {
		return filepath.Join(c.DataDir, filename)
	}
	return filepath.Join(configDir, filename)
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
