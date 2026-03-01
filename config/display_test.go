package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func testConfig() (*Config, AgentConfig) {
	cfg := &Config{
		Anthropic: AnthropicConfig{
			SetupToken:      "sk-ant-secret",
			BraveAPIKey:     "brave-secret",
			HTTPTimeout:     "120s",
			UsageAPITimeout: "10s",
		},
		Defaults: DefaultsConfig{
			ShowToolCalls: func() *ToolCallDisplay { v := ToolCallPreview; return &v }(),
			ShowThinking:  func() *ShowThinking { v := ShowThinkingOff; return &v }(),
			DisplayWidth:  func() *int { v := 44; return &v }(),
		},
		Telegram: TelegramConfig{
			AllowedUsers:        []string{"alice"},
			EnableStopAliases:   true,
			EnableStartupNotify: true,
			MultiballSessionTTL: "60m",
			MessageQueueSize:    64,
			LongPollTimeout:     "65s",
		},
		Sessions: SessionsConfig{
			Dir:                   "/data/sessions",
			CompactionThreshold:   0.8,
			CompactionMaxTokens:   4096,
			CompactionMinMessages: 4,
		},
		Memory: MemoryConfig{
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
			EventFile:             "/logs/foci.log",
			APIFile:               "/logs/api.jsonl",
			ConversationFile:      "/data/conversation.db",
			WarningMaxPerWindow:   3,
			WarningWindowDuration: "5m",
		},
		Tools: ToolsConfig{
			MaxResultChars:          15000,
			TempDir:                 "/tmp/foci-tool-results",
			TmuxCols:                300,
			TmuxRows:                30,
			ExecAutoBackground:      10,
			ExecDefaultTimeout:      30,
			MaxSummaryChars:         300000,
			TmuxCommandTimeout:      "5s",
			WebFetchTimeout:         "30s",
			WebFetchMaxBytes:        1048576,
				WebSearchTimeout:        "15s",
			MaxConcurrentSpawns:     3,
			ToolCallPreviewChars:    450,
			TmuxMemoryCheckInterval: "5m",
			TmuxMemoryWarn:          "10%",
			TmuxMemoryCritical:      "20%",
			TmuxMemoryKill:          "30%",
		},
		Environment:  EnvironmentConfig{Enabled: true},
		ManaWarnings: ManaWarningsConfig{},
		Database:     DatabaseConfig{BusyTimeout: "5s"},
	}
	agent := AgentConfig{
		ID:        "test-agent",
		Model:     "claude-haiku-4-5",
		Workspace: "/home/user/workspace",

		MaxToolLoops:    25,
		MaxOutputTokens: 8192,
	}
	return cfg, agent
}

func TestFormatConfig(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfig(cfg, agent)

	// Should have KEY/VALUE headers (no SECTION column — sections are headers)
	if !strings.Contains(result, "KEY") || !strings.Contains(result, "VALUE") {
		t.Error("missing table header columns")
	}
	if strings.Contains(result, "SECTION") {
		t.Error("SECTION column should not appear — sections are now headers")
	}

	// Check separator line
	if !strings.Contains(result, "─") {
		t.Error("missing table separator")
	}

	// Check sections appear as [section] headers
	for _, section := range []string{
		"agent", "telegram", "sessions", "memory",
		"logging", "http", "tools", "environment",
		"database", "anthropic",
	} {
		if !strings.Contains(result, "["+section+"]") {
			t.Errorf("missing section header [%s]", section)
		}
	}

	// Check agent fields appear as rows
	if !strings.Contains(result, "test-agent") {
		t.Error("missing agent id value")
	}
	if !strings.Contains(result, "claude-haiku-4-5") {
		t.Error("missing agent model value")
	}
	if !strings.Contains(result, "/home/user/workspace") {
		t.Error("missing agent workspace value")
	}
}

