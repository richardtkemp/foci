package config

import (
	"fmt"
	"strings"
	"time"
)

// validate checks semantic validity of config values after parsing and defaults.
// Returns an error describing the first invalid value found.

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

func validate(cfg *Config) error {
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

	// Validate webhook keys contain no path separators (defense in depth)
	for k := range cfg.Defaults.Webhooks {
		if strings.ContainsAny(k, "/\\") {
			return fmt.Errorf("[defaults] webhooks: key %q must not contain path separators", k)
		}
	}
	for _, a := range cfg.Agents {
		for k := range a.Webhooks {
			if strings.ContainsAny(k, "/\\") {
				return fmt.Errorf("agent %q webhooks: key %q must not contain path separators", a.ID, k)
			}
		}
	}

	// Validate agent model format (must use slash syntax, not colon)
	for _, a := range cfg.Agents {
		if a.Model == "" {
			continue
		}
		// Check for legacy colon format
		if strings.Contains(a.Model, ":") {
			return fmt.Errorf(`agent %q: invalid model format %q
  The model format has changed from "endpoint:model" to "developer/model_id"

  Update your config:
  - Old: model = %q
  - New: model = %q

  Or use an alias:
  - model = "haiku"  (expands to "anthropic/claude-haiku-4-5-20251001")`,
				a.ID, a.Model, a.Model, strings.ReplaceAll(a.Model, ":", "/"))
		}
		// Validate slash format (will be checked by ResolveModel at load time)
		if !strings.Contains(a.Model, "/") {
			// Could be an alias - this is checked during Load()
			continue
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
	if err := validateRange(cfg.Sessions.CompactionThreshold, 0.0, 1.0, "[sessions] compaction_threshold"); err != nil {
		return err
	}
	if err := validateNonNegative(cfg.Sessions.CompactionMaxTokens, "[sessions] compaction_max_tokens"); err != nil {
		return err
	}
	if err := validateNonNegative(cfg.Sessions.CompactionMinMessages, "[sessions] compaction_min_messages"); err != nil {
		return err
	}
	if err := validateNonNegative(cfg.Sessions.CompactionPreserveMessages, "[sessions] compaction_preserve_messages"); err != nil {
		return err
	}
	if err := validateRange(cfg.Sessions.CompactionIdlePressureMax, 0.0, 1.0, "[sessions] compaction_idle_pressure_max"); err != nil {
		return err
	}
	// Idle threshold: "0" disables, otherwise must parse as duration
	if cfg.Sessions.CompactionIdleThreshold != "0" {
		if _, err := time.ParseDuration(cfg.Sessions.CompactionIdleThreshold); err != nil {
			return fmt.Errorf("[sessions] compaction_idle_threshold = %q: %w", cfg.Sessions.CompactionIdleThreshold, err)
		}
	}
	// Mana refresh threshold: "0" disables, otherwise must parse as duration
	if cfg.Sessions.CompactionManaRefreshThreshold != "0" {
		if _, err := time.ParseDuration(cfg.Sessions.CompactionManaRefreshThreshold); err != nil {
			return fmt.Errorf("[sessions] compaction_mana_refresh_threshold = %q: %w", cfg.Sessions.CompactionManaRefreshThreshold, err)
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

	// Cache
	validStrategies := map[string]bool{"auto": true, "explicit": true}
	if !validStrategies[cfg.Cache.Strategy] {
		return fmt.Errorf("[cache] strategy = %q: must be \"auto\" or \"explicit\"", cfg.Cache.Strategy)
	}
	validCacheTTLs := map[string]bool{"5m": true, "1h": true}
	if !validCacheTTLs[cfg.Cache.TTL] {
		return fmt.Errorf("[cache] ttl = %q: must be \"5m\" or \"1h\"", cfg.Cache.TTL)
	}
	for _, a := range cfg.Agents {
		if a.CacheTTL != "" && !validCacheTTLs[a.CacheTTL] {
			return fmt.Errorf("agent %q cache_ttl = %q: must be \"5m\" or \"1h\"", a.ID, a.CacheTTL)
		}
	}
	if cfg.Defaults.CacheTTL != "" && !validCacheTTLs[cfg.Defaults.CacheTTL] {
		return fmt.Errorf("[defaults] cache_ttl = %q: must be \"5m\" or \"1h\"", cfg.Defaults.CacheTTL)
	}

	// Memory sources
	for i, src := range cfg.Memory.Sources {
		if err := validateRange(src.Weight, 0.0, 1.0, fmt.Sprintf("[memory] sources[%d] (%s) weight", i, src.Name)); err != nil {
			return err
		}
	}
	for _, backend := range cfg.Memory.SearchBackends {
		if backend != "fts5" && backend != "bleve" {
			return fmt.Errorf("[memory] search_backends: unknown backend %q (must be \"fts5\" or \"bleve\")", backend)
		}
	}
	if err := validateRange(cfg.Memory.ConversationWeight, 0.0, 1.0, "[memory] conversation_weight"); err != nil {
		return err
	}

	// Mana warnings thresholds
	for i, t := range cfg.ManaWarnings.Thresholds {
		if err := validateIntRange(t, 0, 100, fmt.Sprintf("[usage_warnings] thresholds[%d]", i)); err != nil {
			return err
		}
	}
	if err := validateIntRange(cfg.ManaWarnings.RestoreThreshold, 0, 100, "[usage_warnings] restore_threshold"); err != nil {
		return err
	}
	for _, a := range cfg.Agents {
		for i, t := range a.UsageWarnings.Thresholds {
			if err := validateIntRange(t, 0, 100, fmt.Sprintf("agent %q [usage_warnings] thresholds[%d]", a.ID, i)); err != nil {
				return err
			}
		}
		if a.UsageWarnings.RestoreThreshold != nil {
			if err := validateIntRange(*a.UsageWarnings.RestoreThreshold, 0, 100, fmt.Sprintf("agent %q [usage_warnings] restore_threshold", a.ID)); err != nil {
				return err
			}
		}
	}

	// Special case: gemini cache_ttl allows "0" to disable
	if cfg.Gemini.CacheTTL != "0" {
		if _, err := time.ParseDuration(cfg.Gemini.CacheTTL); err != nil {
			return fmt.Errorf("[gemini] cache_ttl = %q: %w", cfg.Gemini.CacheTTL, err)
		}
	}

	// Special case: tmux_session_ttl allows "0" to disable
	if cfg.Tools.TmuxSessionTTL != "0" {
		if _, err := time.ParseDuration(cfg.Tools.TmuxSessionTTL); err != nil {
			return fmt.Errorf("[tools] tmux_session_ttl = %q: %w", cfg.Tools.TmuxSessionTTL, err)
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
	if err := validateIntRange(cfg.Resources.MemoryWarnPercent, 0, 100, "[resources] memory_warn_percent"); err != nil {
		return err
	}
	if err := validateIntRange(cfg.Resources.MemoryKillPercent, 0, 100, "[resources] memory_kill_percent"); err != nil {
		return err
	}
	if cfg.Resources.MemoryPressureThreshold < 0 {
		return fmt.Errorf("[resources] memory_pressure_threshold = %g: must not be negative", cfg.Resources.MemoryPressureThreshold)
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
		{"anthropic", "http_timeout", cfg.Anthropic.HTTPTimeout},
		{"anthropic", "usage_api_timeout", cfg.Anthropic.UsageAPITimeout},
		{"anthropic", "usage_cache_ttl", cfg.Anthropic.UsageCacheTTL},
		{"anthropic", "cc_credentials_poll_interval", cfg.Anthropic.CCCredentialsPollInterval},
		{"gemini", "http_timeout", cfg.Gemini.HTTPTimeout},
		{"openai", "http_timeout", cfg.OpenAI.HTTPTimeout},
		{"tools", "tmux_command_timeout", cfg.Tools.TmuxCommandTimeout},
		{"tools", "web_fetch_timeout", cfg.Tools.WebFetchTimeout},
		{"tools", "web_search_timeout", cfg.Tools.WebSearchTimeout},
		{"resources", "memory_guard_interval", cfg.Resources.MemoryGuardInterval},
		{"telegram", "long_poll_timeout", cfg.Telegram.LongPollTimeout},
		{"telegram", "multiball_session_ttl", cfg.Telegram.MultiballSessionTTL},
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
	if err := validateDurations(durations); err != nil {
		return err
	}

	return nil
}
