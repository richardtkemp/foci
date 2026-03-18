package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	"inject_agent_warnings": true,
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
// [[agents]] array element explicitly defines. Returns a slice (one entry per
// agent) of sets of TOML field names.
func agentDefinedFields(md toml.MetaData) []map[string]bool {
	var result []map[string]bool
	var current map[string]bool

	for _, key := range md.Keys() {
		parts := []string(key)
		if len(parts) == 0 || parts[0] != "agents" {
			continue
		}
		if len(parts) == 1 {
			// Start of a new [[agents]] block
			current = make(map[string]bool)
			result = append(result, current)
			continue
		}
		if current != nil {
			current[parts[1]] = true
		}
	}
	return result
}

// platformDisplayFields is implemented by platform configs that carry display overrides.
type platformDisplayFields interface {
	getShowToolCalls() *ToolCallDisplay
	setShowToolCalls(*ToolCallDisplay)
	getShowThinking() *ShowThinking
	setShowThinking(*ShowThinking)
}

// syncPlatformDisplayFields copies agent-level display fields (ShowToolCalls, ShowThinking)
// to/from a platform config so both levels stay in sync.
func syncPlatformDisplayFields(acfg *AgentConfig, pf platformDisplayFields) {
	if pf == nil {
		return
	}
	// Forward: agent -> platform (so the bot inherits agent display prefs).
	if acfg.ShowToolCalls != nil && pf.getShowToolCalls() == nil {
		pf.setShowToolCalls(acfg.ShowToolCalls)
	}
	if acfg.ShowThinking != nil && pf.getShowThinking() == nil {
		pf.setShowThinking(acfg.ShowThinking)
	}
	// Reverse: platform -> agent (so generic code sees platform overrides).
	if pf.getShowToolCalls() != nil && acfg.ShowToolCalls == nil {
		acfg.ShowToolCalls = pf.getShowToolCalls()
	}
	if pf.getShowThinking() != nil && acfg.ShowThinking == nil {
		acfg.ShowThinking = pf.getShowThinking()
	}
}

// syncDisplayFields syncs agent-level display fields with all platform configs.
func syncDisplayFields(acfg *AgentConfig) {
	if acfg.Platforms == nil {
		return
	}
	if acfg.Platforms.Telegram != nil {
		syncPlatformDisplayFields(acfg, acfg.Platforms.Telegram)
	}
	if acfg.Platforms.Discord != nil {
		syncPlatformDisplayFields(acfg, acfg.Platforms.Discord)
	}
}

// applyStructToAgent copies fields from a source struct to agent where the
// agent field is zero-value and was not explicitly set in the TOML file.
// Fields are matched by TOML tag name between the source and AgentConfig.
func applyStructToAgent(agent *AgentConfig, source any, defined map[string]bool) {
	dv := reflect.ValueOf(source).Elem()
	dt := dv.Type()
	av := reflect.ValueOf(agent).Elem()
	at := av.Type()

	// Build AgentConfig field index by TOML tag
	agentFieldByTag := make(map[string]int, at.NumField())
	for i := 0; i < at.NumField(); i++ {
		tag := at.Field(i).Tag.Get("toml")
		if tag != "" && tag != "-" {
			agentFieldByTag[tag] = i
		}
	}

	for i := 0; i < dt.NumField(); i++ {
		tag := dt.Field(i).Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}

		ai, ok := agentFieldByTag[tag]
		if !ok {
			continue // source field has no matching AgentConfig field
		}

		af := av.Field(ai)
		df := dv.Field(i)

		// Skip if agent explicitly defined this field in TOML
		if defined[tag] {
			continue
		}

		// Skip if agent value is already non-zero
		if !af.IsZero() {
			continue
		}

		// Skip if default is also zero (nothing to copy)
		if df.IsZero() {
			continue
		}

		af.Set(df)
	}
}

// applyDefaultsToAgent copies fields from defaults and LLM config to agent
// where the agent field is zero-value and was not explicitly set in the TOML file.
// Fields are matched by TOML tag name between the source and AgentConfig.
func applyDefaultsToAgent(agent *AgentConfig, cfg *Config, defined map[string]bool) {
	applyStructToAgent(agent, &cfg.LLM, defined)
	applyStructToAgent(agent, &cfg.Defaults, defined)
}

