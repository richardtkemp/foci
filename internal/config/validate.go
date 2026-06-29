package config

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
)

// validate checks semantic validity of config values after parsing and defaults.
// Returns an error describing the first invalid value found.

// validateAgentBackends checks each agent's delegated backend name against the
// set of registered backends. Empty or "api" is the traditional agent loop (no
// delegated backend) and is always allowed; any other value must be a registered
// backend, else the agent would otherwise fail obscurely at startup
// (delegator.New returns nil) instead of here with a clear message listing the
// valid names.
//
// known is the registered-backend set. It is empty until the backend packages'
// init() functions have run (only in the assembled foci-gw binary), so an empty
// `known` means "registry not populated" → skip validation rather than reject
// every backend. Pure (takes `known` as a parameter) so it is testable without
// mutating the global delegator registry. (#947)
func validateAgentBackends(agents []AgentConfig, known []string) error {
	if len(known) == 0 {
		return nil
	}
	registered := make(map[string]bool, len(known))
	for _, n := range known {
		registered[n] = true
	}
	for _, a := range agents {
		if a.Backend == "" || a.Backend == "api" {
			continue
		}
		if !registered[a.Backend] {
			return fmt.Errorf("agent %q: backend %q is not a registered backend (valid: %s, or \"api\")",
				a.ID, a.Backend, strings.Join(known, ", "))
		}
	}
	return nil
}

// validateRange checks if value is within [min, max] inclusive.
func validateRange(value, min, max float64, fieldName string) error { //nolint:unparam
	if value < min || value > max {
		return fmt.Errorf("%s = %g: must be between %g and %g", fieldName, value, min, max)
	}
	return nil
}

// validateIntRange checks if value is within [min, max] inclusive.
func validateIntRange(value, min, max int, fieldName string) error {
	if value < min || value > max {
		return fmt.Errorf("%s = %d: must be between %d and %d", fieldName, value, min, max)
	}
	return nil
}

// validateNonNegative checks that value is >= 0.
func validateNonNegative(value int, fieldName string) error {
	if value < 0 {
		return fmt.Errorf("%s = %d: must not be negative", fieldName, value)
	}
	return nil
}

// reservedAgentIDs are directory names in the foci home directory that must not
// be used as agent IDs, since the workspace defaults to ~/agentid/.
var reservedAgentIDs = map[string]bool{
	"bin":        true,
	"character":  true,
	"config":     true,
	"data":       true,
	"go":         true,
	"logs":       true,
	"memory":     true,
	"oldscripts": true,
	"scripts":    true,
	"shared":     true,
}

// IsDelegated reports whether this agent uses a delegated (non-API) backend
// such as claude-code. Delegated agents route ALL LLM work — turns, compaction,
// summaries, memory — through the backend itself and never touch the model
// groups, so they need no GroupResolver, no API client, and no anthropic
// credentials. The empty backend defaults to "api".
func (a AgentConfig) IsDelegated() bool {
	return a.Backend != "" && a.Backend != "api"
}

// HasAPIAgent reports whether any configured agent uses an API backend.
// API agents resolve their turns (and foci's auxiliary calls — compaction,
// summaries, memory) through the model groups; delegated backends
// (claude-code, etc.) route ALL of that through the backend itself and never
// touch the groups. A deployment with no API agent should therefore never
// resolve a model group at all — see GroupResolver's guard.
func (cfg *Config) HasAPIAgent() bool {
	for _, a := range cfg.Agents {
		if !a.IsDelegated() {
			return true
		}
	}
	return false
}

