package config

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"clod/log"

	"github.com/BurntSushi/toml"
)

type AgentConfig struct {
	ID                string   `toml:"id"`
	Model             string   `toml:"model"`
	Workspace         string   `toml:"workspace"`
	HeartbeatInterval string   `toml:"heartbeat_interval"`
	SystemFiles       []string `toml:"system_files"`       // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages bool     `toml:"duplicate_messages"` // send user text twice per API call (improves instruction following)
	ForkPrompt        string   `toml:"fork_prompt"`        // injected as context when a multiball session is forked
	TelegramBot       string   `toml:"telegram_bot"`       // references key in [telegram.bots] map
	MultiballBot      string   `toml:"multiball_bot"`      // references key in [telegram.bots] map (optional)
}

type AnthropicConfig struct {
	Token           string `toml:"token"`
	OAuthToken      string `toml:"oauth_token"`      // OAuth access token for usage API (legacy, static)
	BraveAPIKey     string `toml:"brave_api_key"`
	CredentialsFile string `toml:"credentials_file"` // path to Claude Code credentials.json (default ~/.claude/.credentials.json)
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
	EnableStartupNotify bool                         `toml:"enable_startup_notify"` // send notification on startup (default true)
}

type SessionsConfig struct {
	Dir                     string  `toml:"dir"`
	CompactionThreshold     float64 `toml:"compaction_threshold"`      // compact at this % of context window (default 0.8)
	CompactionModel         string  `toml:"compaction_model"`          // model to use for summarization (default: agent model)
	CompactionMaxTokens     int     `toml:"compaction_max_tokens"`     // max output tokens for summary (default 4096)
	CompactionMinMessages   int     `toml:"compaction_min_messages"`   // min messages before compacting (default 4)
	CompactionSummaryPrompt string  `toml:"compaction_summary_prompt"` // custom summary prompt
	CompactionHandoffMsg    string  `toml:"compaction_handoff_msg"`    // handoff message after compaction
}

type MemorySource struct {
	Name   string  `toml:"name"`   // unique identifier (e.g., "canonical", "code", "docs")
	Dir    string  `toml:"dir"`    // directory path to index
	Weight float64 `toml:"weight"` // weight multiplier: 0.0-1.0 (1.0 = highest priority)
}

type MemoryConfig struct {
	Dir             string         `toml:"dir"`              // backward compat: single directory
	Sources         []MemorySource `toml:"sources"`          // new: multiple sources with weights
	ReindexDebounce string         `toml:"reindex_debounce"` // delay before reindex (e.g., "500ms", "2s"), default "0s"
}

type HTTPConfig struct {
	Port int    `toml:"port"`
	Bind string `toml:"bind"`
}

type LoggingConfig struct {
	Level               string `toml:"level"`
	EventFile           string `toml:"event_file"`
	APIFile             string `toml:"api_file"`
	ConversationFile    string `toml:"conversation_file"`
	FullPayload         bool   `toml:"full_payload"`          // write full API payloads to api-payload.jsonl
	PayloadFile         string `toml:"payload_file"`          // path to api-payload.jsonl (default: api-payload.jsonl)
	CacheBustDetect     bool   `toml:"cache_bust_detect"`     // alert when cache_read drops >50% vs previous request
	InjectAgentWarnings bool   `toml:"inject_agent_warnings"` // inject warnings/errors into agent session (default false)
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

type SkillsConfig struct {
	Dirs []string `toml:"dirs"` // directories to scan for skill subdirectories
}

type ToolsConfig struct {
	MaxResultChars     int    `toml:"max_result_chars"`      // max chars before writing result to file (default 10000)
	TempDir            string `toml:"temp_dir"`              // where to write large tool results (default /tmp/clod-tool-results)
	TmuxCols           int    `toml:"tmux_cols"`             // tmux window columns on start (default 300)
	TmuxRows           int    `toml:"tmux_rows"`             // tmux window rows on start (default 30)
	ExecAutoBackground int    `toml:"exec_auto_background"`  // seconds before auto-backgrounding exec (default 10, 0 disables)
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
	Agent     AgentConfig     `toml:"agent"`  // legacy: single agent
	Agents    []AgentConfig   `toml:"agents"` // multi-agent: array of agents
	Anthropic AnthropicConfig `toml:"anthropic"`
	Telegram  TelegramConfig  `toml:"telegram"`
	Sessions  SessionsConfig  `toml:"sessions"`
	Memory    MemoryConfig    `toml:"memory"`
	HTTP      HTTPConfig      `toml:"http"`
	Logging   LoggingConfig   `toml:"logging"`
	Voice     VoiceConfig     `toml:"voice"`
	Cache     CacheConfig     `toml:"cache"`
	Skills    SkillsConfig    `toml:"skills"`
	Tools     ToolsConfig     `toml:"tools"`
	Commands    []CommandConfig `toml:"commands"`
	PromptRules []PromptRule   `toml:"prompt_rules"` // regex find/replace rules applied to inbound messages
}

// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
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
	if cfg.Sessions.CompactionSummaryPrompt == "" {
		cfg.Sessions.CompactionSummaryPrompt = "Please provide a concise summary of our entire conversation so far, capturing all key decisions, context, and important details. This summary will replace the conversation history."
	}
	if cfg.Sessions.CompactionHandoffMsg == "" {
		cfg.Sessions.CompactionHandoffMsg = "[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]"
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
	if cfg.Anthropic.CredentialsFile == "" {
		cfg.Anthropic.CredentialsFile = "~/.claude/.credentials.json"
	}
	if cfg.Cache.Strategy == "" {
		cfg.Cache.Strategy = "auto"
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

	// Bool defaults: default to true unless explicitly set to false in config.
	// We use md.IsDefined because Go's zero value for bool is false,
	// so we can't distinguish "not set" from "set to false" otherwise.
	if !md.IsDefined("telegram", "enable_stop_aliases") {
		cfg.Telegram.EnableStopAliases = true
	}
	if !md.IsDefined("telegram", "enable_startup_notify") {
		cfg.Telegram.EnableStartupNotify = true
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
