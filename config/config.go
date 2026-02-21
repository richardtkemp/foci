package config

import (
	"flag"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type AgentConfig struct {
	ID                string   `toml:"id"`
	Model             string   `toml:"model"`
	Workspace         string   `toml:"workspace"`
	HeartbeatInterval string   `toml:"heartbeat_interval"`
	SystemFiles       []string `toml:"system_files"`       // workspace file order for system prompt (default: IDENTITY.md, SOUL.md, ...)
	DuplicateMessages bool     `toml:"duplicate_messages"` // send user text twice per API call (improves instruction following)
}

type AnthropicConfig struct {
	Token       string `toml:"token"`
	BraveAPIKey string `toml:"brave_api_key"`
}

type TelegramConfig struct {
	BotToken      string   `toml:"bot_token"`
	AllowedUsers  []string `toml:"allowed_users"`
	SecondaryBots []string `toml:"secondary_bots"` // tokens for secondary bots (multiball)
}

type SessionsConfig struct {
	Dir                  string  `toml:"dir"`
	CompactionThreshold  float64 `toml:"compaction_threshold"`
}

type MemoryConfig struct {
	Dir string `toml:"dir"`
}

type HTTPConfig struct {
	Port int    `toml:"port"`
	Bind string `toml:"bind"`
}

type LoggingConfig struct {
	Level            string `toml:"level"`
	EventFile        string `toml:"event_file"`
	APIFile          string `toml:"api_file"`
	ConversationFile string `toml:"conversation_file"`
	FullPayload         bool   `toml:"full_payload"`          // write full API payloads to api-payload.jsonl
	PayloadFile         string `toml:"payload_file"`          // path to api-payload.jsonl (default: api-payload.jsonl)
	CacheBustThreshold  int    `toml:"cache_bust_threshold"`  // alert when cache_write exceeds this (0 = disabled)
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

type CommandConfig struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Script      string `toml:"script"`
	Timeout     int    `toml:"timeout"` // seconds, default 10
}

type Config struct {
	Agent     AgentConfig     `toml:"agent"`
	Anthropic AnthropicConfig `toml:"anthropic"`
	Telegram  TelegramConfig  `toml:"telegram"`
	Sessions  SessionsConfig  `toml:"sessions"`
	Memory    MemoryConfig    `toml:"memory"`
	HTTP      HTTPConfig      `toml:"http"`
	Logging   LoggingConfig   `toml:"logging"`
	Voice     VoiceConfig     `toml:"voice"`
	Commands  []CommandConfig `toml:"commands"`
}

// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Defaults
	if cfg.Agent.Model == "" {
		cfg.Agent.Model = "claude-haiku-4-5"
	}
	if cfg.Agent.HeartbeatInterval == "" {
		cfg.Agent.HeartbeatInterval = "45m"
	}
	if cfg.Sessions.CompactionThreshold == 0 {
		cfg.Sessions.CompactionThreshold = 0.8
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

	return &cfg, nil
}

// ParseFlags returns the config file path from command-line flags.
func ParseFlags() string {
	path := flag.String("config", "clod.toml", "path to config file")
	flag.Parse()
	return *path
}
