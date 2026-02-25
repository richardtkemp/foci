package config

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// FormatConfig returns a readable section-by-section dump of the running config
// for the given agent. Secrets are redacted.
func FormatConfig(cfg *Config, agent AgentConfig) string {
	var b strings.Builder

	// [agent]
	b.WriteString("[agent]\n")
	writeField(&b, "id", agent.ID)
	writeField(&b, "model", agent.Model)
	writeField(&b, "workspace", agent.Workspace)
	writeField(&b, "heartbeat_interval", agent.HeartbeatInterval)
	if len(agent.SystemFiles) > 0 {
		writeField(&b, "system_files", agent.SystemFiles)
	}
	writeField(&b, "duplicate_messages", agent.DuplicateMessages)
	if agent.ForkPrompt != "" {
		writeField(&b, "fork_prompt", agent.ForkPrompt)
	}
	if agent.TelegramBot != "" {
		writeField(&b, "telegram_bot", agent.TelegramBot)
	}
	if len(agent.MultiballBots) > 0 {
		writeField(&b, "multiball_bots", agent.MultiballBots)
	}
	writeField(&b, "max_tool_loops", agent.MaxToolLoops)
	writeField(&b, "max_output_tokens", agent.MaxOutputTokens)
	if agent.TTSRate != 0 {
		writeField(&b, "tts_rate", agent.TTSRate)
	}
	writeField(&b, "inject_agent_warnings", agent.InjectAgentWarnings)
	if agent.StartupNotification != nil {
		writeField(&b, "startup_notification", *agent.StartupNotification)
	}
	if agent.ShowToolCalls != nil {
		writeField(&b, "show_tool_calls", *agent.ShowToolCalls)
	}
	if agent.ImageSaveDir != "" {
		writeField(&b, "image_save_dir", agent.ImageSaveDir)
	}
	if len(agent.AllowedUsers) > 0 {
		writeField(&b, "allowed_users", agent.AllowedUsers)
	}

	// [defaults]
	b.WriteString("\n[defaults]\n")
	writeField(&b, "model", cfg.Defaults.Model)
	writeField(&b, "heartbeat_interval", cfg.Defaults.HeartbeatInterval)
	writeField(&b, "max_tool_loops", cfg.Defaults.MaxToolLoops)
	writeField(&b, "max_output_tokens", cfg.Defaults.MaxOutputTokens)
	if cfg.Defaults.DuplicateMessages {
		writeField(&b, "duplicate_messages", cfg.Defaults.DuplicateMessages)
	}
	if cfg.Defaults.InjectAgentWarnings {
		writeField(&b, "inject_agent_warnings", cfg.Defaults.InjectAgentWarnings)
	}
	if cfg.Defaults.TTSRate != 0 {
		writeField(&b, "tts_rate", cfg.Defaults.TTSRate)
	}
	if len(cfg.Defaults.SystemFiles) > 0 {
		writeField(&b, "system_files", cfg.Defaults.SystemFiles)
	}

	// [telegram]
	b.WriteString("\n[telegram]\n")
	writeField(&b, "bot_token", redactString(cfg.Telegram.BotToken))
	if len(cfg.Telegram.AllowedUsers) > 0 {
		writeField(&b, "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if len(cfg.Telegram.MultiballBots) > 0 {
		writeField(&b, "multiball_bots", cfg.Telegram.MultiballBots)
	}
	if len(cfg.Telegram.Bots) > 0 {
		var names []string
		for k := range cfg.Telegram.Bots {
			names = append(names, k)
		}
		writeField(&b, "bots", names)
	}
	if len(cfg.Telegram.StopAliases) > 0 {
		writeField(&b, "stop_aliases", cfg.Telegram.StopAliases)
	}
	writeField(&b, "enable_stop_aliases", cfg.Telegram.EnableStopAliases)
	writeField(&b, "enable_startup_notify", cfg.Telegram.EnableStartupNotify)
	writeField(&b, "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL)
	writeField(&b, "message_queue_size", cfg.Telegram.MessageQueueSize)
	writeField(&b, "long_poll_timeout", cfg.Telegram.LongPollTimeout)
	writeField(&b, "show_tool_calls", cfg.Telegram.ShowToolCalls)
	if cfg.Telegram.ImageSaveDir != "" {
		writeField(&b, "image_save_dir", cfg.Telegram.ImageSaveDir)
	}

	// [sessions]
	b.WriteString("\n[sessions]\n")
	writeField(&b, "dir", cfg.Sessions.Dir)
	writeField(&b, "compaction_threshold", cfg.Sessions.CompactionThreshold)
	writeField(&b, "compaction_max_tokens", cfg.Sessions.CompactionMaxTokens)
	writeField(&b, "compaction_min_messages", cfg.Sessions.CompactionMinMessages)
	if cfg.Sessions.CompactionSummaryPrompt != "" {
		writeField(&b, "compaction_summary_prompt", cfg.Sessions.CompactionSummaryPrompt)
	}
	if cfg.Sessions.CompactionHandoffMsg != "" {
		writeField(&b, "compaction_handoff_msg", cfg.Sessions.CompactionHandoffMsg)
	}
	if cfg.Sessions.CompactionSystemPrompt != "" {
		writeField(&b, "compaction_system_prompt", cfg.Sessions.CompactionSystemPrompt)
	}
	if cfg.Sessions.CompactionNotify != nil {
		writeField(&b, "compaction_notify", *cfg.Sessions.CompactionNotify)
	}
	writeField(&b, "compaction_debug", cfg.Sessions.CompactionDebug)
	writeField(&b, "max_system_prompt_chars_file", cfg.Sessions.MaxSystemPromptFile)
	writeField(&b, "max_system_prompt_chars_total", cfg.Sessions.MaxSystemPromptTotal)
	if cfg.Sessions.SessionResetPrompt != "" {
		writeField(&b, "session_reset_prompt", cfg.Sessions.SessionResetPrompt)
	}

	// [memory]
	b.WriteString("\n[memory]\n")
	if cfg.Memory.Dir != "" {
		writeField(&b, "dir", cfg.Memory.Dir)
	}
	if len(cfg.Memory.Sources) > 0 {
		writeField(&b, "sources", fmt.Sprintf("(%d configured)", len(cfg.Memory.Sources)))
	}
	if cfg.Memory.ReindexDebounce != "" {
		writeField(&b, "reindex_debounce", cfg.Memory.ReindexDebounce)
	}
	writeField(&b, "conversation_weight", cfg.Memory.ConversationWeight)
	writeField(&b, "search_limit", cfg.Memory.SearchLimit)

	// [logging]
	b.WriteString("\n[logging]\n")
	writeField(&b, "level", cfg.Logging.Level)
	writeField(&b, "event_file", cfg.Logging.EventFile)
	writeField(&b, "api_file", cfg.Logging.APIFile)
	writeField(&b, "conversation_file", cfg.Logging.ConversationFile)
	writeField(&b, "full_payload", cfg.Logging.FullPayload)
	if cfg.Logging.PayloadFile != "" {
		writeField(&b, "payload_file", cfg.Logging.PayloadFile)
	}
	writeField(&b, "cache_bust_detect", cfg.Logging.CacheBustDetect)
	writeField(&b, "cache_bust_idle_minutes", cfg.Logging.CacheBustIdleMinutes)
	writeField(&b, "warning_max_per_window", cfg.Logging.WarningMaxPerWindow)
	writeField(&b, "warning_window_duration", cfg.Logging.WarningWindowDuration)

	// [http]
	b.WriteString("\n[http]\n")
	writeField(&b, "bind", cfg.HTTP.Bind)
	writeField(&b, "port", cfg.HTTP.Port)
	writeField(&b, "graceful_shutdown_timeout", cfg.HTTP.GracefulShutdownTimeout)

	// [tools]
	b.WriteString("\n[tools]\n")
	writeField(&b, "max_result_chars", cfg.Tools.MaxResultChars)
	writeField(&b, "temp_dir", cfg.Tools.TempDir)
	writeField(&b, "tmux_cols", cfg.Tools.TmuxCols)
	writeField(&b, "tmux_rows", cfg.Tools.TmuxRows)
	writeField(&b, "exec_auto_background", cfg.Tools.ExecAutoBackground)
	writeField(&b, "exec_default_timeout", cfg.Tools.ExecDefaultTimeout)
	writeField(&b, "exec_max_output_chars", cfg.Tools.ExecMaxOutputChars)
	writeField(&b, "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout)
	writeField(&b, "web_fetch_timeout", cfg.Tools.WebFetchTimeout)
	writeField(&b, "web_fetch_max_bytes", cfg.Tools.WebFetchMaxBytes)
	writeField(&b, "web_fetch_max_chars", cfg.Tools.WebFetchMaxChars)
	writeField(&b, "web_search_timeout", cfg.Tools.WebSearchTimeout)
	writeField(&b, "max_concurrent_spawns", cfg.Tools.MaxConcurrentSpawns)
	writeField(&b, "tool_call_preview_chars", cfg.Tools.ToolCallPreviewChars)
	writeField(&b, "tmux_memory_check_interval", cfg.Tools.TmuxMemoryCheckInterval)
	writeField(&b, "tmux_memory_warn", cfg.Tools.TmuxMemoryWarn)
	writeField(&b, "tmux_memory_critical", cfg.Tools.TmuxMemoryCritical)
	writeField(&b, "tmux_memory_kill", cfg.Tools.TmuxMemoryKill)

	// [environment]
	b.WriteString("\n[environment]\n")
	writeField(&b, "enabled", cfg.Environment.Enabled)
	if cfg.Environment.DocsPath != "" {
		writeField(&b, "docs_path", cfg.Environment.DocsPath)
	}

	// [skills]
	if len(cfg.Skills.Dirs) > 0 {
		b.WriteString("\n[skills]\n")
		writeField(&b, "dirs", cfg.Skills.Dirs)
	}

	// [cache]
	b.WriteString("\n[cache]\n")
	writeField(&b, "strategy", cfg.Cache.Strategy)

	// [usage_warnings]
	b.WriteString("\n[usage_warnings]\n")
	writeField(&b, "name", cfg.ManaWarnings.Name)
	if len(cfg.ManaWarnings.Thresholds) > 0 {
		writeField(&b, "thresholds", cfg.ManaWarnings.Thresholds)
	}

	// [voice]
	b.WriteString("\n[voice]\n")
	if cfg.Voice.STTEndpoint != "" {
		writeField(&b, "stt_endpoint", cfg.Voice.STTEndpoint)
	}
	if cfg.Voice.STTModel != "" {
		writeField(&b, "stt_model", cfg.Voice.STTModel)
	}
	if cfg.Voice.TTSProvider != "" {
		writeField(&b, "tts_provider", cfg.Voice.TTSProvider)
	}
	if cfg.Voice.TTSEndpoint != "" {
		writeField(&b, "tts_endpoint", cfg.Voice.TTSEndpoint)
	}
	if cfg.Voice.TTSModel != "" {
		writeField(&b, "tts_model", cfg.Voice.TTSModel)
	}
	if cfg.Voice.TTSVoice != "" {
		writeField(&b, "tts_voice", cfg.Voice.TTSVoice)
	}
	if cfg.Voice.TTSRate != 0 {
		writeField(&b, "tts_rate", cfg.Voice.TTSRate)
	}

	// [database]
	b.WriteString("\n[database]\n")
	writeField(&b, "busy_timeout", cfg.Database.BusyTimeout)

	// [anthropic] (secrets redacted)
	b.WriteString("\n[anthropic]\n")
	writeField(&b, "token", redactString(cfg.Anthropic.Token))
	writeField(&b, "oauth_token", redactString(cfg.Anthropic.OAuthToken))
	writeField(&b, "brave_api_key", redactString(cfg.Anthropic.BraveAPIKey))
	writeField(&b, "credentials_file", cfg.Anthropic.CredentialsFile)
	writeField(&b, "http_timeout", cfg.Anthropic.HTTPTimeout)
	writeField(&b, "usage_api_timeout", cfg.Anthropic.UsageAPITimeout)

	// [[prompt_rules]]
	if len(cfg.PromptRules) > 0 {
		b.WriteString(fmt.Sprintf("\n[[prompt_rules]] (%d rules)\n", len(cfg.PromptRules)))
		for _, r := range cfg.PromptRules {
			writeField(&b, "find", r.Find)
			writeField(&b, "replace", r.Replace)
			b.WriteString("---\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
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
	if len(agent.SystemFiles) == 0 {
		opts = append(opts, availableOption{"agent", "system_files", "[]", "workspace file order for system prompt"})
	}
	if agent.ForkPrompt == "" {
		opts = append(opts, availableOption{"agent", "fork_prompt", "\"\"", "prompt file injected when a multiball session is forked"})
	}
	if agent.TelegramBot == "" {
		opts = append(opts, availableOption{"agent", "telegram_bot", "\"\"", "references key in [telegram.bots] map"})
	}
	if len(agent.MultiballBots) == 0 {
		opts = append(opts, availableOption{"agent", "multiball_bots", "[]", "references keys in [telegram.bots] map"})
	}
	if agent.TTSRate == 0 {
		opts = append(opts, availableOption{"agent", "tts_rate", "0", "per-agent TTS speech rate override"})
	}
	if agent.StartupNotification == nil {
		opts = append(opts, availableOption{"agent", "startup_notification", "(global)", "send startup notification (nil = use global)"})
	}
	if agent.ShowToolCalls == nil {
		opts = append(opts, availableOption{"agent", "show_tool_calls", "(global)", "show tool calls in Telegram (nil = use global)"})
	}
	if agent.ImageSaveDir == "" {
		opts = append(opts, availableOption{"agent", "image_save_dir", "\"\"", "save received images to this directory"})
	}
	if len(agent.AllowedUsers) == 0 {
		opts = append(opts, availableOption{"agent", "allowed_users", "(global)", "per-agent allowed Telegram user IDs (empty = use global)"})
	}

	// Sessions fields
	if cfg.Sessions.CompactionSummaryPrompt == "" {
		opts = append(opts, availableOption{"sessions", "compaction_summary_prompt", "\"\"", "path to summary prompt file"})
	}
	if cfg.Sessions.CompactionHandoffMsg == "" {
		opts = append(opts, availableOption{"sessions", "compaction_handoff_msg", "\"\"", "handoff message after compaction"})
	}
	if cfg.Sessions.CompactionSystemPrompt == "" {
		opts = append(opts, availableOption{"sessions", "compaction_system_prompt", "\"\"", "extra system prompt during compaction"})
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
	if len(cfg.ManaWarnings.Thresholds) == 0 {
		opts = append(opts, availableOption{"usage_warnings", "thresholds", "[]", "mana percentages to warn at"})
	}

	if len(opts) == 0 {
		return "All config options are set."
	}

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