func (cfg *Config) Validate() error {
	// Validate agent IDs don't collide with reserved directory names
	for _, a := range cfg.Agents {
		if a.ID == "" {
			continue
		}
		if strings.HasPrefix(a.ID, ".") {
			return fmt.Errorf("agent id %q: must not start with a dot (conflicts with hidden directories)", a.ID)
		}
		if reservedAgentIDs[a.ID] {
			return fmt.Errorf("agent id %q: conflicts with reserved directory ~/%s/ — choose a different id", a.ID, a.ID)
		}
	}

	// Validate each agent's delegated backend name is real (#947).
	if err := validateAgentBackends(cfg.Agents, delegator.RegisteredNames()); err != nil {
		return err
	}

	// Validate timezone if configured.
	if cfg.Timezone != "" {
		if _, err := time.LoadLocation(cfg.Timezone); err != nil {
			return fmt.Errorf("timezone = %q: %w", cfg.Timezone, err)
		}
	}

	// Validate webhook keys contain no path separators (defense in depth)
	for k := range cfg.System.Webhooks {
		if strings.ContainsAny(k, "/\\") {
			return fmt.Errorf("[system] webhooks: key %q must not contain path separators", k)
		}
	}
	for _, a := range cfg.Agents {
		for k := range a.System.Webhooks {
			if strings.ContainsAny(k, "/\\") {
				return fmt.Errorf("agent %q webhooks: key %q must not contain path separators", a.ID, k)
			}
		}
	}

	// Validate groups.powerful. It is REQUIRED only when at least one agent uses
	// an API backend: API agents resolve their turns (and foci's auxiliary calls
	// — compaction, summaries, memory) through the model groups. Delegated
	// backends (claude-code, etc.) route ALL of that through the backend itself
	// and never touch the groups, so a pure-delegated deployment needs none.
	// (If a group IS defined we still validate it resolves, whatever the backends.)
	needsGroups := cfg.HasAPIAgent()
	powerful := cfg.Groups.Groups[GroupPowerful]
	if needsGroups && powerful == "" {
		return fmt.Errorf("[groups] powerful is required (an API-backed agent is configured) — set it in foci.toml")
	}
	if powerful != "" {
		if _, err := ResolveModel(powerful, "", cfg.Models); err != nil {
			return fmt.Errorf("[groups] powerful = %q: %w", powerful, err)
		}
	}

	// Validate endpoint configs
	validFormats := map[string]bool{"anthropic": true, "openai": true, "gemini": true, "": true}
	for name, ep := range cfg.Endpoints {
		if !validFormats[ep.Format] {
			return fmt.Errorf("[endpoints.%s] format = %q: must be \"anthropic\", \"openai\", or \"gemini\"", name, ep.Format)
		}
	}

	// Sessions
	if cfg.Sessions.CompactionThreshold != nil {
		if err := validateRange(*cfg.Sessions.CompactionThreshold, 0.0, 1.0, "[sessions] compaction_threshold"); err != nil {
			return err
		}
	}
	if err := validateNonNegative(cfg.Sessions.CompactionMaxTokens, "[sessions] compaction_max_tokens"); err != nil {
		return err
	}
	if err := validateNonNegative(cfg.Sessions.CompactionMinMessages, "[sessions] compaction_min_messages"); err != nil {
		return err
	}
	if cfg.Sessions.CompactionPreserveMessages != nil {
		if err := validateNonNegative(*cfg.Sessions.CompactionPreserveMessages, "[sessions] compaction_preserve_messages"); err != nil {
			return err
		}
	}
	if cfg.Sessions.AutocompactBeforeManaRefreshFactor != nil {
		if err := validateRange(*cfg.Sessions.AutocompactBeforeManaRefreshFactor, 0.0, 1.0, "[sessions] autocompact_before_mana_refresh_factor"); err != nil {
			return err
		}
	}
	if cfg.Sessions.AutocompactBeforeManaRefreshThreshold != nil {
		if _, err := time.ParseDuration(*cfg.Sessions.AutocompactBeforeManaRefreshThreshold); err != nil {
			return fmt.Errorf("[sessions] autocompact_before_mana_refresh_threshold = %q: %w", *cfg.Sessions.AutocompactBeforeManaRefreshThreshold, err)
		}
	}

	if cfg.Sessions.FileMode != "" {
		if _, err := ParseFileMode(cfg.Sessions.FileMode); err != nil {
			return fmt.Errorf("[sessions] file_mode = %q: %w", cfg.Sessions.FileMode, err)
		}
	}

	// HTTP
	if err := validateIntRange(cfg.HTTP.Port, 1, 65535, "[http] port"); err != nil {
		return err
	}

	// Logging
	validLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	levelUpper := strings.ToUpper(strings.TrimSpace(cfg.Logging.Level))
	if !validLevels[levelUpper] {
		return fmt.Errorf("[logging] level = %q: must be one of DEBUG, INFO, WARN, ERROR", cfg.Logging.Level)
	}
	if _, err := ParseByteSize(cfg.Logging.RotationMaxLineSize); err != nil {
		return fmt.Errorf("[logging] rotation_max_line_size = %q: %w", cfg.Logging.RotationMaxLineSize, err)
	}
	if _, err := ParseFileMode(cfg.Logging.LogFileMode); err != nil {
		return fmt.Errorf("[logging] log_file_mode = %q: %w", cfg.Logging.LogFileMode, err)
	}

	// Global file mode
	if _, err := ParseFileMode(cfg.FileMode); err != nil {
		return fmt.Errorf("file_mode = %q: %w", cfg.FileMode, err)
	}

	// Model cache settings
	validStrategies := map[string]bool{"": true, "auto": true, "explicit": true}
	for name, mc := range cfg.Models {
		if !validStrategies[mc.CacheStrategy] {
			return fmt.Errorf("[models.%s] cache_strategy = %q: must be \"auto\" or \"explicit\"", name, mc.CacheStrategy)
		}
	}

	// Memory sources
	for i, src := range cfg.Memory.Sources {
		if err := validateRange(src.Weight, 0.0, 1.0, fmt.Sprintf("[memory] sources[%d] (%s) weight", i, src.Name)); err != nil {
			return err
		}
	}
	if b := DerefStr(cfg.Memory.SearchBackend); b != "fts5" && b != "bleve" {
		return fmt.Errorf("[memory] search_backend: unknown backend %q (must be \"fts5\" or \"bleve\")", b)
	}
	if err := validateRange(DerefFloat(cfg.Memory.ConversationWeight), 0.0, 1.0, "[memory] conversation_weight"); err != nil {
		return err
	}

	// Mana warnings thresholds
	for i, t := range cfg.Mana.Thresholds {
		if err := validateIntRange(t, 0, 100, fmt.Sprintf("[mana] thresholds[%d]", i)); err != nil {
			return err
		}
	}
	if cfg.Mana.RestoreThreshold != nil {
		if err := validateIntRange(*cfg.Mana.RestoreThreshold, 0, 100, "[mana] restore_threshold"); err != nil {
			return err
		}
	}
	for _, a := range cfg.Agents {
		for i, t := range a.Mana.Thresholds {
			if err := validateIntRange(t, 0, 100, fmt.Sprintf("agent %q [mana] thresholds[%d]", a.ID, i)); err != nil {
				return err
			}
		}
		if a.Mana.RestoreThreshold != nil {
			if err := validateIntRange(*a.Mana.RestoreThreshold, 0, 100, fmt.Sprintf("agent %q [mana] restore_threshold", a.ID)); err != nil {
				return err
			}
		}
	}

	if ttl := cfg.Permissions.PromptTTL; ttl != "" {
		if _, err := time.ParseDuration(ttl); err != nil {
			return fmt.Errorf("[permissions] prompt_ttl = %q: %w", ttl, err)
		}
	}

	if t := DerefStr(cfg.Voice.HTTPTimeout); t != "" {
		if _, err := time.ParseDuration(t); err != nil {
			return fmt.Errorf("[voice] http_timeout = %q: %w", t, err)
		}
	}

	// Special case: tmux_session_ttl allows "0" to disable
	if ttl := DerefStr(cfg.Tools.TmuxSessionTTL); ttl != "" && ttl != "0" {
		if _, err := time.ParseDuration(ttl); err != nil {
			return fmt.Errorf("[tools] tmux_session_ttl = %q: %w", ttl, err)
		}
	}

	// Special case: tmux_memory_check_interval allows "0" to disable
	if cfg.Tools.TmuxMemoryCheckInterval != "0" {
		if _, err := time.ParseDuration(cfg.Tools.TmuxMemoryCheckInterval); err != nil {
			return fmt.Errorf("[tools] tmux_memory_check_interval = %q: %w", cfg.Tools.TmuxMemoryCheckInterval, err)
		}
	}
	for _, kv := range []struct{ k, v string }{
		{"tmux_memory_warn", cfg.Tools.TmuxMemoryWarn},
		{"tmux_memory_critical", cfg.Tools.TmuxMemoryCritical},
		{"tmux_memory_kill", cfg.Tools.TmuxMemoryKill},
	} {
		if err := ValidateMemoryThreshold(kv.v); err != nil {
			return fmt.Errorf("[tools] %s = %q: %w", kv.k, kv.v, err)
		}
	}

	// Resources
	if err := validateIntRange(DerefInt(cfg.Resources.MemoryWarnPercent), 0, 100, "[resources] memory_warn_percent"); err != nil {
		return err
	}
	if err := validateIntRange(DerefInt(cfg.Resources.MemoryKillPercent), 0, 100, "[resources] memory_kill_percent"); err != nil {
		return err
	}
	if DerefFloat(cfg.Resources.MemoryPressureThreshold) < 0 {
		return fmt.Errorf("[resources] memory_pressure_threshold = %g: must not be negative", DerefFloat(cfg.Resources.MemoryPressureThreshold))
	}

	// Table-driven duration validation — all fields that must be valid Go durations
	durations := []durationEntry{
		{"logging", "warning_window_duration", cfg.Logging.WarningWindowDuration},
		{"logging", "warning_proactive_active_interval", cfg.Logging.WarningProactiveActiveInterval},
		{"logging", "warning_proactive_inactive_interval", cfg.Logging.WarningProactiveInactiveInterval},
		{"logging", "warning_proactive_activity_threshold", cfg.Logging.WarningProactiveActivityThreshold},
		{"logging", "rotation_period", cfg.Logging.RotationPeriod},
		{"logging", "retention_period", cfg.Logging.RetentionPeriod},
		{"database", "busy_timeout", cfg.Database.BusyTimeout},
		{"anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout},
		{"anthropic", "usage_cache_ttl", cfg.Anthropic.UsageCacheTTL},
		{"anthropic", "cc_expiry_threshold", cfg.Anthropic.CCExpiryThreshold},
		{"tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout},
		{"tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout},
		{"tools", "web_search_timeout", cfg.Tools.WebSearchTimeout},
		{"resources", "memory_guard_interval", cfg.Resources.MemoryGuardInterval},
		{"http", "graceful_shutdown_timeout", cfg.HTTP.GracefulShutdownTimeout},
		{"sessions", "archive_after", cfg.Sessions.ArchiveAfter},
	}
	if cfg.Bitwarden.Enabled {
		durations = append(durations,
			durationEntry{"bitwarden", "refresh_interval", cfg.Bitwarden.RefreshInterval},
			durationEntry{"bitwarden", "secret_ttl", cfg.Bitwarden.SecretTTL},
			durationEntry{"bitwarden", "cleanup_interval", cfg.Bitwarden.CleanupInterval},
		)
	}
	for name, ep := range cfg.Endpoints {
		if ep.HTTPTimeout != "" {
			durations = append(durations, durationEntry{"endpoints." + name, "http_timeout", ep.HTTPTimeout})
		}
	}
	for _, p := range cfg.Platforms {
		if p.FacetSessionTTL != "" {
			durations = append(durations, durationEntry{"platforms." + p.ID, "facet_session_ttl", p.FacetSessionTTL})
		}
		if p.Telegram != nil && p.Telegram.LongPollTimeout != "" {
			durations = append(durations, durationEntry{"platforms." + p.ID, "long_poll_timeout", p.Telegram.LongPollTimeout})
		}
	}
	if err := validateDurations(durations); err != nil {
		return err
	}

	// Model fallbacks: validate keys/values resolve and chains don't exceed depth
	if err := validateFallbacks("groups.fallbacks", cfg.Groups.Fallbacks, cfg.Models); err != nil {
		return err
	}
	for _, a := range cfg.Agents {
		if err := validateFallbacks(fmt.Sprintf("agent %q groups.fallbacks", a.ID), a.Groups.Fallbacks, cfg.Models); err != nil {
			return err
		}
		// Validate per-agent group models resolve
		for name, model := range a.Groups.Groups {
			if _, err := ResolveModel(model, "", cfg.Models); err != nil {
				return fmt.Errorf("agent %q [groups] %s = %q: %w", a.ID, name, model, err)
			}
		}
	}

	return nil
}

// validateFallbacks checks that all keys and values in a fallback map resolve
// to valid models, and that no chain exceeds MaxFallbackDepth.
func validateFallbacks(section string, fallbacks map[string]string, models map[string]ModelConfig) error {
	if len(fallbacks) == 0 {
		return nil
	}
	// Validate each entry resolves
	canonical := make(map[string]string, len(fallbacks))
	for k, v := range fallbacks {
		rk, err := ResolveModel(k, "", models)
		if err != nil {
			return fmt.Errorf("[%s] key %q: %w", section, k, err)
		}
		rv, err := ResolveModel(v, "", models)
		if err != nil {
			return fmt.Errorf("[%s] value %q (for key %q): %w", section, v, k, err)
		}
		ck := rk.Developer + "/" + rk.ModelID
		cv := rv.Developer + "/" + rv.ModelID
		canonical[ck] = cv
	}
	// Check chain depth
	for start := range canonical {
		depth := 0
		visited := map[string]bool{start: true}
		cur := start
		for {
			next, ok := canonical[cur]
			if !ok {
				break
			}
			depth++
			if depth > MaxFallbackDepth {
				return fmt.Errorf("[%s] chain starting at %q exceeds max depth %d", section, start, MaxFallbackDepth)
			}
			if visited[next] {
				return fmt.Errorf("[%s] cycle detected: %q → %q", section, cur, next)
			}
			visited[next] = true
			cur = next
		}
	}
	return nil
}

// BotTokenConflict describes multiple agents sharing a single bot token.
type BotTokenConflict struct {
	Platform string   // "telegram" or "discord"
	BotName  string   // the bot name from the first (surviving) agent's config
	AgentIDs []string // all agents using this token; first = survivor
}

// DetectBotTokenConflicts finds agents that share the same resolved bot token.
// Agents are grouped by actual token value (not bot name), so two different
// bot names pointing to the same secret are detected.  The returned slice
// contains only tokens used by more than one agent, with AgentIDs in config
// order (first = survivor).
func DetectBotTokenConflicts(agents []AgentConfig, secrets SecretGetter) []BotTokenConflict {
	type tokenInfo struct {
		botName  string
		agentIDs []string
	}

	tgTokens := make(map[string]*tokenInfo) // resolved token → info
	dcTokens := make(map[string]*tokenInfo)

	for _, a := range agents {
		if tg := a.Platform("telegram"); tg != nil && tg.Bot != "" {
			token := ResolveBotToken(tg.Bot, tg.BotSecret, secrets)
			if token == "" {
				continue
			}
			if ti, ok := tgTokens[token]; ok {
				ti.agentIDs = append(ti.agentIDs, a.ID)
			} else {
				tgTokens[token] = &tokenInfo{botName: tg.Bot, agentIDs: []string{a.ID}}
			}
		}
		if dc := a.Platform("discord"); dc != nil && dc.Bot != "" {
			token := ResolveDiscordToken(dc.Bot, dc.BotSecret, secrets)
			if token == "" {
				continue
			}
			if ti, ok := dcTokens[token]; ok {
				ti.agentIDs = append(ti.agentIDs, a.ID)
			} else {
				dcTokens[token] = &tokenInfo{botName: dc.Bot, agentIDs: []string{a.ID}}
			}
		}
	}

	var conflicts []BotTokenConflict
	for _, ti := range tgTokens {
		if len(ti.agentIDs) > 1 {
			conflicts = append(conflicts, BotTokenConflict{
				Platform: "telegram",
				BotName:  ti.botName,
				AgentIDs: ti.agentIDs,
			})
		}
	}
	for _, ti := range dcTokens {
		if len(ti.agentIDs) > 1 {
			conflicts = append(conflicts, BotTokenConflict{
				Platform: "discord",
				BotName:  ti.botName,
				AgentIDs: ti.agentIDs,
			})
		}
	}
	return conflicts
}
