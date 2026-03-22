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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[agents.agent_loop]
max_tool_loops = 50
max_output_tokens = 16384

[anthropic]
http_timeout = "180s"
usage_api_timeout = "15s"

[[platforms]]
id = "telegram"
message_queue_size = 128
[platforms.telegram]
long_poll_timeout = "70s"

[[platforms]]
id = "discord"
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

	if DerefInt(cfg.Agents[0].AgentLoop.MaxToolLoops) != 50 {
		t.Errorf("Agent.AgentLoop.MaxToolLoops = %d, want 50", cfg.Agents[0].AgentLoop.MaxToolLoops)
	}
	if DerefInt(cfg.Agents[0].AgentLoop.MaxOutputTokens) != 16384 {
		t.Errorf("Agent.AgentLoop.MaxOutputTokens = %d, want 16384", cfg.Agents[0].AgentLoop.MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "180s" {
		t.Errorf("Anthropic.HTTPTimeout = %q, want 180s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "15s" {
		t.Errorf("Anthropic.UsageAPITimeout = %q, want 15s", cfg.Anthropic.UsageAPITimeout)
	}
	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil {
		t.Fatal("Platform(telegram) = nil")
	}
	if tgPlat.MessageQueueSize != 128 {
		t.Errorf("Platform(telegram).MessageQueueSize = %d, want 128", tgPlat.MessageQueueSize)
	}
	if tgPlat.Telegram == nil || tgPlat.Telegram.LongPollTimeout != "70s" {
		t.Errorf("Platform(telegram).LongPollTimeout = %v, want 70s", tgPlat.Telegram)
	}
	dcPlat := cfg.Platform("discord")
	if dcPlat == nil {
		t.Fatal("Platform(discord) = nil")
	}
	if dcPlat.MessageQueueSize != 32 {
		t.Errorf("Platform(discord).MessageQueueSize = %d, want 32", dcPlat.MessageQueueSize)
	}
	if dcPlat.FacetSessionTTL != "30m" {
		t.Errorf("Platform(discord).FacetSessionTTL = %q, want 30m", dcPlat.FacetSessionTTL)
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
	if DerefInt(cfg.Tools.MaxSummaryChars) != 500000 {
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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// MaxToolLoops and MaxOutputTokens are nil on agent when unset — defaults resolve at use time
	if cfg.Agents[0].AgentLoop.MaxToolLoops != nil {
		t.Errorf("default Agent.AgentLoop.MaxToolLoops should be nil (use-time resolution), got %v", cfg.Agents[0].AgentLoop.MaxToolLoops)
	}
	if cfg.Agents[0].AgentLoop.MaxOutputTokens != nil {
		t.Errorf("default Agent.AgentLoop.MaxOutputTokens should be nil (use-time resolution), got %v", cfg.Agents[0].AgentLoop.MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "600s" {
		t.Errorf("default Anthropic.HTTPTimeout = %q, want 600s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "10s" {
		t.Errorf("default Anthropic.UsageAPITimeout = %q, want 10s", cfg.Anthropic.UsageAPITimeout)
	}
	// Platform defaults (message_queue_size, long_poll_timeout, etc.) are now
	// provider-driven via ApplyProviderDefaults, not hardcoded in Load().
	// They are tested in the provider packages.
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
	if cfg.Tools.MaxSummaryChars != nil {
		t.Errorf("default Tools.MaxSummaryChars should be nil (code default at use time), got %v", cfg.Tools.MaxSummaryChars)
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


func TestApplyDefaultsReflect(t *testing.T) {
	// Verify that the reflect-based waterfall copies all config section fields.
	// Note: effort and thinking are now per-model in [models.<name>], not global sections.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-opus-4-6"

[agent_loop]
max_output_tokens = 16384
max_tool_loops = 50
duplicate_messages = true

[nudge]
nudge_default_braindead_threshold = 20
nudge_default_braindead_prompt = "watch it"

[system]
system_files = ["A.md", "B.md"]

[debug]
inject_agent_warnings = true

[sessions]
compaction_effort = "low"

[[agents]]
id = "bare"

[[agents]]
id = "override"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Bare agent has nil fields — values resolve via Merge with defaults at use time.
	bare := cfg.Agents[0]
	if bare.AgentLoop.MaxToolLoops != nil {
		t.Errorf("bare AgentLoop.MaxToolLoops should be nil, got %v", bare.AgentLoop.MaxToolLoops)
	}

	// Verify global config sections were parsed correctly
	if DerefInt(cfg.AgentLoop.MaxToolLoops) != 50 {
		t.Errorf("AgentLoop.MaxToolLoops = %v, want 50", cfg.AgentLoop.MaxToolLoops)
	}
	if DerefInt(cfg.AgentLoop.MaxOutputTokens) != 16384 {
		t.Errorf("AgentLoop.MaxOutputTokens = %v, want 16384", cfg.AgentLoop.MaxOutputTokens)
	}
	if DerefInt(cfg.Nudge.NudgeDefaultBraindeadThreshold) != 20 {
		t.Errorf("Nudge.NudgeDefaultBraindeadThreshold = %v, want 20", cfg.Nudge.NudgeDefaultBraindeadThreshold)
	}
	if DerefStr(cfg.Nudge.NudgeDefaultBraindeadPrompt) != "watch it" {
		t.Errorf("Nudge.NudgeDefaultBraindeadPrompt = %v", cfg.Nudge.NudgeDefaultBraindeadPrompt)
	}
	if !DerefBool(cfg.AgentLoop.DuplicateMessages) {
		t.Error("AgentLoop.DuplicateMessages should be true")
	}
	if cfg.Debug.InjectAgentWarnings == nil || *cfg.Debug.InjectAgentWarnings != InjectionAll {
		t.Errorf("Debug.InjectAgentWarnings = %v, want %q", cfg.Debug.InjectAgentWarnings, InjectionAll)
	}
	if len(cfg.System.SystemFiles) != 2 || cfg.System.SystemFiles[0] != "A.md" {
		t.Errorf("System.SystemFiles = %v", cfg.System.SystemFiles)
	}

	// Merge resolves bare agent fields from defaults
	al := Merge(bare.AgentLoop, cfg.AgentLoop)
	if DerefInt(al.MaxToolLoops) != 50 {
		t.Errorf("Merge resolved MaxToolLoops = %v, want 50", al.MaxToolLoops)
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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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

[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
