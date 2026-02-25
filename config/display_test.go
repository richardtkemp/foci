package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func testConfig() (*Config, AgentConfig) {
	cfg := &Config{
		Anthropic: AnthropicConfig{
			Token:           "sk-ant-secret",
			OAuthToken:      "oauth-secret",
			BraveAPIKey:     "brave-secret",
			CredentialsFile: "/home/user/.credentials.json",
			HTTPTimeout:     "120s",
			UsageAPITimeout: "10s",
		},
		Telegram: TelegramConfig{
			BotToken:            "bot-token-secret",
			AllowedUsers:        []string{"alice"},
			EnableStopAliases:   true,
			EnableStartupNotify: true,
			MultiballSessionTTL: "60m",
			MessageQueueSize:    64,
			LongPollTimeout:     "65s",
			ShowToolCalls:       true,
		},
		Sessions: SessionsConfig{
			Dir:                 "/data/sessions",
			CompactionThreshold: 0.8,
			CompactionMaxTokens: 4096,
			CompactionMinMessages: 4,
		},
		Memory: MemoryConfig{
			Dir:                "/data/memory",
			ConversationWeight: 0.1,
			SearchLimit:        20,
		},
		HTTP: HTTPConfig{
			Bind:                    "127.0.0.1",
			Port:                    18791,
			GracefulShutdownTimeout: "30s",
		},
		Logging: LoggingConfig{
			Level:                 "INFO",
			EventFile:             "/logs/clod.log",
			APIFile:               "/logs/api.jsonl",
			ConversationFile:      "/data/conversation.db",
			WarningMaxPerWindow:   3,
			WarningWindowDuration: "5m",
		},
		Tools: ToolsConfig{
			MaxResultChars:          10000,
			TempDir:                 "/tmp/clod-tool-results",
			TmuxCols:                300,
			TmuxRows:                30,
			ExecAutoBackground:      10,
			ExecDefaultTimeout:      30,
			ExecMaxOutputChars:      100000,
			TmuxCommandTimeout:      "5s",
			WebFetchTimeout:         "30s",
			WebFetchMaxBytes:        1048576,
			WebFetchMaxChars:        50000,
			WebSearchTimeout:        "15s",
			MaxConcurrentSpawns:     3,
			ToolCallPreviewChars:    450,
			TmuxMemoryCheckInterval: "5m",
			TmuxMemoryWarn:          "10%",
			TmuxMemoryCritical:      "20%",
			TmuxMemoryKill:          "30%",
		},
		Environment: EnvironmentConfig{Enabled: true},
		Cache:       CacheConfig{Strategy: "auto"},
		ManaWarnings: ManaWarningsConfig{Name: "mana"},
		Database:    DatabaseConfig{BusyTimeout: "5s"},
	}
	agent := AgentConfig{
		ID:                "test-agent",
		Model:             "claude-haiku-4-5",
		Workspace:         "/home/user/workspace",
		HeartbeatInterval: "45m",
		MaxToolLoops:      25,
		MaxOutputTokens:   8192,
	}
	return cfg, agent
}

func TestFormatConfig(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfig(cfg, agent)

	// Check section headers
	for _, section := range []string{
		"[agent]", "[telegram]", "[sessions]", "[memory]",
		"[logging]", "[http]", "[tools]", "[environment]",
		"[cache]", "[usage_warnings]", "[voice]", "[database]",
		"[anthropic]",
	} {
		if !strings.Contains(result, section) {
			t.Errorf("missing section %q", section)
		}
	}

	// Check agent fields
	if !strings.Contains(result, `id = "test-agent"`) {
		t.Error("missing agent id")
	}
	if !strings.Contains(result, `model = "claude-haiku-4-5"`) {
		t.Error("missing agent model")
	}
	if !strings.Contains(result, `workspace = "/home/user/workspace"`) {
		t.Error("missing agent workspace")
	}
}

