package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func testConfig() (*Config, AgentConfig) {
	cfg := &Config{
		Anthropic: AnthropicConfig{
			HTTPTimeout:     "120s",
			UsageAPITimeout: "10s",
		},
		Telegram: TelegramConfig{
			AllowedUsers:        []string{"alice"},
			StartupNotify:       true,
			FacetSessionTTL: "60m",
			MessageQueueSize:    64,
			LongPollTimeout:     "65s",
			ShowToolCalls:       func() *ToolCallDisplay { v := ToolCallPreview; return &v }(),
			ShowThinking:        func() *ShowThinking { v := ShowThinkingOff; return &v }(),
			DisplayWidth:        func() *int { v := 44; return &v }(),
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
			TempDir:                 "/tmp/foci/tool-results",
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
			ExploreMaxDepth:         100,
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
		Workspace: "/home/user/workspace",

		MaxToolLoops:    25,
		MaxOutputTokens: 16384,
	}
	return cfg, agent
}


func TestFormatConfigGroupedBackgroundFieldsAlwaysShown(t *testing.T) {
	// Proves that background and invest_interval fields appear in the grouped output
	// even when background is disabled, verifying the always-shown behavior in the
	// multi-table format as well.
	cfg, agent := testConfig()
	cfg.Agents = []AgentConfig{agent}
	cfg.Background.Enabled = false
	cfg.Background.Interval = "5m"
	cfg.Mana.InvestInterval = "30m"
	agent.Background = cfg.Background

	tables := FormatConfigGrouped(cfg, agent)
	combined := strings.Join(tables, "\n")

	for _, key := range []string{
		"background.enabled",
		"background.interval",
		"invest_interval",
	} {
		if !strings.Contains(combined, key) {
			t.Errorf("missing %q in FormatConfigGrouped output when background disabled", key)
		}
	}
}

func TestFormatConfigTOML(t *testing.T) {
	// Proves that FormatConfigTOML produces valid parseable TOML containing at
	// minimum an [agent] and [telegram] section.
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

func TestFormatAvailable(t *testing.T) {
	// Proves that FormatAvailable lists unset optional fields (like system_files and
	// orientation prompts) and excludes already-set fields like max_tool_loops.
	cfg, agent := testConfig()
	result := FormatAvailable(cfg, agent)

	// Unset fields should appear
	if !strings.Contains(result, "branch_orientation_facet_prompt") {
		t.Error("expected branch_orientation_facet_prompt in available options")
	}
	if !strings.Contains(result, "branch_orientation_headless_prompt") {
		t.Error("expected branch_orientation_headless_prompt in available options")
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
	// Proves that when all optional config fields are explicitly set, FormatAvailable
	// returns an "all set" message rather than listing any remaining options.
	cfg, agent := testConfig()
	// Set all optional agent fields
	agent.SystemFiles = []string{"IDENTITY.md"}
	agent.BranchOrientationFacetPrompt = "/tmp/orientation-facet.md"
	agent.BranchOrientationHeadlessPrompt = "/tmp/orientation-headless.md"
	displayWidth := 44
	tableWrapLines := 5
	tableStyle := "pretty"
	agent.Platforms = &PlatformsConfig{Telegram: &TelegramPlatformConfig{
		Bot:              "primary",
		FacetBots:    []string{"mb1"},
		DisplayWidth:     &displayWidth,
		TableWrapLines:   &tableWrapLines,
		TableStyle:       &tableStyle,
		ReceivedFilesDir: "/tmp/images",
		AllowedUsers:     []string{"123"},
	}}
	agent.TTSRate = 1.3
	boolTrue := true
	agent.StartupNotify = &boolTrue
	showPreview := ToolCallPreview
	agent.ShowToolCalls = &showPreview
	showCompact := ShowThinkingCompact
	agent.ShowThinking = &showCompact
	agent.Effort = "high"
	agent.CompactionEffort = "high"
	// Set optional global fields
	cfg.Sessions.CompactionSummaryPrompt = "/tmp/summary.md"
	cfg.Sessions.CompactionHandoffMsg = "handoff"
	cfg.Sessions.CompactionNotify = &boolTrue
	cfg.Sessions.MaxSystemPromptFile = 20000
	cfg.Sessions.MaxSystemPromptTotal = 80000
	cfg.Sessions.BranchOrientationFacetPrompt = "/tmp/orient-facet.md"
	cfg.Sessions.BranchOrientationHeadlessPrompt = "/tmp/orient-headless.md"
	cfg.Sessions.CompactionPreserveMessages = 25
	cfg.Memory.ReindexDebounce = "2s"
	cfg.Logging.MessagesInLog = true
	cfg.Logging.FullPayload = true
	cfg.Logging.CacheBustDetect = true
	cfg.Logging.CacheBustIdleMinutes = 10
	cfg.TTS = []TTSConfig{{ID: "edge", Format: "edge-tts", Voice: "en-US-AriaNeural"}}
	cfg.STT = []STTConfig{{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com", Model: "whisper-large-v3"}}
	cfg.Environment.DocsPath = "/docs"
	cfg.Skills.Dirs = []string{"/skills"}
	cfg.ManaWarnings.Thresholds = []int{50, 25, 10}

	result := FormatAvailable(cfg, agent)
	if result != "All config options are set." {
		t.Errorf("expected all set message, got:\n%s", result)
	}
}

func TestFormatConfigGrouped(t *testing.T) {
	// Proves that FormatConfigGrouped produces a Global table (non-agent sections)
	// and one table for the calling agent only, with other agents excluded and
	// section headers present in the global table.
	cfg, agent := testConfig()
	cfg.Agents = []AgentConfig{agent, {
		ID:        "second-agent",
		Workspace: "/home/user/workspace2",

		MaxToolLoops:    25,
		MaxOutputTokens: 16384,
	}}

	tables := FormatConfigGrouped(cfg, agent)

	// Should have 2 tables: Global + calling agent only
	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(tables))
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
	if !strings.Contains(tables[1], "/home/user/workspace") {
		t.Error("test-agent table missing workspace")
	}

	// Other agents should NOT appear
	for _, table := range tables {
		if strings.Contains(table, "second-agent") {
			t.Error("other agent should not appear in output")
		}
	}
}

func TestFormatConfigGroupedAnnotations(t *testing.T) {
	// Proves that FormatConfigGrouped annotates global default values as "(overridden)"
	// when the active agent uses a different value, and shows no annotation when the
	// agent matches the default.
	cfg, _ := testConfig()
	// Set defaults as Load() would.
	cfg.Defaults = DefaultsConfig{
		MaxToolLoops:    25,
		MaxOutputTokens: 16384,
	}
	cfg.Groups.Powerful = "claude-haiku-4-5"
	// Agent overrides max_output_tokens from the default.
	agent := AgentConfig{
		ID:        "test-agent",
		Workspace: "/home/user/workspace",

		MaxToolLoops:    25,
		MaxOutputTokens: 32768,
	}
	cfg.Agents = []AgentConfig{agent}

	// Simulate TOML metadata: max_output_tokens is explicitly set, some others are not (hardcoded default).
	cfg.DefinedKeys = map[string]bool{
		"defaults":                     true,
		"defaults.max_output_tokens":   true,
		"defaults.max_tool_loops":      true,
		"groups":                       true,
		"groups.powerful":              true,
		"telegram":                     true,
		"telegram.allowed_users":       true,
		"sessions":                     true,
		"sessions.dir":                 true,
		"logging":                      true,
		"logging.level":                true,
		"http":                         true,
		"http.port":                    true,
	}

	tables := FormatConfigGrouped(cfg, agent)
	if len(tables) < 1 {
		t.Fatal("expected at least 1 table")
	}
	global := tables[0]

	// defaults.max_output_tokens is explicitly set but overridden by agent → "(overridden)"
	if !strings.Contains(global, "16384 (overridden)") {
		t.Errorf("expected max_output_tokens to show (overridden):\n%s", global)
	}

	// defaults.max_tool_loops is set and NOT overridden → no annotation
	if strings.Contains(global, "25 (overridden)") || strings.Contains(global, "25 (default)") {
		t.Errorf("max_tool_loops should have no annotation:\n%s", global)
	}
}

func TestFormatTableBySection(t *testing.T) {
	// Proves that formatTableBySection groups rows under [section] headers in
	// insertion order, without a SECTION column, and includes all keys even when
	// the same section name appears non-consecutively in the input.
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
	// Proves that FormatAvailable deduplicates fields that appear in both agent and
	// sessions sections, showing each option only once in the output.
	cfg, agent := testConfig()
	// Ensure both agent and sessions have orientation prompts unset
	agent.BranchOrientationFacetPrompt = ""
	agent.BranchOrientationHeadlessPrompt = ""
	cfg.Sessions.BranchOrientationFacetPrompt = ""
	cfg.Sessions.BranchOrientationHeadlessPrompt = ""
	// Ensure both agent and defaults have system_files unset
	agent.SystemFiles = nil
	cfg.Defaults.SystemFiles = nil

	result := FormatAvailable(cfg, agent)

	// branch_orientation_facet_prompt appears in both agent and sessions sections,
	// but after deduplication only the sessions entry should remain.
	facetCount := strings.Count(result, "branch_orientation_facet_prompt")
	if facetCount > 1 {
		t.Errorf("branch_orientation_facet_prompt appears %d times, expected 1 after dedup", facetCount)
	}
}