// ApplyProviderDefaults fills in agent Effort/Thinking/Speed from per-agent
// provider subsections and global provider config. Resolution order:
// per-agent provider subsection → global provider config → empty.
// Call after model resolution so `format` is known.
func ApplyProviderDefaults(agent *AgentConfig, format string, cfg *Config) {
	switch format {
	case "anthropic":
		if agent.Effort == "" {
			agent.Effort = firstNonEmpty(agent.Anthropic.Effort, cfg.Anthropic.Effort)
		}
		if agent.Thinking == "" {
			agent.Thinking = firstNonEmpty(agent.Anthropic.Thinking, cfg.Anthropic.Thinking)
		}
		if agent.Speed == "" {
			agent.Speed = firstNonEmpty(agent.Anthropic.Speed, cfg.Anthropic.Speed)
		}
	case "gemini":
		if agent.Thinking == "" {
			agent.Thinking = firstNonEmpty(agent.Gemini.Thinking, cfg.Gemini.Thinking)
		}
	case "openai":
		if agent.Thinking == "" {
			agent.Thinking = firstNonEmpty(agent.OpenAI.Reasoning, cfg.OpenAI.Reasoning)
		}
	}
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

	// Populate [defaults] section with hardcoded fallbacks.
	// All defaults must be set BEFORE applyDefaultsToAgent so the reflection-based
	// copier propagates them to agents automatically — no manual fallback needed.
	setStringDefault(&cfg.LLM.Model, "anthropic/claude-haiku-4-5-20251001")
	setIntDefault(&cfg.LLM.MaxOutputTokens, 16384)
	setIntDefault(&cfg.Defaults.MaxToolLoops, 25)
	setIntDefaultDefined(&cfg.Defaults.BraindeadThreshold, 10, md.IsDefined("defaults", "braindead_threshold"))
	setIntDefaultDefined(&cfg.Defaults.NudgeCooldown, 5, md.IsDefined("defaults", "nudge_cooldown"))
	setIntDefaultDefined(&cfg.Defaults.NudgeMaxPerBatch, 1, md.IsDefined("defaults", "nudge_max_per_batch"))
	setIntDefaultDefined(&cfg.Defaults.NudgePreAnswerMinTools, 2, md.IsDefined("defaults", "nudge_pre_answer_min_tools"))
	setStringDefault(&cfg.Defaults.TurnLockWarnThreshold, "3m")
	if cfg.Telegram.ShowToolCalls == nil {
		v := ToolCallOff
		cfg.Telegram.ShowToolCalls = &v
	}
	if cfg.Telegram.ShowThinking == nil {
		v := ShowThinkingOff
		cfg.Telegram.ShowThinking = &v
	}
	setStringDefaultDefined(&cfg.Defaults.InjectedMessageHeader, "[[ System message ]]", md.IsDefined("defaults", "injected_message_header"))
	setBoolDefaultDefined(&cfg.Defaults.SteerMode, true, md.IsDefined("defaults", "steer_mode"))
	setBoolDefaultDefined(&cfg.Defaults.NudgeEnable, true, md.IsDefined("defaults", "nudge_enable"))
	setBoolDefaultDefined(&cfg.Defaults.NudgeAutoExtract, true, md.IsDefined("defaults", "nudge_auto_extract"))
	setStringDefault(&cfg.Telegram.StreamUpdateInterval, "250ms")

	// Discord global defaults
	if cfg.Discord.ShowToolCalls == nil {
		v := ToolCallOff
		cfg.Discord.ShowToolCalls = &v
	}
	if cfg.Discord.ShowThinking == nil {
		v := ShowThinkingOff
		cfg.Discord.ShowThinking = &v
	}
	setStringDefault(&cfg.Discord.StreamUpdateInterval, "1200ms")
	setStringDefault(&cfg.Discord.FacetSessionTTL, "60m")
	setIntDefault(&cfg.Discord.MessageQueueSize, 64)
	if cfg.Discord.DisplayWidth == nil {
		v := 60
		cfg.Discord.DisplayWidth = &v
	}
	setBoolDefaultDefined(&cfg.Discord.RequireMention, true, md.IsDefined("discord", "require_mention"))
	setBoolDefaultDefined(&cfg.Discord.AutoThread, true, md.IsDefined("discord", "auto_thread"))
	setBoolDefaultDefined(&cfg.Discord.StartupNotify, true, md.IsDefined("discord", "startup_notify"))

	// Apply [defaults] to all agents (agent value > global default > hardcoded).
	// Uses reflect to iterate DefaultsConfig fields and copy to matching
	// AgentConfig fields when the agent value is zero and wasn't explicitly
	// set in the TOML file. This means adding new fields to DefaultsConfig
	// with matching TOML tags in AgentConfig "just works" — no new if-blocks.
	perAgentDefined := agentDefinedFields(md)
	for i := range cfg.Agents {
		var defined map[string]bool
		if i < len(perAgentDefined) {
			defined = perAgentDefined[i]
		}
		applyDefaultsToAgent(&cfg.Agents[i], &cfg, defined)

		if cfg.Agents[i].BranchOrientationFacetPrompt != "" {
			cfg.Agents[i].BranchOrientationFacetPrompt = ResolvePath(cfg.Agents[i].BranchOrientationFacetPrompt)
		}
		if cfg.Agents[i].BranchOrientationHeadlessPrompt != "" {
			cfg.Agents[i].BranchOrientationHeadlessPrompt = ResolvePath(cfg.Agents[i].BranchOrientationHeadlessPrompt)
		}

		// =========================================================================
		// BACKWARD COMPATIBILITY: Migrate deprecated telegram fields to Platforms
		//
		// This migration code is TEMPORARY. Once all callers read from
		// Platforms.Telegram, the deprecated fields on AgentConfig and this
		// migration code will be removed.
		// =========================================================================
		syncDisplayFields(&cfg.Agents[i])
	}

	// Model aliases defaults (if not configured) — use developer/model_id format
	if len(cfg.Models.Aliases) == 0 {
		cfg.Models.Aliases = map[string]string{
			"opus":     "anthropic/claude-opus-4-6",
			"sonnet":   "anthropic/claude-sonnet-4-6",
			"haiku":    "anthropic/claude-haiku-4-5-20251001",
			"gemini-flash": "google/gemini-2.5-flash",
			"gemini-pro":   "google/gemini-2.5-pro",
			"gpt4o":    "openai/gpt-4o",
			"o3":       "openai/o3",
			"o4mini":   "openai/o4-mini",
			"deepseek": "deepseek/deepseek-chat",
		}
	}

	// Endpoint defaults — only create built-in defaults for endpoints that
	// agents actually resolve to. This avoids spurious "missing secret" warnings
	// for endpoints the user doesn't use (e.g. openai.api_key when no agent
	// references OpenAI).
	usedEndpoints := make(map[string]bool)
	for _, agent := range cfg.Agents {
		resolved, err := ResolveModel(agent.Model, agent.Endpoint, cfg.Models.Aliases)
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

	setFloatDefault(&cfg.Sessions.CompactionThreshold, 0.8)
	setIntDefault(&cfg.Sessions.CompactionMaxTokens, 4096)
	setIntDefault(&cfg.Sessions.CompactionMinMessages, 4)
	setIntDefaultDefined(&cfg.Sessions.CompactionPreserveMessages, 25, md.IsDefined("sessions", "compaction_preserve_messages"))
	setBoolDefaultDefined(&cfg.Sessions.AutocompactBeforeManaRefresh, true, md.IsDefined("sessions", "autocompact_before_mana_refresh"))
	setStringDefault(&cfg.Sessions.AutocompactBeforeManaRefreshThreshold, "5m")
	setFloatDefault(&cfg.Sessions.AutocompactBeforeManaRefreshFactor, 0.5)
	// AutocompactBeforeManaRefreshPreserve: nil = use percentage-based default
	// AutocompactBeforeManaRefreshPreservePct: nil = default 0.5 (50% of messages)

	// Apply debug.log_api_key_suffix to the log package global.
	log.DebugLogKeySuffix = cfg.Debug.LogAPIKeySuffix

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
	// GoroutineMonitorThreshold: 0 means auto (35 × agent count), computed at startup.
	// Bitwarden defaults
	setStringDefault(&cfg.Bitwarden.SessionFile, "/home/bitwarden/.bw_session")
	setStringDefault(&cfg.Bitwarden.RefreshInterval, "15m")
	setStringDefault(&cfg.Bitwarden.SecretTTL, "30m")
	setStringDefault(&cfg.Bitwarden.CleanupInterval, "1m")

	setStringDefault(&cfg.Cache.Strategy, "auto")
	setStringDefault(&cfg.Cache.TTL, "1h")
	setStringDefault(&cfg.ManaWarnings.Name, "mana")
	setIntDefault(&cfg.Tools.MaxResultChars, 15000)
	setStringDefault(&cfg.Tools.TempDir, "/tmp/foci/tool-results")
	setIntDefault(&cfg.Tools.TmuxCols, 300)
	setIntDefault(&cfg.Tools.TmuxRows, 30)
	setIntDefaultDefined(&cfg.Tools.ExecAutoBackground, 10, md.IsDefined("tools", "exec_auto_background"))
	setBoolDefaultDefined(&cfg.Tools.AutoSummarise, true, md.IsDefined("tools", "auto_summarise"))
	setBoolDefaultDefined(&cfg.Tools.TmuxAutopilot, true, md.IsDefined("tools", "tmux_autopilot"))
	setStringDefault(&cfg.Tools.TmuxWatchThreshold, "30s")
	setStringDefault(&cfg.Tools.TmuxSessionTTL, "24h")
	setStringDefault(&cfg.Tools.SearchProvider, "brave")
	setStringDefault(&cfg.Tools.FetchProvider, "builtin")
	if len(cfg.Defaults.StopAliases) == 0 {
		cfg.Defaults.StopAliases = []string{"stop", "wait"}
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
	setStringDefault(&cfg.Anthropic.Effort, "low")
	setStringDefault(&cfg.Anthropic.Thinking, "adaptive")

	// Gemini defaults
	setStringDefault(&cfg.Gemini.HTTPTimeout, "120s")
	setStringDefault(&cfg.Gemini.CacheTTL, "1h")
	setStringDefault(&cfg.Gemini.Thinking, "adaptive")

	// OpenAI defaults
	setStringDefault(&cfg.OpenAI.HTTPTimeout, "120s")
	setStringDefault(&cfg.OpenAI.Reasoning, "off")

	// Tools defaults
	setIntDefault(&cfg.Tools.ExecDefaultTimeout, 30)
	setIntDefault(&cfg.Tools.MaxSummaryChars, 300000)
	setStringDefault(&cfg.Tools.TmuxCommandTimeout, "5s")
	setStringDefault(&cfg.Tools.WebFetchTimeout, "30s")
	setIntDefault(&cfg.Tools.WebFetchMaxBytes, 1048576) // 1MB
	setStringDefault(&cfg.Tools.WebSearchTimeout, "15s")
	setIntDefault(&cfg.Tools.MaxConcurrentSpawns, 3)
	setIntDefault(&cfg.Tools.ExploreMaxDepth, 100)
	setInt64Default(&cfg.Tools.MaxUploadFileSize, 50*1024*1024) // 50MB
	setIntDefault(&cfg.Tools.ToolCallPreviewChars, 450)
	setStringDefault(&cfg.Tools.TmuxMemoryCheckInterval, "5m")
	setStringDefault(&cfg.Tools.TmuxMemoryWarn, "10%")
	setStringDefault(&cfg.Tools.TmuxMemoryCritical, "20%")
	setStringDefault(&cfg.Tools.TmuxMemoryKill, "30%")
	setIntDefault(&cfg.Tools.SummaryContextTurns, 5)
	setIntDefault(&cfg.Tools.SummaryContextChars, 6000)
	setIntDefault(&cfg.Tools.MaxSummaryInputChars, 100000)
	setIntDefault(&cfg.Tools.MaxImagePixels, 1920*1080) // 2,073,600 pixels

	// Browser defaults
	setBoolDefaultDefined(&cfg.Tools.Browser.Enabled, true, md.IsDefined("tools", "browser", "enabled"))
	setBoolDefaultDefined(&cfg.Tools.Browser.Headless, true, md.IsDefined("tools", "browser", "headless"))
	setIntDefault(&cfg.Tools.Browser.TimeoutSec, 30)
	setBoolDefaultDefined(&cfg.Tools.Browser.Incognito, true, md.IsDefined("tools", "browser", "incognito"))
	setFloatDefault(&cfg.Tools.Browser.DOMStableSec, 1.0)
	setFloatDefault(&cfg.Tools.Browser.DOMStableDiff, 0.2)

	// Telegram defaults
	setIntDefault(&cfg.Telegram.MessageQueueSize, 64)
	setStringDefault(&cfg.Telegram.LongPollTimeout, "65s")
	setStringDefault(&cfg.Telegram.FacetSessionTTL, "60m")
	if cfg.Telegram.DisplayWidth == nil {
		v := 44
		cfg.Telegram.DisplayWidth = &v
	}
	if cfg.Telegram.TableWrapLines == nil {
		v := 5
		cfg.Telegram.TableWrapLines = &v
	}
	if cfg.Telegram.TableStyle == nil {
		v := "pretty"
		cfg.Telegram.TableStyle = &v
	}

	// HTTP defaults
	setStringDefault(&cfg.HTTP.GracefulShutdownTimeout, "30s")

	// Bool defaults: default to true unless explicitly set to false in config.
	setBoolDefaultDefined(&cfg.Environment.Enabled, true, md.IsDefined("environment", "enabled"))
	setStringDefault(&cfg.Environment.DocsPath, "shared/docs")
	setBoolDefaultDefined(&cfg.Defaults.EnableStopAliases, true, md.IsDefined("defaults", "enable_stop_aliases"))
	setBoolDefaultDefined(&cfg.Telegram.StartupNotify, true, md.IsDefined("telegram", "startup_notify"))

	// Keepalive/background defaults
	setStringDefault(&cfg.Keepalive.Interval, "55m")
	// Keepalive.Prompt: empty = use embedded default (via prompts.ResolvePrompt)
	setStringDefault(&cfg.Background.Interval, "15m")
	// Background.Prompt: empty = use embedded default (via prompts.ResolvePrompt)

	// Mana defaults
	setStringDefault(&cfg.Mana.InvestInterval, "30m")

	// Memory formation defaults
	setStringDefault(&cfg.MemoryFormation.Interval, "1h")
	setStringDefault(&cfg.MemoryFormation.ConsolidationInterval, "20h")
	// IntervalEnabled/ConsolidationEnabled/SessionEndEnabled: nil = true (resolved at runtime)

	// Per-agent keepalive/background/memory-formation: inherit from global.
	for i := range cfg.Agents {
		cfg.Agents[i].Keepalive.MergeDefaults(cfg.Keepalive)
		cfg.Agents[i].Background.MergeDefaults(cfg.Background)
		cfg.Agents[i].MemoryFormation.MergeDefaults(cfg.MemoryFormation)
	}

	// Apply convention-based defaults before path resolution.
	for i := range cfg.Agents {
		// Workspace default: $HOME/$id
		if cfg.Agents[i].Workspace == "" {
			home, _ := os.UserHomeDir()
			cfg.Agents[i].Workspace = filepath.Join(home, cfg.Agents[i].ID)
		}
		// Telegram defaults: bot name = agent ID, received_files_dir = $workspace/received_files.
		// Initialize Platforms.Telegram if not already set (every agent gets one by default).
		if cfg.Agents[i].Platforms == nil {
			cfg.Agents[i].Platforms = &PlatformsConfig{}
		}
		if cfg.Agents[i].Platforms.Telegram == nil {
			cfg.Agents[i].Platforms.Telegram = &TelegramPlatformConfig{}
		}
		tg := cfg.Agents[i].Platforms.Telegram
		if tg.Bot == "" {
			tg.Bot = cfg.Agents[i].ID
		}
		if tg.ReceivedFilesDir == "" {
			tg.ReceivedFilesDir = filepath.Join(cfg.Agents[i].Workspace, "received_files")
		}
		// Discord defaults: initialize Platforms.Discord only if discord is configured globally.
		if len(cfg.Discord.AllowedUsers) > 0 {
			if cfg.Agents[i].Platforms.Discord == nil {
				cfg.Agents[i].Platforms.Discord = &DiscordPlatformConfig{}
			}
			dc := cfg.Agents[i].Platforms.Discord
			if dc.Bot == "" {
				dc.Bot = cfg.Agents[i].ID
			}
			if dc.ReceivedFilesDir == "" {
				dc.ReceivedFilesDir = filepath.Join(cfg.Agents[i].Workspace, "received_files")
			}
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
	if cfg.Background.Enabled && cfg.Keepalive.Enabled {
		bgInt, _ := time.ParseDuration(cfg.Background.Interval)
		kaInt, _ := time.ParseDuration(cfg.Keepalive.Interval)
		if bgInt > 0 && kaInt > 0 && bgInt > kaInt {
			log.Warnf("config", "[background] interval %s > [keepalive] interval %s — keepalive resets cache timer, background work may never trigger", cfg.Background.Interval, cfg.Keepalive.Interval)
		}
	}
	if cfg.Keepalive.Enabled {
		kaInt, _ := time.ParseDuration(cfg.Keepalive.Interval)
		if kaInt > time.Hour {
			log.Warnf("config", "[keepalive] interval %s > 1h — Anthropic cache TTL is 1 hour, cache may expire between keepalives", cfg.Keepalive.Interval)
		}
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}