func TestFormatConfigSecretRedaction(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfig(cfg, agent)

	// Secrets must be redacted
	if strings.Contains(result, "sk-ant-secret") {
		t.Error("anthropic token not redacted")
	}
	if strings.Contains(result, "oauth-secret") {
		t.Error("oauth token not redacted")
	}
	if strings.Contains(result, "brave-secret") {
		t.Error("brave api key not redacted")
	}
	if strings.Contains(result, "bot-token-secret") {
		t.Error("telegram bot token not redacted")
	}

	// Redacted markers must be present
	if !strings.Contains(result, `token = "***"`) {
		t.Error("expected redacted token marker")
	}
}

func TestFormatConfigTOML(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfigTOML(cfg, agent)

	// Must be parseable TOML
	var parsed map[string]interface{}
	if _, err := toml.Decode(result, &parsed); err != nil {
		t.Fatalf("TOML parse error: %v\noutput:\n%s", err, result)
	}

	// Check key sections exist
	if _, ok := parsed["agent"]; !ok {
		t.Error("missing [agent] section in TOML")
	}
	if _, ok := parsed["telegram"]; !ok {
		t.Error("missing [telegram] section in TOML")
	}
}

func TestFormatConfigTOMLSecretRedaction(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfigTOML(cfg, agent)

	if strings.Contains(result, "sk-ant-secret") {
		t.Error("anthropic token not redacted in TOML")
	}
	if strings.Contains(result, "bot-token-secret") {
		t.Error("telegram bot token not redacted in TOML")
	}
}

func TestFormatAvailable(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatAvailable(cfg, agent)

	// Unset fields should appear
	if !strings.Contains(result, "fork_prompt") {
		t.Error("expected fork_prompt in available options")
	}
	if !strings.Contains(result, "system_files") {
		t.Error("expected system_files in available options")
	}
	if !strings.Contains(result, "tts_rate") {
		t.Error("expected tts_rate in available options")
	}

	// Set fields should NOT appear
	if strings.Contains(result, "max_tool_loops") {
		t.Error("max_tool_loops should not appear (it's set)")
	}
}

func TestFormatAvailableAllSet(t *testing.T) {
	cfg, agent := testConfig()
	// Set all optional agent fields
	agent.SystemFiles = []string{"IDENTITY.md"}
	agent.ForkPrompt = "/tmp/fork.md"
	agent.TelegramBot = "primary"
	agent.MultiballBots = []string{"mb1"}
	agent.TTSRate = 1.3
	boolTrue := true
	agent.StartupNotification = &boolTrue
	agent.ShowToolCalls = &boolTrue
	agent.ImageSaveDir = "/tmp/images"
	agent.AllowedUsers = []string{"123"}
	// Set optional global fields
	cfg.Sessions.CompactionSummaryPrompt = "/tmp/summary.md"
	cfg.Sessions.CompactionHandoffMsg = "handoff"
	cfg.Sessions.CompactionSystemPrompt = "/tmp/sys.md"
	cfg.Sessions.CompactionNotify = &boolTrue
	cfg.Sessions.MaxSystemPromptFile = 20000
	cfg.Sessions.MaxSystemPromptTotal = 80000
	cfg.Sessions.SessionResetPrompt = "/tmp/reset.md"
	cfg.Memory.ReindexDebounce = "2s"
	cfg.Logging.FullPayload = true
	cfg.Logging.CacheBustDetect = true
	cfg.Logging.CacheBustIdleMinutes = 10
	cfg.Voice.STTEndpoint = "https://api.groq.com"
	cfg.Voice.STTModel = "whisper-large-v3"
	cfg.Voice.TTSProvider = "edge-tts"
	cfg.Voice.TTSVoice = "en-US-AriaNeural"
	cfg.Environment.DocsPath = "/docs"
	cfg.Skills.Dirs = []string{"/skills"}
	cfg.ManaWarnings.Thresholds = []int{50, 25, 10}

	result := FormatAvailable(cfg, agent)
	if result != "All config options are set." {
		t.Errorf("expected all set message, got:\n%s", result)
	}
}

func TestRedactString(t *testing.T) {
	if redactString("secret") != "***" {
		t.Error("non-empty string should be redacted")
	}
	if redactString("") != "" {
		t.Error("empty string should remain empty")
	}
}
