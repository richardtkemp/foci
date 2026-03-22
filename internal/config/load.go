package config

import (
	"fmt"
	"os"
	"path/filepath"

	"regexp"
	"strings"
	"time"
	"unicode"

	"foci/internal/log"

	"github.com/BurntSushi/toml"
)

// boolKeyLineRe matches a TOML key = "on"/"off"/"true"/"false" line,
// capturing the key name, the equals sign, the quoted value, and trailing comment.
var boolKeyLineRe = regexp.MustCompile(`(?m)^(\s*(\w+)\s*=\s*)"(?i)(on|off|true|false)"(\s*(?:#.*)?)$`)

// boolKeys is the set of TOML keys that are bool-typed in the config structs.
// Only these keys have their quoted string values normalized to native bools.
var boolKeys = map[string]bool{
	"duplicate_messages":    true,
	"startup_notify":        true,
	"messages_in_log":       true,
	"compaction_notify":     true,
	"compaction_debug":      true,
	"log_api_key_suffix":    true,
	"tmux_autopilot":        true,
	"auto_refresh":          true,
	"enable_stop_aliases":   true,
	"full_payload":          true,
	"cache_bust_detect":     true,
	"log_rotation":          true,
	"ws_enabled":            true,
	"enabled":               true,
	"skip_security_checks":  true,
	"use_sdk":               true,
	"streaming":             true,
	"interval_enabled":      true,
	"consolidation_enabled": true,
	"session_end_enabled":   true,
	"steer_mode":            true,
	"nudge_enable":           true,
	"nudge_auto_extract":     true,
	"nudge_pre_answer_gate":  true,
	"nudge_default_enable":   true,
	"browser_enabled":       true,
	"headless":              true,
	"incognito":                          true,
	"autocompact_before_mana_refresh":     true,
}

// normalizeBoolStrings preprocesses TOML content to convert quoted bool-like
// strings ("on"/"off"/"true"/"false") to native TOML booleans for known bool
// keys. This allows users to write `enabled = "on"` as an alias for
// `enabled = true`. Only applies to keys in the boolKeys set — string fields
// like `thinking = "off"` are not affected.
func normalizeBoolStrings(data string) string {
	return boolKeyLineRe.ReplaceAllStringFunc(data, func(match string) string {
		sub := boolKeyLineRe.FindStringSubmatch(match)
		if len(sub) < 5 {
			return match
		}
		prefix := sub[1] // "  key = " including whitespace
		key := sub[2]    // the key name
		val := sub[3]    // on/off/true/false
		trail := sub[4]  // trailing comment

		if !boolKeys[key] {
			return match // not a bool key, leave as-is
		}

		switch strings.ToLower(val) {
		case "on", "true":
			return prefix + "true" + trail
		case "off", "false":
			return prefix + "false" + trail
		default:
			return match
		}
	})
}

// agentDefinedFields parses TOML metadata keys to determine which fields each
// agentDefinedFields is no longer needed — defaults resolve at use time via Merge[T].

// ensureAgentPlatform ensures an agent has a platform entry for the given ID.
// If absent, creates one with sensible defaults (bot = agent ID, received_files_dir).
func ensureAgentPlatform(agent *AgentConfig, platformID string, _ *Config) {
	p := agent.Platform(platformID)
	if p == nil {
		agent.Platforms = append(agent.Platforms, PlatformConfig{ID: platformID})
		p = &agent.Platforms[len(agent.Platforms)-1]
	}
	if p.Bot == "" {
		p.Bot = agent.ID
	}
	dir := filepath.Join(agent.Workspace, "received_files")
	if p.ReceivedFilesDir == nil {
		p.ReceivedFilesDir = &dir
	}
}

// PlatformDefaulter returns the default PlatformConfig for a platform ID.
// Used as a callback from main.go to avoid config importing platform.
type PlatformDefaulter func(id string) *PlatformConfig