func TestFormatConfigSecretRedaction(t *testing.T) {
	cfg, agent := testConfig()
	result := FormatConfig(cfg, agent)

	// Secrets must be redacted
	if strings.Contains(result, "sk-ant-secret") {
		t.Error("anthropic token not redacted")
	}
	if strings.Contains(result, "brave-secret") {
		t.Error("brave api key not redacted")
	}
	if strings.Contains(result, "bot-token-secret") {
		t.Error("telegram bot token not redacted")
	}

	// Redacted markers must be present
	if !strings.Contains(result, "***") {
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
	if !strings.Contains(result, "branch_orientation_prompt") {
		t.Error("expected branch_orientation_prompt in available options")
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
	agent.BranchOrientationPrompt = "/tmp/orientation.md"
	agent.TelegramBot = "primary"
	agent.MultiballBots = []string{"mb1"}
	agent.TTSRate = 1.3
	boolTrue := true
	agent.StartupNotification = &boolTrue
	showPreview := ToolCallPreview
	agent.ShowToolCalls = &showPreview
	showCompact := ShowThinkingCompact
	agent.ShowThinking = &showCompact
	displayWidth := 44
	agent.DisplayWidth = &displayWidth
	agent.ReceivedFilesDir = "/tmp/images"
	agent.AllowedUsers = []string{"123"}
	agent.Effort = "high"
	agent.CompactionEffort = "high"
	// Set optional global fields
	cfg.Sessions.CompactionSummaryPrompt = "/tmp/summary.md"
	cfg.Sessions.CompactionHandoffMsg = "handoff"
	cfg.Sessions.CompactionNotify = &boolTrue
	cfg.Sessions.MaxSystemPromptFile = 20000
	cfg.Sessions.MaxSystemPromptTotal = 80000
	cfg.Sessions.BranchOrientationPrompt = "/tmp/orient.md"
	cfg.Sessions.CompactionPreserveMessages = 25
	cfg.Memory.ReindexDebounce = "2s"
	cfg.Logging.MessagesInLog = true
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

func TestFormatConfigGrouped(t *testing.T) {
	cfg, agent := testConfig()
	cfg.Agents = []AgentConfig{agent, {
		ID:        "second-agent",
		Model:     "claude-sonnet-4-6",
		Workspace: "/home/user/workspace2",

		MaxToolLoops:    25,
		MaxOutputTokens: 8192,
	}}

	tables := FormatConfigGrouped(cfg, agent)

	// Should have 2 tables: Global + calling agent only
	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(tables))
	}

	// Each table should be wrapped in code blocks
	for i, table := range tables {
		if !strings.HasPrefix(table, "```\n") || !strings.HasSuffix(table, "\n```") {
			t.Errorf("table %d not wrapped in code blocks:\n%s", i, table)
		}
	}

	// First table should be Global
	if !strings.Contains(tables[0], "Global") {
		t.Errorf("first table should be Global:\n%s", tables[0])
	}
	// Global should contain non-agent sections as [section] headers
	for _, section := range []string{"telegram", "sessions", "logging", "tools"} {
		if !strings.Contains(tables[0], "["+section+"]") {
			t.Errorf("Global table missing section header [%s]", section)
		}
	}
	// Global should NOT contain agent-specific data
	if strings.Contains(tables[0], "test-agent") {
		t.Error("Global table should not contain agent ID")
	}

	// Only the calling agent should appear
	if !strings.Contains(tables[1], "Agent: test-agent") {
		t.Errorf("second table should be test-agent:\n%s", tables[1])
	}
	if !strings.Contains(tables[1], "claude-haiku-4-5") {
		t.Error("test-agent table missing model")
	}

	// Other agents should NOT appear
	for _, table := range tables {
		if strings.Contains(table, "second-agent") {
			t.Error("other agent should not appear in output")
		}
	}
}

func TestFormatConfigGroupedAnnotations(t *testing.T) {
	cfg, _ := testConfig()
	// Set defaults as Load() would.
	cfg.Defaults = DefaultsConfig{
		Model: "claude-haiku-4-5",

		MaxToolLoops:    25,
		MaxOutputTokens: 8192,
	}
	// Agent overrides model from the default.
	agent := AgentConfig{
		ID:        "test-agent",
		Model:     "claude-sonnet-4-6",
		Workspace: "/home/user/workspace",

		MaxToolLoops:    25,
		MaxOutputTokens: 8192,
	}
	cfg.Agents = []AgentConfig{agent}

	// Simulate TOML metadata: model is explicitly set, some others are not (hardcoded default).
	cfg.DefinedKeys = map[string]bool{
		"defaults":                   true,
		"defaults.model":             true,
		"defaults.max_tool_loops":    true,
		"defaults.max_output_tokens": true,
		"telegram":                   true,
		"telegram.allowed_users":     true,
		"sessions":                   true,
		"sessions.dir":               true,
		"logging":                    true,
		"logging.level":              true,
		"http":                       true,
		"http.port":                  true,
	}

	tables := FormatConfigGrouped(cfg, agent)
	if len(tables) < 1 {
		t.Fatal("expected at least 1 table")
	}
	global := tables[0]

	// defaults.model is explicitly set but overridden by agent → "(overridden)"
	if !strings.Contains(global, "claude-haiku-4-5 (overridden)") {
		t.Errorf("expected model to show (overridden):\n%s", global)
	}

	// defaults.max_tool_loops is set and NOT overridden → no annotation
	if strings.Contains(global, "25 (overridden)") || strings.Contains(global, "25 (default)") {
		t.Errorf("max_tool_loops should have no annotation:\n%s", global)
	}
}

func TestFormatTableBySection(t *testing.T) {
	rows := []configRow{
		{"alpha", "key1", "val1"},
		{"alpha", "key2", "val2"},
		{"beta", "key3", "val3"},
		{"alpha", "key4", "val4"}, // alpha appears again — should still be grouped under first alpha
	}
	result := formatTableBySection(rows)

	// Should have section headers
	if !strings.Contains(result, "[alpha]") {
		t.Error("missing [alpha] header")
	}
	if !strings.Contains(result, "[beta]") {
		t.Error("missing [beta] header")
	}
	// Should NOT have SECTION column
	if strings.Contains(result, "SECTION") {
		t.Error("should not have SECTION column")
	}
	// alpha section should contain all three alpha keys
	for _, key := range []string{"key1", "key2", "key4"} {
		if !strings.Contains(result, key) {
			t.Errorf("missing key %q", key)
		}
	}
	// beta section should contain key3
	if !strings.Contains(result, "key3") {
		t.Error("missing key3")
	}
	// Sections should appear in insertion order: alpha before beta
	alphaIdx := strings.Index(result, "[alpha]")
	betaIdx := strings.Index(result, "[beta]")
	if alphaIdx >= betaIdx {
		t.Error("[alpha] should appear before [beta]")
	}
}

func TestFormatAvailableDeduplication(t *testing.T) {
	cfg, agent := testConfig()
	// Ensure both agent and sessions have branch_orientation_prompt unset
	agent.BranchOrientationPrompt = ""
	cfg.Sessions.BranchOrientationPrompt = ""
	// Ensure both agent and defaults have system_files unset
	agent.SystemFiles = nil
	cfg.Defaults.SystemFiles = nil

	result := FormatAvailable(cfg, agent)

	// branch_orientation_prompt appears in both agent and sessions sections,
	// but after deduplication only the sessions entry should remain.
	branchCount := strings.Count(result, "branch_orientation_prompt")
	if branchCount > 1 {
		t.Errorf("branch_orientation_prompt appears %d times, expected 1 after dedup", branchCount)
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
