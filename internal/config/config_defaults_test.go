package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestLoadNewConfigFields(t *testing.T) {
	// Proves that non-default values for all newly-added config fields across
	// multiple sections are correctly loaded from TOML into their struct fields.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
id = "test"
max_tool_loops = 50
max_output_tokens = 16384

[anthropic]
http_timeout = "180s"
usage_api_timeout = "15s"

[telegram]
message_queue_size = 128
long_poll_timeout = "70s"

[discord]
message_queue_size = 32
facet_session_ttl = "30m"

[http]
graceful_shutdown_timeout = "10s"

[memory]
search_limit = 50

[database]
busy_timeout = "10s"

[tools]
exec_default_timeout = 60
max_summary_chars = 500000
tmux_command_timeout = "10s"
web_fetch_timeout = "45s"
web_fetch_max_bytes = 2097152
web_search_timeout = "20s"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].MaxToolLoops != 50 {
		t.Errorf("Agent.MaxToolLoops = %d, want 50", cfg.Agents[0].MaxToolLoops)
	}
	if cfg.Agents[0].MaxOutputTokens != 16384 {
		t.Errorf("Agent.MaxOutputTokens = %d, want 16384", cfg.Agents[0].MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "180s" {
		t.Errorf("Anthropic.HTTPTimeout = %q, want 180s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "15s" {
		t.Errorf("Anthropic.UsageAPITimeout = %q, want 15s", cfg.Anthropic.UsageAPITimeout)
	}
	if cfg.Telegram.MessageQueueSize != 128 {
		t.Errorf("Telegram.MessageQueueSize = %d, want 128", cfg.Telegram.MessageQueueSize)
	}
	if cfg.Telegram.LongPollTimeout != "70s" {
		t.Errorf("Telegram.LongPollTimeout = %q, want 70s", cfg.Telegram.LongPollTimeout)
	}
	if cfg.Discord.MessageQueueSize != 32 {
		t.Errorf("Discord.MessageQueueSize = %d, want 32", cfg.Discord.MessageQueueSize)
	}
	if cfg.Discord.FacetSessionTTL != "30m" {
		t.Errorf("Discord.FacetSessionTTL = %q, want 30m", cfg.Discord.FacetSessionTTL)
	}
	if cfg.HTTP.GracefulShutdownTimeout != "10s" {
		t.Errorf("HTTP.GracefulShutdownTimeout = %q, want 10s", cfg.HTTP.GracefulShutdownTimeout)
	}
	if cfg.Memory.SearchLimit != 50 {
		t.Errorf("Memory.SearchLimit = %d, want 50", cfg.Memory.SearchLimit)
	}
	if cfg.Database.BusyTimeout != "10s" {
		t.Errorf("Database.BusyTimeout = %q, want 10s", cfg.Database.BusyTimeout)
	}
	if cfg.Tools.ExecDefaultTimeout != 60 {
		t.Errorf("Tools.ExecDefaultTimeout = %d, want 60", cfg.Tools.ExecDefaultTimeout)
	}
	if cfg.Tools.MaxSummaryChars != 500000 {
		t.Errorf("Tools.MaxSummaryChars = %d, want 500000", cfg.Tools.MaxSummaryChars)
	}
	if cfg.Tools.TmuxCommandTimeout != "10s" {
		t.Errorf("Tools.TmuxCommandTimeout = %q, want 10s", cfg.Tools.TmuxCommandTimeout)
	}
	if cfg.Tools.WebFetchTimeout != "45s" {
		t.Errorf("Tools.WebFetchTimeout = %q, want 45s", cfg.Tools.WebFetchTimeout)
	}
	if cfg.Tools.WebFetchMaxBytes != 2097152 {
		t.Errorf("Tools.WebFetchMaxBytes = %d, want 2097152", cfg.Tools.WebFetchMaxBytes)
	}
	if cfg.Tools.WebSearchTimeout != "20s" {
		t.Errorf("Tools.WebSearchTimeout = %q, want 20s", cfg.Tools.WebSearchTimeout)
	}
}

func TestNewConfigDefaults(t *testing.T) {
	// Proves that all newly-added config fields have the expected default values
	// when not explicitly set in the TOML file.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].MaxToolLoops != 25 {
		t.Errorf("default Agent.MaxToolLoops = %d, want 25", cfg.Agents[0].MaxToolLoops)
	}
	if cfg.Agents[0].MaxOutputTokens != 16384 {
		t.Errorf("default Agent.MaxOutputTokens = %d, want 16384", cfg.Agents[0].MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "600s" {
		t.Errorf("default Anthropic.HTTPTimeout = %q, want 600s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "10s" {
		t.Errorf("default Anthropic.UsageAPITimeout = %q, want 10s", cfg.Anthropic.UsageAPITimeout)
	}
	if cfg.Telegram.MessageQueueSize != 64 {
		t.Errorf("default Telegram.MessageQueueSize = %d, want 64", cfg.Telegram.MessageQueueSize)
	}
	if cfg.Telegram.LongPollTimeout != "65s" {
		t.Errorf("default Telegram.LongPollTimeout = %q, want 65s", cfg.Telegram.LongPollTimeout)
	}
	if cfg.Discord.MessageQueueSize != 64 {
		t.Errorf("default Discord.MessageQueueSize = %d, want 64", cfg.Discord.MessageQueueSize)
	}
	if cfg.Discord.FacetSessionTTL != "60m" {
		t.Errorf("default Discord.FacetSessionTTL = %q, want 60m", cfg.Discord.FacetSessionTTL)
	}
	if cfg.Discord.StreamUpdateInterval != "1200ms" {
		t.Errorf("default Discord.StreamUpdateInterval = %q, want 1200ms", cfg.Discord.StreamUpdateInterval)
	}
	if !cfg.Discord.RequireMention {
		t.Error("default Discord.RequireMention should be true")
	}
	if !cfg.Discord.AutoThread {
		t.Error("default Discord.AutoThread should be true")
	}
	if !cfg.Discord.StartupNotify {
		t.Error("default Discord.StartupNotify should be true")
	}
	if cfg.HTTP.GracefulShutdownTimeout != "30s" {
		t.Errorf("default HTTP.GracefulShutdownTimeout = %q, want 30s", cfg.HTTP.GracefulShutdownTimeout)
	}
	if cfg.Memory.SearchLimit != 20 {
		t.Errorf("default Memory.SearchLimit = %d, want 20", cfg.Memory.SearchLimit)
	}
	if cfg.Database.BusyTimeout != "5s" {
		t.Errorf("default Database.BusyTimeout = %q, want 5s", cfg.Database.BusyTimeout)
	}
	if cfg.Tools.ExecDefaultTimeout != 30 {
		t.Errorf("default Tools.ExecDefaultTimeout = %d, want 30", cfg.Tools.ExecDefaultTimeout)
	}
	if cfg.Tools.MaxSummaryChars != 300000 {
		t.Errorf("default Tools.MaxSummaryChars = %d, want 300000", cfg.Tools.MaxSummaryChars)
	}
	if cfg.Tools.TmuxCommandTimeout != "5s" {
		t.Errorf("default Tools.TmuxCommandTimeout = %q, want 5s", cfg.Tools.TmuxCommandTimeout)
	}
	if cfg.Tools.WebFetchTimeout != "30s" {
		t.Errorf("default Tools.WebFetchTimeout = %q, want 30s", cfg.Tools.WebFetchTimeout)
	}
	if cfg.Tools.WebFetchMaxBytes != 1048576 {
		t.Errorf("default Tools.WebFetchMaxBytes = %d, want 1048576", cfg.Tools.WebFetchMaxBytes)
	}
	if cfg.Tools.WebSearchTimeout != "15s" {
		t.Errorf("default Tools.WebSearchTimeout = %q, want 15s", cfg.Tools.WebSearchTimeout)
	}
}

func TestApplyProviderDefaults(t *testing.T) {
	// Proves that ApplyProviderDefaults resolves per-agent provider subsection →
	// global provider config, respects per-agent overrides, and only populates
	// fields relevant to each provider (e.g. effort is anthropic-only).
	cfg := &Config{
		Anthropic: AnthropicConfig{Effort: "low", Thinking: "adaptive"},
		Gemini:    GeminiConfig{Thinking: "adaptive"},
		OpenAI:    OpenAIConfig{Reasoning: "adaptive"},
	}

	// Anthropic agent with no per-agent overrides gets global defaults
	agent := AgentConfig{}
	ApplyProviderDefaults(&agent, "anthropic", cfg)
	if agent.Effort != "low" {
		t.Errorf("anthropic effort = %q, want %q", agent.Effort, "low")
	}
	if agent.Thinking != "adaptive" {
		t.Errorf("anthropic thinking = %q, want %q", agent.Thinking, "adaptive")
	}

	// Per-agent provider subsection overrides global
	agent1b := AgentConfig{
		Anthropic: AgentAnthropicConfig{Effort: "high", Thinking: "off"},
	}
	ApplyProviderDefaults(&agent1b, "anthropic", cfg)
	if agent1b.Effort != "high" {
		t.Errorf("anthropic subsection effort = %q, want %q", agent1b.Effort, "high")
	}
	if agent1b.Thinking != "off" {
		t.Errorf("anthropic subsection thinking = %q, want %q", agent1b.Thinking, "off")
	}

	// Gemini agent gets thinking but not effort
	agent2 := AgentConfig{}
	ApplyProviderDefaults(&agent2, "gemini", cfg)
	if agent2.Effort != "" {
		t.Errorf("gemini effort = %q, want %q", agent2.Effort, "")
	}
	if agent2.Thinking != "adaptive" {
		t.Errorf("gemini thinking = %q, want %q", agent2.Thinking, "adaptive")
	}

	// Gemini per-agent subsection overrides global
	agent2b := AgentConfig{
		Gemini: AgentGeminiConfig{Thinking: "off"},
	}
	ApplyProviderDefaults(&agent2b, "gemini", cfg)
	if agent2b.Thinking != "off" {
		t.Errorf("gemini subsection thinking = %q, want %q", agent2b.Thinking, "off")
	}

	// OpenAI agent gets reasoning mapped to thinking
	agent3 := AgentConfig{}
	ApplyProviderDefaults(&agent3, "openai", cfg)
	if agent3.Effort != "" {
		t.Errorf("openai effort = %q, want %q", agent3.Effort, "")
	}
	if agent3.Thinking != "adaptive" {
		t.Errorf("openai thinking = %q, want %q", agent3.Thinking, "adaptive")
	}

	// OpenAI per-agent subsection overrides global
	agent3b := AgentConfig{
		OpenAI: AgentOpenAIConfig{Reasoning: "off"},
	}
	ApplyProviderDefaults(&agent3b, "openai", cfg)
	if agent3b.Thinking != "off" {
		t.Errorf("openai subsection thinking = %q, want %q", agent3b.Thinking, "off")
	}

	// Runtime field already set — not overwritten
	agent4 := AgentConfig{Effort: "high", Thinking: "off"}
	ApplyProviderDefaults(&agent4, "anthropic", cfg)
	if agent4.Effort != "high" {
		t.Errorf("override effort = %q, want %q", agent4.Effort, "high")
	}
	if agent4.Thinking != "off" {
		t.Errorf("override thinking = %q, want %q", agent4.Thinking, "off")
	}
}

func TestApplyDefaultsReflect(t *testing.T) {
	// Verify that the reflect-based waterfall copies all DefaultsConfig fields.
	// Note: effort and thinking are now in provider sections, not [defaults].
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[llm]
model = "anthropic/claude-opus-4-6"
max_output_tokens = 16384

[defaults]
max_tool_loops = 50
nudge_default_braindead_threshold = 20
nudge_default_braindead_prompt = "watch it"
duplicate_messages = true
inject_agent_warnings = true
compaction_effort = "low"
system_files = ["A.md", "B.md"]

[anthropic]
effort = "high"
thinking = "adaptive"

[[agents]]
id = "bare"

[[agents]]
id = "override"
model = "anthropic/claude-haiku-4-5"

[agents.anthropic]
effort = "low"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	bare := cfg.Agents[0]
	if bare.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("bare Model = %q", bare.Model)
	}
	if bare.MaxToolLoops != 50 {
		t.Errorf("bare MaxToolLoops = %d", bare.MaxToolLoops)
	}
	if bare.MaxOutputTokens != 16384 {
		t.Errorf("bare MaxOutputTokens = %d", bare.MaxOutputTokens)
	}
	if bare.NudgeDefaultBraindeadThreshold != 20 {
		t.Errorf("bare NudgeDefaultBraindeadThreshold = %d", bare.NudgeDefaultBraindeadThreshold)
	}
	if bare.NudgeDefaultBraindeadPrompt != "watch it" {
		t.Errorf("bare NudgeDefaultBraindeadPrompt = %q", bare.NudgeDefaultBraindeadPrompt)
	}
	// Effort/thinking come from provider sections via ApplyProviderDefaults, not [defaults]
	if bare.Effort != "" {
		t.Errorf("bare Effort = %q, want empty (set via ApplyProviderDefaults)", bare.Effort)
	}
	if bare.Thinking != "" {
		t.Errorf("bare Thinking = %q, want empty (set via ApplyProviderDefaults)", bare.Thinking)
	}
	// Verify ApplyProviderDefaults fills them in
	ApplyProviderDefaults(&bare, "anthropic", cfg)
	if bare.Effort != "high" {
		t.Errorf("bare Effort after ApplyProviderDefaults = %q, want high", bare.Effort)
	}
	if bare.Thinking != "adaptive" {
		t.Errorf("bare Thinking after ApplyProviderDefaults = %q, want adaptive", bare.Thinking)
	}
	if !bare.DuplicateMessages {
		t.Error("bare DuplicateMessages should be true")
	}
	if !bare.InjectAgentWarnings {
		t.Error("bare InjectAgentWarnings should be true")
	}
	if bare.CompactionEffort != "low" {
		t.Errorf("bare CompactionEffort = %q", bare.CompactionEffort)
	}
	if len(bare.SystemFiles) != 2 || bare.SystemFiles[0] != "A.md" {
		t.Errorf("bare SystemFiles = %v", bare.SystemFiles)
	}

	// Override agent keeps its own values
	override := cfg.Agents[1]
	if override.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("override Model = %q, want anthropic/claude-haiku-4-5", override.Model)
	}
	if override.Anthropic.Effort != "low" {
		t.Errorf("override Anthropic.Effort = %q, want low", override.Anthropic.Effort)
	}
	// ApplyProviderDefaults resolves subsection → runtime field
	ApplyProviderDefaults(&override, "anthropic", cfg)
	if override.Effort != "low" {
		t.Errorf("override Effort after ApplyProviderDefaults = %q, want low", override.Effort)
	}
	// But inherits defaults for fields it didn't set
	if override.MaxToolLoops != 50 {
		t.Errorf("override MaxToolLoops = %d, want 50 (from defaults)", override.MaxToolLoops)
	}
}

func TestExampleConfigKeysValid(t *testing.T) {
	// Validates that foci.toml.example contains exactly the right config keys.
	// If you add a new config field, this test will fail until you either:
	//   - Add it to foci.toml.example (if users should know about it)
	//   - Add it to exampleSkipPrefixes below (if it's internal or a duplicate section)

	examplePath := filepath.Join("..", "foci.toml.example")
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("foci.toml.example not found: %v", err)
	}

	// Uncomment config lines to get the full key set.
	// Only uncomment lines that look like TOML key=value or section headers.
	tomlKeyValue := regexp.MustCompile(`^[a-z][a-z0-9_]*\s*=`)
	tomlSection := regexp.MustCompile(`^\[+[a-z][a-z0-9_.]*\]+$`)
	var uncommented strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		stripped := ""
		if strings.HasPrefix(trimmed, "# ") {
			stripped = strings.TrimSpace(trimmed[2:])
		} else if strings.HasPrefix(trimmed, "#") && len(trimmed) > 1 {
			stripped = strings.TrimSpace(trimmed[1:])
		}
		if stripped != "" {
			if tomlKeyValue.MatchString(stripped) || tomlSection.MatchString(stripped) {
				uncommented.WriteString(stripped)
			} else {
				uncommented.WriteString(line)
			}
		} else {
			uncommented.WriteString(line)
		}
		uncommented.WriteString("\n")
	}

	// Parse the uncommented example — every key must decode into Config.
	var cfg Config
	meta, err := tomlParser.Decode(uncommented.String(), &cfg)
	if err != nil {
		t.Fatalf("foci.toml.example has invalid TOML after uncommenting: %v", err)
	}

	// Check 1: no unknown keys in the example.
	undecoded := meta.Undecoded()
	if len(undecoded) > 0 {
		var keys []string
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		t.Errorf("foci.toml.example contains keys that don't match any Config field:\n  %s\n"+
			"Fix the key names in the example or add the fields to config structs.",
			strings.Join(keys, "\n  "))
	}

	// Check 2: every Config struct field appears in the example.
	structKeys := collectTOMLKeys(reflect.TypeOf(Config{}), "")

	// The legacy [[agents]] (singular) section has the same fields as [[agents]].
	// The example only shows [[agents]]; skip all agent.* paths.
	exampleSkipPrefixes := []string{"agent."}

	exampleText := string(raw)
	var missing []string
	for _, key := range structKeys {
		skip := false
		for _, prefix := range exampleSkipPrefixes {
			if strings.HasPrefix(key, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Check the leaf key name appears in the raw file (commented or not).
		parts := strings.Split(key, ".")
		leaf := parts[len(parts)-1]
		if !strings.Contains(exampleText, leaf) {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Config fields missing from foci.toml.example:\n  %s\n"+
			"Add them to the example file, or add to exampleSkipKeys with a reason.",
			strings.Join(missing, "\n  "))
	}
}

func TestMemorySourcesInheritance(t *testing.T) {
	// Proves that global memory sources are prepended to each agent's source list,
	// with the agent's own source appended last, and that explicit per-agent sources
	// replace the default agent source while still including global ones.
	t.Run("global sources prepended to agent default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[memory.sources]]
name = "shared"
dir = "/shared/memory"
weight = 0.5

[[agents]]
id = "clutch"
workspace = "/ws/clutch"
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 2 {
			t.Fatalf("sources len = %d, want 2", len(sources))
		}
		if sources[0].Name != "shared" || sources[0].Dir != "/shared/memory" || sources[0].Weight != 0.5 {
			t.Errorf("sources[0] = %+v, want shared source", sources[0])
		}
		if sources[1].Name != "clutch" || sources[1].Weight != 1.0 {
			t.Errorf("sources[1] = %+v, want agent default source", sources[1])
		}
	})

	t.Run("global sources prepended to agent explicit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[memory.sources]]
name = "shared"
dir = "/shared/memory"
weight = 0.5

[[agents]]
id = "clutch"

[[agents.memory.sources]]
name = "custom"
dir = "/custom/memory"
weight = 0.8
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 2 {
			t.Fatalf("sources len = %d, want 2", len(sources))
		}
		if sources[0].Name != "shared" {
			t.Errorf("sources[0].Name = %q, want shared", sources[0].Name)
		}
		if sources[1].Name != "custom" || sources[1].Weight != 0.8 {
			t.Errorf("sources[1] = %+v, want custom source", sources[1])
		}
	})

	t.Run("no global sources only agent default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[agents]]
id = "clutch"
workspace = "/ws/clutch"
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 1 {
			t.Fatalf("sources len = %d, want 1", len(sources))
		}
		if sources[0].Name != "clutch" {
			t.Errorf("sources[0].Name = %q, want clutch", sources[0].Name)
		}
	})
}

func TestDataDirDefault(t *testing.T) {
	// Proves that when data_dir is not set, it defaults to ~/data (relative to the
	// user's home directory).
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[[agents]]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "data")
	if cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

func TestDataDirExplicitNotOverridden(t *testing.T) {
	// Proves that an explicitly-configured data_dir is preserved and not replaced
	// by the default home-relative path during Load.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
data_dir = "/opt/foci/data"

[[agents]]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.DataDir != "/opt/foci/data" {
		t.Errorf("DataDir = %q, want /opt/foci/data", cfg.DataDir)
	}
}