// ApplyProviderDefaults applies provider-driven defaults to all platform configs.
// Called from main.go after both config and platform packages are initialised.
func ApplyProviderDefaults(cfg *Config, getDefaults PlatformDefaulter) {
	for i := range cfg.Platforms {
		if defaults := getDefaults(cfg.Platforms[i].ID); defaults != nil {
			cfg.Platforms[i].ApplyDefaults(*defaults)
		}
	}
	for i := range cfg.Agents {
		for j := range cfg.Agents[i].Platforms {
			agentPlat := &cfg.Agents[i].Platforms[j]
			// Merge: agent platform < global platform
			if globalPlat := cfg.Platform(agentPlat.ID); globalPlat != nil {
				agentPlat.ApplyDefaults(*globalPlat)
			}
			// Then apply provider defaults for anything still unset
			if defaults := getDefaults(agentPlat.ID); defaults != nil {
				agentPlat.ApplyDefaults(*defaults)
			}
		}
	}
}


// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	md, err := toml.Decode(normalizeBoolStrings(string(data)), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Collect unknown config keys for the caller to log after logging is
	// fully initialised (the early log init gets rotated away on startup).
	cfg.UndefinedKeys = UnknownKeys(md)

	// Record which keys were explicitly set in the TOML file.
	cfg.DefinedKeys = make(map[string]bool)
	for _, key := range md.Keys() {
		cfg.DefinedKeys[strings.Join(key, ".")] = true
	}

	// Defaults are now resolved at use time via Merge[T] — no load-time copying needed.
	// Resolve agent path fields.
	for i := range cfg.Agents {
		ResolvePathPtr(cfg.Agents[i].Sessions.BranchOrientationFacetPrompt)
		ResolvePathPtr(cfg.Agents[i].Sessions.BranchOrientationHeadlessPrompt)
	}

	// Endpoint defaults — only create built-in defaults for endpoints that
	// model groups resolve to. This avoids spurious "missing secret" warnings
	// for endpoints the user doesn't use (e.g. openai.api_key when no group
	// references OpenAI).
	usedEndpoints := make(map[string]bool)
	for _, groupModel := range []string{DerefStr(cfg.Groups.Powerful), DerefStr(cfg.Groups.Fast), DerefStr(cfg.Groups.Cheap)} {
		if groupModel == "" {
			continue
		}
		resolved, err := ResolveModel(groupModel, "", cfg.Models)
		if err == nil {
			usedEndpoints[resolved.Endpoint] = true
		}
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = make(map[string]EndpointConfig)
	}
	type epDefault struct {
		name string
		cfg  EndpointConfig
	}
	for _, d := range []epDefault{
		{"anthropic", EndpointConfig{Format: "anthropic", APIKey: "anthropic.api_key"}},
		{"gemini", EndpointConfig{Format: "gemini", APIKey: "gemini.api_key"}},
		{"openai", EndpointConfig{Format: "openai", APIKey: "openai.api_key"}},
		{"openrouter", EndpointConfig{
			AnthropicURL: "https://openrouter.ai/api/v1",
			OpenAIURL:    "https://openrouter.ai/api/v1",
			APIKey:       "openrouter.api_key",
		}},
	} {
		if usedEndpoints[d.name] {
			if _, ok := cfg.Endpoints[d.name]; !ok {
				cfg.Endpoints[d.name] = d.cfg
			}
		}
	}

	// CompactionConfig fields are now pointer-typed in CompactionConfig.
	// Defaults are resolved at use time via Merge + code defaults.
	setIntDefault(&cfg.Sessions.CompactionMaxTokens, 4096)
	setIntDefault(&cfg.Sessions.CompactionMinMessages, 4)

	// Apply debug.log_api_key_suffix to the log package global.
	log.DebugLogKeySuffix = DerefBool(cfg.Debug.LogAPIKeySuffix)

	setStringDefault(&cfg.Sessions.ArchiveAfter, "24h")
	setIntDefault(&cfg.HTTP.Port, 18791)
	setStringDefault(&cfg.HTTP.Bind, "127.0.0.1")
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, "data")
	}
	setStringDefault(&cfg.Logging.Level, "INFO")
	setStringDefault(&cfg.Logging.EventFile, "logs/foci.log")
	setStringDefault(&cfg.Logging.APIFile, "logs/api.jsonl")
	if cfg.Logging.FullPayload && cfg.Logging.PayloadFile == "" {
		cfg.Logging.PayloadFile = "logs/api-payload.jsonl"
	}
	setStringDefaultDefined(&cfg.Logging.APIDB, cfg.DataPath("api.db"), md.IsDefined("logging", "api_db"))
	setIntDefaultDefined(&cfg.Logging.CacheBustIdleMinutes, 10, md.IsDefined("logging", "cache_bust_idle_minutes"))
	setIntDefaultDefined(&cfg.Logging.WarningMaxPerWindow, 3, md.IsDefined("logging", "warning_max_per_window"))
	setStringDefault(&cfg.Logging.WarningWindowDuration, "5m")
	setStringDefault(&cfg.Logging.WarningProactiveActiveInterval, "5m")
	setStringDefault(&cfg.Logging.WarningProactiveInactiveInterval, "1h")
	setStringDefault(&cfg.Logging.WarningProactiveActivityThreshold, "10m")
	setBoolDefaultDefined(&cfg.Logging.LogRotation, true, md.IsDefined("logging", "log_rotation"))
	setStringDefault(&cfg.Logging.RotationPeriod, "24h")
	setStringDefault(&cfg.Logging.RetentionPeriod, "48h")
	setStringDefault(&cfg.Logging.RotationMaxLineSize, "64MB")
	setStringDefault(&cfg.Logging.LogFileMode, "0600")
	// Resources defaults
	setBoolDefaultDefined(&cfg.Resources.MemoryGuardEnabled, true, md.IsDefined("resources", "memory_guard_enabled"))
	setStringDefault(&cfg.Resources.MemoryGuardInterval, "60s")
	setIntDefaultDefined(&cfg.Resources.MemoryWarnPercent, 25, md.IsDefined("resources", "memory_warn_percent"))
	setIntDefaultDefined(&cfg.Resources.MemoryKillPercent, 40, md.IsDefined("resources", "memory_kill_percent"))
	setFloatDefaultDefined(&cfg.Resources.MemoryPressureThreshold, 10.0, md.IsDefined("resources", "memory_pressure_threshold"))
	setStringDefault(&cfg.Resources.GoroutineMonitorInterval, "60s")
	// GoroutineMonitorThreshold: 0 means auto (30 + 25×agents + 5×telegram_bots), computed at startup.
	// Bitwarden defaults
	setStringDefault(&cfg.Bitwarden.SessionFile, "/home/bitwarden/.bw_session")
	setStringDefault(&cfg.Bitwarden.RefreshInterval, "15m")
	setStringDefault(&cfg.Bitwarden.SecretTTL, "30m")
	setStringDefault(&cfg.Bitwarden.CleanupInterval, "1m")

	setStringDefault(&cfg.Cache.Strategy, "auto")
	setStringDefault(&cfg.Cache.TTL, "1h")
	if cfg.Mana.Name == nil {
		cfg.Mana.Name = Ptr[string]("mana")
	}
	if cfg.Mana.InvestInterval == nil {
		cfg.Mana.InvestInterval = Ptr[string]("30m")
	}
	// SummaryConfig fields (MaxResultChars etc.) are now pointers — defaults at use time via Merge
	setStringDefault(&cfg.Tools.TempDir, "/tmp/foci/tool-results")
	setIntDefault(&cfg.Tools.TmuxCols, 300)
	setIntDefault(&cfg.Tools.TmuxRows, 30)
	// ToolConfig fields are pointers — nil means "not set" (no IsDefined needed).
	if cfg.Tools.ExecAutoBackground == nil {
		cfg.Tools.ExecAutoBackground = Ptr[int](10)
	}
	// AutoSummarise now in SummaryConfig — default at use time
	if cfg.Tools.TmuxAutopilot == nil {
		cfg.Tools.TmuxAutopilot = Ptr[bool](true)
	}
	if cfg.Tools.TmuxWatchThreshold == nil {
		cfg.Tools.TmuxWatchThreshold = Ptr[string]("30s")
	}
	if cfg.Tools.TmuxSessionTTL == nil {
		cfg.Tools.TmuxSessionTTL = Ptr[string]("24h")
	}
	if cfg.Tools.SearchProvider == nil {
		cfg.Tools.SearchProvider = Ptr[string]("brave")
	}
	if cfg.Tools.FetchProvider == nil {
		cfg.Tools.FetchProvider = Ptr[string]("builtin")
	}
	if len(cfg.Behavior.StopAliases) == 0 {
		cfg.Behavior.StopAliases = []string{"stop", "wait"}
	}
	setStringDefault(&cfg.WelcomeFile, "data/WELCOME.md")
	if len(cfg.Memory.SearchBackends) == 0 {
		cfg.Memory.SearchBackends = []string{"bleve"}
	}
	setFloatDefault(&cfg.Memory.ConversationWeight, 0.1)
	setIntDefault(&cfg.Memory.SearchLimit, 20)
	setStringDefault(&cfg.Memory.SweepInterval, "1h")

	// Database defaults
	setStringDefault(&cfg.Database.BusyTimeout, "5s")

	// Anthropic defaults
	setStringDefault(&cfg.Anthropic.HTTPTimeout, "600s") // 10 min — thinking responses can take several minutes
	setStringDefault(&cfg.Anthropic.UsageAPITimeout, "10s")
	setStringDefault(&cfg.Anthropic.UsageCacheTTL, "10m")
	setStringDefault(&cfg.Anthropic.CCExpiryThreshold, "5m")
	setBoolDefaultDefined(&cfg.Anthropic.UseSDK, true, md.IsDefined("anthropic", "use_sdk"))

	// Gemini defaults
	setStringDefault(&cfg.Gemini.HTTPTimeout, "120s")
	setStringDefault(&cfg.Gemini.CacheTTL, "1h")

	// OpenAI defaults
	setStringDefault(&cfg.OpenAI.HTTPTimeout, "120s")

	// Tools defaults
	setIntDefault(&cfg.Tools.ExecDefaultTimeout, 30)
	// MaxSummaryChars now in SummaryConfig — default at use time
	setStringDefault(&cfg.Tools.TmuxCommandTimeout, "5s")
	setStringDefault(&cfg.Tools.WebFetchTimeout, "30s")
	setIntDefault(&cfg.Tools.WebFetchMaxBytes, 1048576) // 1MB
	setStringDefault(&cfg.Tools.WebSearchTimeout, "15s")
	if cfg.Tools.MaxConcurrentSpawns == nil {
		cfg.Tools.MaxConcurrentSpawns = Ptr[int](3)
	}
	if cfg.Tools.ExploreMaxDepth == nil {
		cfg.Tools.ExploreMaxDepth = Ptr[int](100)
	}
	if cfg.Tools.MaxUploadFileSize == nil {
		cfg.Tools.MaxUploadFileSize = Ptr[int64](50 * 1024 * 1024) // 50MB
	}
	setIntDefault(&cfg.Tools.ToolCallPreviewChars, 450)
	setStringDefault(&cfg.Tools.TmuxMemoryCheckInterval, "5m")
	setStringDefault(&cfg.Tools.TmuxMemoryWarn, "10%")
	setStringDefault(&cfg.Tools.TmuxMemoryCritical, "20%")
	setStringDefault(&cfg.Tools.TmuxMemoryKill, "30%")
	// SummaryContextTurns, SummaryContextChars, MaxSummaryInputChars, MaxImagePixels
	// now in SummaryConfig — defaults at use time

	// Browser defaults (BrowserConfig is now top-level with all-pointer fields)
	if cfg.Browser.Enabled == nil {
		cfg.Browser.Enabled = Ptr[bool](true)
	}
	if cfg.Browser.Headless == nil {
		cfg.Browser.Headless = Ptr[bool](true)
	}
	if cfg.Browser.TimeoutSec == nil {
		cfg.Browser.TimeoutSec = Ptr[int](30)
	}
	if cfg.Browser.Incognito == nil {
		cfg.Browser.Incognito = Ptr[bool](true)
	}
	if cfg.Browser.DOMStableSec == nil {
		cfg.Browser.DOMStableSec = Ptr[float64](1.0)
	}
	if cfg.Browser.DOMStableDiff == nil {
		cfg.Browser.DOMStableDiff = Ptr[float64](0.2)
	}

	// HTTP defaults
	setStringDefault(&cfg.HTTP.GracefulShutdownTimeout, "30s")

	// Bool defaults: default to true unless explicitly set to false in config.
	setBoolDefaultDefined(&cfg.Environment.Enabled, true, md.IsDefined("environment", "enabled"))
	setStringDefault(&cfg.Environment.DocsPath, "shared/docs")
	// EnableStopAliases now in BehaviorConfig — code default at use time

	// Keepalive/background defaults (pointer — resolved via Merge at use time)
	if cfg.Keepalive.Interval == nil {
		cfg.Keepalive.Interval = Ptr[string]("55m")
	}
	if cfg.Background.Interval == nil {
		cfg.Background.Interval = Ptr[string]("15m")
	}

	// Memory formation defaults
	if cfg.MemoryFormation.Interval == nil {
		cfg.MemoryFormation.Interval = Ptr[string]("1h")
	}
	if cfg.MemoryFormation.ConsolidationInterval == nil {
		cfg.MemoryFormation.ConsolidationInterval = Ptr[string]("20h")
	}
	// IntervalEnabled/ConsolidationEnabled/SessionEndEnabled: nil = true (resolved at runtime)

	// Keepalive/background/memory-formation: resolved via Merge at use time (no load-time copying)

	// Apply convention-based defaults before path resolution.
	for i := range cfg.Agents {
		// Workspace default: $HOME/$id
		if cfg.Agents[i].Workspace == "" {
			home, _ := os.UserHomeDir()
			cfg.Agents[i].Workspace = filepath.Join(home, cfg.Agents[i].ID)
		}
		// Platform defaults: ensure each agent has platform entries for
		// configured platforms. Bot name defaults to agent ID, received_files_dir
		// defaults to $workspace/received_files.
		ensureAgentPlatform(&cfg.Agents[i], "telegram", &cfg)
		// Discord: only auto-create if discord is configured globally.
		if p := cfg.Platform("discord"); p != nil && len(p.AllowedUsers) > 0 {
			ensureAgentPlatform(&cfg.Agents[i], "discord", &cfg)
		}

		// Name default: capitalised ID (e.g. "clutch" → "Clutch")
		if cfg.Agents[i].Name == "" && cfg.Agents[i].ID != "" {
			r := []rune(cfg.Agents[i].ID)
			r[0] = unicode.ToUpper(r[0])
			cfg.Agents[i].Name = string(r)
		}
		// Memory sources: prepend global sources, then agent-specific (or default).
		// Per docstring, agent sources are "combined with global [memory] sources."
		agentSources := cfg.Agents[i].Memory.Sources
		if len(agentSources) == 0 {
			agentSources = []MemorySource{{
				Name:   cfg.Agents[i].ID,
				Dir:    filepath.Join(cfg.Agents[i].Workspace, "memory"),
				Weight: 1.0,
			}}
		}
		if len(cfg.Memory.Sources) > 0 {
			combined := make([]MemorySource, 0, len(cfg.Memory.Sources)+len(agentSources))
			combined = append(combined, cfg.Memory.Sources...)
			combined = append(combined, agentSources...)
			cfg.Agents[i].Memory.Sources = combined
		} else {
			cfg.Agents[i].Memory.Sources = agentSources
		}
	}

	cfg.ResolveAllPaths()

	// Keepalive/background validation warnings
	if DerefBool(cfg.Background.Enabled) && DerefBool(cfg.Keepalive.Enabled) {
		bgInt, _ := time.ParseDuration(DerefStr(cfg.Background.Interval))
		kaInt, _ := time.ParseDuration(DerefStr(cfg.Keepalive.Interval))
		if bgInt > 0 && kaInt > 0 && bgInt > kaInt {
			log.Warnf("config", "[background] interval %s > [keepalive] interval %s — keepalive resets cache timer, background work may never trigger", DerefStr(cfg.Background.Interval), DerefStr(cfg.Keepalive.Interval))
		}
	}
	if DerefBool(cfg.Keepalive.Enabled) {
		kaInt, _ := time.ParseDuration(DerefStr(cfg.Keepalive.Interval))
		if kaInt > time.Hour {
			log.Warnf("config", "[keepalive] interval %s > 1h — Anthropic cache TTL is 1 hour, cache may expire between keepalives", DerefStr(cfg.Keepalive.Interval))
		}
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}
