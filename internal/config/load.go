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
	if p.Display.ReceivedFilesDir == nil {
		p.Display.ReceivedFilesDir = &dir
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

	// Apply struct tag defaults to all global config sections.
	// Recursion handles nested structs; slices (Agents, Platforms) and
	// maps (Models, Endpoints) are skipped — per-agent pointer fields
	// stay nil for Merge to work correctly.
	ApplyTagDefaults(&cfg)

	// Apply debug.log_api_key_suffix to the log package global.
	log.DebugLogKeySuffix = DerefBool(cfg.Debug.LogAPIKeySuffix)

	// Dynamic defaults that can't be expressed as struct tags.
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, "data")
	}
	// GoroutineMonitorThreshold: 0 means auto (30 + 25×agents + 5×telegram_bots), computed at startup.
	if len(cfg.Defaults.Behavior.StopAliases) == 0 {
		cfg.Defaults.Behavior.StopAliases = []string{"stop", "wait"}
	}

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
		if p := cfg.Platform("discord"); p != nil && len(p.Access.AllowedUsers) > 0 {
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
