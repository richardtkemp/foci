package main

import (
	"fmt"
	"sync"

	"foci/internal/config"
	"foci/internal/log"
)

// liveApply routes a successful config-file edit to the running process, for
// the registry fields whose consumers can pick up a change without a restart
// (the fields carrying a `hot` struct tag — see config.ConfigField.NeedsRestart).
// An edit to any other field is file-only, exactly as before.
//
// Appliers receive a FRESHLY LOADED config (the file just written), never the
// startup config: the startup *config.Config and every ResolvedAgentConfig
// stay frozen, and each applier pushes the new values into its consumers
// through their own thread-safe update paths.
type liveApply struct {
	configPath string

	mu       sync.RWMutex
	appliers map[string]func(fresh *config.Config) error // registry address "section.key" → applier
}

// gwLiveApply is the process-wide instance, set early in main (same pattern
// as pprofGate) so command wiring can reach it without threading params.
var gwLiveApply *liveApply

func newLiveApply(configPath string) *liveApply {
	return &liveApply{configPath: configPath, appliers: map[string]func(*config.Config) error{}}
}

// register maps every address in addrs to fn. One applier typically covers a
// whole family (e.g. all periodic knobs) and refreshes every consumer at once.
func (la *liveApply) register(addrs []string, fn func(*config.Config) error) {
	la.mu.Lock()
	defer la.mu.Unlock()
	for _, a := range addrs {
		la.appliers[a] = fn
	}
}

// Apply pushes one just-edited field into the running process. Returns false
// when the field has no applier (restart-required fields). The section is the
// REGISTRY section ("agent" for per-agent overrides, not "agents").
func (la *liveApply) Apply(section, key string) (bool, error) {
	la.mu.RLock()
	fn := la.appliers[section+"."+key]
	la.mu.RUnlock()
	if fn == nil {
		return false, nil
	}
	fresh, err := config.Load(la.configPath)
	if err != nil {
		return true, fmt.Errorf("live apply %s.%s: reload config: %w", section, key, err)
	}
	if err := fn(fresh); err != nil {
		return true, fmt.Errorf("live apply %s.%s: %w", section, key, err)
	}
	log.Infof("config", "applied %s.%s live", section, key)
	return true, nil
}

// Registry addresses covered by each tranche-1 applier. These lists and the
// `hot` struct tags in internal/config/types.go must stay in lockstep —
// TestLiveApplyCoversHotFields enforces it.
var (
	liveApplyLoggingAddrs = []string{"logging.level"}

	// Global rows only: the agent./platforms. debug override rows are dead
	// (nothing reads them — tracker #1199), so their struct tags carry the
	// ",global" scope marker and no applier exists for them.
	liveApplyDebugAddrs = []string{
		"debug.enable_pprof",
		"debug.extra_ccstream_logging",
		"debug.extra_inbox_logging",
		"debug.extra_telegram_logging",
	}

	liveApplyPeriodicAddrs = []string{
		"keepalive.enabled", "keepalive.interval", "keepalive.prompt",
		"background.enabled", "background.interval", "background.prompt",
		"reflection.backend_quiet_period", "reflection.interval", "reflection.interval_enabled",
		"reflection.interval_prompt", "reflection.notify_on_skill_creation",
		"sessions.ephemeral_retention_days",
		"agent.keepalive.enabled", "agent.keepalive.interval", "agent.keepalive.prompt",
		"agent.background.enabled", "agent.background.interval", "agent.background.prompt",
		"agent.reflection.backend_quiet_period", "agent.reflection.interval", "agent.reflection.interval_enabled",
		"agent.reflection.interval_prompt", "agent.reflection.notify_on_skill_creation",
		"agent.maintenance.consolidation_enabled", "agent.maintenance.consolidation_max_idle",
		"agent.maintenance.consolidation_prompt", "agent.maintenance.consolidation_time",
		"agent.maintenance.reset_idle_guard", "agent.maintenance.reset_time",
		"agent.scheduler.tick_interval",
		"agent.sessions.ephemeral_retention_days",
	}

	// Fields read at runtime through agentInstance.LiveConfig(). The applier
	// re-resolves each agent and swaps the whole snapshot, so any field a
	// consumer reads via LiveConfig goes hot by adding it here + a `hot` tag.
	liveApplyResolvedAddrs = []string{
		"voice.tts", "voice.tts_rate",
		"agent.voice.tts", "agent.voice.tts_rate",
		"display.streaming", "agent.display.streaming",
		"agent_loop.max_tool_loops", "agent.loop.max_tool_loops",
		"debug.cache_bust_detect", "agent.debug.cache_bust_detect",
		"debug.cache_bust_idle_minutes", "agent.debug.cache_bust_idle_minutes",
		"background.can_run_background", "agent.background.can_run_background",
		"tools.max_result_chars", "agent.tools.max_result_chars",
		"tools.max_summary_input_chars", "agent.tools.max_summary_input_chars",
		"tools.exec_auto_background", "agent.tools.exec_auto_background",
		"tools.max_upload_file_size", "agent.tools.max_upload_file_size",
		"tools.max_file_read_bytes", "agent.tools.max_file_read_bytes",
		"tools.http_max_spill_bytes", "agent.tools.http_max_spill_bytes",
		"memory.search_backend", "agent.memory.search_backend",
	}
)

// registerLiveAppliers wires the tranche-1 appliers. Called once from main
// after every agent (and its periodic runner) is set up.
func registerLiveAppliers(la *liveApply, agents map[string]*agentInstance) {
	la.register(liveApplyLoggingAddrs, func(fresh *config.Config) error {
		log.SetLevel(log.ParseLevel(fresh.Logging.Level))
		return nil
	})

	la.register(liveApplyDebugAddrs, func(fresh *config.Config) error {
		pprofGate.Store(config.DerefBool(fresh.Debug.EnablePprof))
		log.SetExtra("ccstream", config.DerefBool(fresh.Debug.ExtraCcstreamLogging))
		log.SetExtra("inbox", config.DerefBool(fresh.Debug.ExtraInboxLogging))
		log.SetExtra("telegram", config.DerefBool(fresh.Debug.ExtraTelegramLogging))
		return nil
	})

	la.register(liveApplyPeriodicAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			inst := agents[freshAcfg.ID]
			if inst == nil || inst.kaRunner == nil || inst.periodicRederive == nil {
				continue
			}
			inst.kaRunner.UpdateSettings(inst.periodicRederive(fresh, freshAcfg))
		}
		return nil
	})

	la.register(liveApplyResolvedAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			if inst := agents[freshAcfg.ID]; inst != nil {
				inst.resolved.Store(config.Resolve(fresh, freshAcfg))
			}
		}
		return nil
	})
}
