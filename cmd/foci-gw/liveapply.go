package main

import (
	"fmt"
	"strings"
	"sync"

	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/skills"
)

// liveApply routes a successful config-file edit to the running process, for
// the registry fields whose consumers can pick up a change without a restart
// (the fields carrying a `hot` struct tag — see config.ConfigField.NeedsRestart).
// An edit to any other field is file-only, exactly as before.
//
// Appliers receive a FRESHLY LOADED config (the file just written, through
// loadGatewayConfig so provider platform defaults match startup — #2749),
// never the startup config: the startup *config.Config and every ResolvedAgentConfig
// stay frozen, and each applier pushes the new values into its consumers
// through their own thread-safe update paths.
type liveApply struct {
	configPath string
	// protect is the load-time protection check (#479): an unprotected file
	// applies nothing. nil skips it.
	protect func(path string) error

	mu       sync.RWMutex
	appliers map[string]func(fresh *config.Config) error // registry address "section.key" → applier

	// mapSectionAppliers covers map-typed config sections (groups,
	// groups.calls, groups.fallbacks, system.webhooks — see MapFieldSpec in
	// internal/config/fields.go), keyed by SECTION alone rather than
	// "section.key": the key is a user-defined group/webhook name and can't
	// be pre-enumerated, so Apply falls back here when the exact "section.key"
	// address has no applier. Any key under a registered section triggers the
	// same whole-section rebuild.
	mapSectionAppliers map[string]func(fresh *config.Config) error
}

// gwLiveApply is the process-wide instance, set early in main (same pattern
// as pprofGate) so command wiring can reach it without threading params.
var gwLiveApply *liveApply

func newLiveApply(configPath string, protect func(string) error) *liveApply {
	return &liveApply{
		configPath:         configPath,
		protect:            protect,
		appliers:           map[string]func(*config.Config) error{},
		mapSectionAppliers: map[string]func(*config.Config) error{},
	}
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

// registerMapSection maps a map-typed config section (e.g. "groups.calls")
// to fn, covering every key under it — see mapSectionAppliers' doc.
func (la *liveApply) registerMapSection(sections []string, fn func(*config.Config) error) {
	la.mu.Lock()
	defer la.mu.Unlock()
	for _, s := range sections {
		la.mapSectionAppliers[s] = fn
	}
}

// Apply pushes one just-edited field into the running process. Returns false
// when the field has no applier (restart-required fields). The section is the
// REGISTRY section ("agent" for per-agent overrides, not "agents"). An empty
// key addresses a whole object-list section ([[modelinfo]]), which is how the
// app's config editor sends those edits.
//
// Every call leaves one log line — INFO when applied, WARN when the edit only
// takes effect on restart — so the log answers "did my edit apply?" (#2116).
// An apply error is returned for the caller to log.
func (la *liveApply) Apply(section, key string) (bool, error) {
	addr := section
	if key != "" {
		addr += "." + key
	}
	la.mu.RLock()
	fn := la.appliers[addr]
	if fn == nil {
		fn = la.mapSectionAppliers[section]
	}
	if fn == nil {
		// A mapObject section ([endpoints.*]) edited by key path: section
		// "endpoints.<name>" (or "endpoints" with key "<name>") goes to the
		// applier at the bare section address.
		head, _, _ := strings.Cut(section, ".")
		if spec, ok := config.MapObjectFieldSpecFor(head); ok && spec.Live {
			fn = la.appliers[spec.Section]
		}
	}
	la.mu.RUnlock()
	if fn == nil {
		configLog.Warnf("%s written but not live-appliable: takes effect on restart", addr)
		return false, nil
	}
	if err := checkConfigLoad(la.protect, la.configPath); err != nil {
		return true, fmt.Errorf("live apply %s: %w", addr, err)
	}
	fresh, err := loadGatewayConfig(la.configPath)
	if err != nil {
		return true, fmt.Errorf("live apply %s: reload config: %w", addr, err)
	}
	if err := fn(fresh); err != nil {
		return true, fmt.Errorf("live apply %s: %w", addr, err)
	}
	configLog.Infof("applied %s live", addr)
	return true, nil
}

// Registry addresses covered by each tranche-1 applier. These lists and the
// `hot` struct tags in internal/config/types.go must stay in lockstep —
// TestLiveApplyCoversHotFields enforces it.
var (
	liveApplyLoggingAddrs = []string{"logging.level"}

	// The usage-hold threshold (#2491): global only, read from the raw
	// config into the process-wide usagehold.Default.
	liveApplyUsageHoldAddrs = []string{"usage_hold.threshold"}

	// Extra skill dirs (#2657): a derived handle — each agent's
	// *skills.LiveDirs, read at every skill reload (session start) and every
	// reflection snapshot. The shared dir and skills_dir stay restart-only.
	liveApplySkillsAddrs = []string{"skills.extra_dirs", "agent.skills.extra_dirs"}

	// Read from the config file at every Claude Code launch (#2953,
	// liveClaudeAISkills), like the pretool rules: there is no handle to push
	// into, so the applier only confirms the edit; the next CC launch reads it.
	liveApplyCCLaunchAddrs = []string{"agent.backend_config.claude_ai_skills"}

	// modelinfo is an object[] section (not scalar hot-tagged fields), so it
	// has no per-field registry rows. The bare section address is what
	// Apply(section, "") looks up — the app editor's object-list edit path.
	liveApplyModelInfoAddrs = []string{"modelinfo"}

	// [endpoints.*] (#2980) is a mapObject section: no per-field registry
	// rows. The app editor applies it at the bare section address; a CLI
	// path (endpoints.<name>.<sub>) reaches the same applier through
	// Apply's mapObject fallback. The client registry swaps each changed
	// endpoint's client behind the handle callers hold.
	liveApplyEndpointsAddrs = []string{"endpoints"}

	// #2956: the pin label summary's model and provider order, pushed into
	// the app hub's summariser (a derived handle).
	liveApplyPinSummaryAddrs = []string{"platforms.app.pin_summary_model", "platforms.app.pin_summary_provider"}

	// #2962: the automatic chat name's switch, counts and word limit, pushed
	// into the app hub's autoNamer (a derived handle).
	liveApplyAutoNameAddrs = []string{"platforms.app.auto_name", "platforms.app.auto_name_first", "platforms.app.auto_name_every", "platforms.app.auto_name_words"}

	// #2993: the chat sections' switch and counts, pushed into the app hub's
	// sectioner (a derived handle).
	liveApplyChatSectionsAddrs = []string{"platforms.app.chat_sections", "platforms.app.chat_sections_every", "platforms.app.chat_sections_after_break"}

	// Global rows only: the agent./platforms. debug override rows are dead
	// (nothing reads them — tracker #1199), so their struct tags carry the
	// ",global" scope marker and no applier exists for them.
	liveApplyDebugAddrs = []string{
		"debug.enable_pprof",
		"debug.extra_ccstream_logging",
		"debug.extra_inbox_logging",
		"debug.extra_telegram_logging",
		"debug.log_api_key_suffix",
	}

	// Goroutine monitor on/off (#2649): a derived handle, the running
	// *resources.GoroutineMonitor. Registered by setupGoroutineMonitor, which
	// runs after registerLiveAppliers. Interval and threshold stay restart-only.
	liveApplyGoroutineMonitorAddrs = []string{"resources.goroutine_monitor_enabled"}

	liveApplyPeriodicAddrs = []string{
		"keepalive.enabled", "keepalive.interval", "keepalive.prompt", "keepalive.max_user_idle",
		"keepalive.warm_open_app_chats", "keepalive.timeout",
		"background.enabled", "background.interval", "background.prompt",
		"reflection.backend_quiet_period", "reflection.interval", "reflection.interval_enabled",
		"reflection.interval_prompt", "reflection.notify_on_skill_creation",
		"sessions.ephemeral_retention_days",
		"sessions.compaction_quiet_hours", "sessions.compaction_quiet_threshold", "sessions.compaction_quiet_min_idle",
		"sessions.compaction_precold", "sessions.compaction_precold_threshold", "sessions.compaction_precold_lead",
		"maintenance.consolidation_enabled", "maintenance.consolidation_max_idle",
		"maintenance.consolidation_prompt", "maintenance.consolidation_time",
		"maintenance.reset_idle_guard", "maintenance.reset_time",
		"scheduler.tick_interval",
		"agent.keepalive.enabled", "agent.keepalive.interval", "agent.keepalive.prompt", "agent.keepalive.max_user_idle",
		"agent.keepalive.warm_open_app_chats", "agent.keepalive.timeout",
		"agent.background.enabled", "agent.background.interval", "agent.background.prompt",
		"agent.reflection.backend_quiet_period", "agent.reflection.interval", "agent.reflection.interval_enabled",
		"agent.reflection.interval_prompt", "agent.reflection.notify_on_skill_creation",
		"agent.maintenance.consolidation_enabled", "agent.maintenance.consolidation_max_idle",
		"agent.maintenance.consolidation_prompt", "agent.maintenance.consolidation_time",
		"agent.maintenance.reset_idle_guard", "agent.maintenance.reset_time",
		"agent.scheduler.tick_interval",
		"agent.sessions.ephemeral_retention_days",
		"agent.sessions.compaction_quiet_hours", "agent.sessions.compaction_quiet_threshold", "agent.sessions.compaction_quiet_min_idle",
		"agent.sessions.compaction_precold", "agent.sessions.compaction_precold_threshold", "agent.sessions.compaction_precold_lead",
	}

	// Fields consumed off agentInstance.resolved (the LiveValue), either
	// read-through via LiveConfig()/a func() getter, or by a derived-handle
	// rebuild registered via resolved.OnChange (compactor, group throttle).
	// The applier re-resolves each agent and swaps/notifies the whole
	// snapshot, so any field a consumer reaches this way goes hot by adding
	// it here + a `hot` tag.
	liveApplyResolvedAddrs = []string{
		"voice.tts", "voice.tts_order", "voice.tts_rate", "voice.stt",
		"agent.voice.tts", "agent.voice.tts_order", "agent.voice.tts_rate", "agent.voice.stt",
		"agent_loop.max_tool_loops", "agent.loop.max_tool_loops",
		"agent_loop.streaming", "agent.loop.streaming",
		"debug.cache_bust_detect", "agent.debug.cache_bust_detect",
		"background.can_run_background", "agent.background.can_run_background",
		"tools.max_result_chars", "agent.tools.max_result_chars",
		"tools.max_summary_input_chars", "agent.tools.max_summary_input_chars",
		"tools.exec_auto_background", "agent.tools.exec_auto_background",
		"tools.max_upload_file_size", "agent.tools.max_upload_file_size",
		"tools.max_file_read_bytes", "agent.tools.max_file_read_bytes",
		"tools.http_max_spill_bytes", "agent.tools.http_max_spill_bytes",
		"tools.web_fetch_max_bytes", "agent.tools.web_fetch_max_bytes",
		"memory.search_backend", "agent.memory.search_backend",
		// #2827: read by the system-prompt builders at each session start.
		"memory.daily_digest", "agent.memory.daily_digest",
		"tools.todo_format", "agent.tools.todo_format",
		"tools.explore_max_depth", "agent.tools.explore_max_depth",
		"tools.spawn_reserve_percent", "agent.tools.spawn_reserve_percent",
		// #2722: resizes each Spawner's slots via getSpawner's OnChange.
		"tools.max_concurrent_spawns", "agent.tools.max_concurrent_spawns",
		"environment.enabled", "agent.environment.enabled",
		"environment.docs_path", "agent.environment.docs_path",
		"sessions.compaction_threshold", "agent.sessions.compaction_threshold",
		"sessions.compaction_preserve_messages", "agent.sessions.compaction_preserve_messages",
		"behavior.steer_mode", "agent.behavior.steer_mode",
		"behavior.group_throttle", "agent.behavior.group_throttle",
		"notify.compaction_notify", "agent.notify.compaction_notify", "platforms.notify.compaction_notify",
		"notify.task_list_notify", "agent.notify.task_list_notify", "platforms.notify.task_list_notify",
		"notify.compaction_debug", "agent.notify.compaction_debug", "platforms.notify.compaction_debug",
		// #1857: read live by the ccstream rate-limit hook off LiveConfig().
		"notify.rate_limit_notify_to", "agent.notify.rate_limit_notify_to",
		// #2933: read live by the cache-bust alert off LiveConfig().
		"notify.cache_bust_notify", "agent.notify.cache_bust_notify",
		"display.stream_output", "agent.display.stream_output", "platforms.display.stream_output",
		"display.table_wrap_lines", "agent.display.table_wrap_lines", "platforms.display.table_wrap_lines",
		"display.table_style", "agent.display.table_style", "platforms.display.table_style",
		"debug.messages_in_log", "agent.debug.messages_in_log",
		"platforms.telegram.long_poll_timeout",
		// Bucket E (#1230): remaining Agent scalars read live per-use.
		"agent_loop.max_output_tokens", "agent.loop.max_output_tokens",
		"agent_loop.duplicate_messages", "agent.loop.duplicate_messages",
		"agent_loop.batch_partial_assistant_messages", "agent.loop.batch_partial_assistant_messages",
		"agent_loop.batch_partial_joiner", "agent.loop.batch_partial_joiner",
		"tools.max_summary_chars", "agent.tools.max_summary_chars",
		"tools.auto_summarise", "agent.tools.auto_summarise",
		"tools.summary_context_turns", "agent.tools.summary_context_turns",
		"tools.summary_context_chars", "agent.tools.summary_context_chars",
		"tools.max_image_pixels", "agent.tools.max_image_pixels",
		"sessions.compaction_handoff_msg", "agent.sessions.compaction_handoff_msg",
		"sessions.reload_on_compact", "agent.sessions.reload_on_compact",
		"behavior.turn_lock_warn_threshold", "agent.behavior.turn_lock_warn_threshold",
		// #2931: read per turn by Agent.promptSuggestionMaxUsage, carried on the inject.
		"behavior.prompt_suggestion_max_usage", "agent.behavior.prompt_suggestion_max_usage",
		"display.show_tool_calls", "agent.display.show_tool_calls", "platforms.display.show_tool_calls",
		"display.statusline", "agent.display.statusline", "platforms.display.statusline",
		// #1241: reflection session-end + compaction fields, read live at the
		// memory-formation sites via a.reflection() (the resolved snapshot).
		"reflection.session_end_enabled", "agent.reflection.session_end_enabled",
		"reflection.session_end_prompt", "agent.reflection.session_end_prompt",
		"reflection.compaction_enabled", "agent.reflection.compaction_enabled",
		"reflection.compaction_prompt", "agent.reflection.compaction_prompt",
		// #1450: per-operation force-in-session overrides for the four
		// periodic ops, read live by agent.BranchStrategyFor via
		// keepalive()/backgroundConfig()/reflection()/maintenance() (the
		// resolved snapshot) — not consumed by the periodic.Runner, so these
		// live in liveApplyResolvedAddrs rather than liveApplyPeriodicAddrs.
		"keepalive.force_in_session", "agent.keepalive.force_in_session",
		"background.force_in_session", "agent.background.force_in_session",
		"reflection.force_in_session", "agent.reflection.force_in_session",
		"maintenance.consolidation_force_in_session", "agent.maintenance.consolidation_force_in_session",
		// #2603: the depth tools' live gate reads LiveConfig().Depth.Enabled;
		// an off→on edit restores the trees (getDepth's OnChange).
		"depth.enabled", "agent.depth.enabled",
	}

	liveApplyWarningAddrs = []string{
		"notify.inject_agent_warnings", "agent.notify.inject_agent_warnings", "platforms.notify.inject_agent_warnings",
		"notify.inject_chat_warnings", "agent.notify.inject_chat_warnings", "platforms.notify.inject_chat_warnings",
		"notify.warning_max_per_window", "agent.notify.warning_max_per_window",
		"logging.warning_window_duration",
	}

	liveApplyNudgeAddrs = []string{
		"nudge.nudge_enable", "nudge.nudge_auto_extract", "nudge.nudge_cooldown",
		"nudge.nudge_max_per_batch", "nudge.nudge_max_per_turn", "nudge.nudge_extraction_model",
		"nudge.nudge_pre_answer_gate", "nudge.nudge_pre_answer_min_tools",
		"nudge.nudge_default_enable", "nudge.nudge_default_frequency", "nudge.nudge_default_scratchpad_frequency",
		"nudge.nudge_default_braindead_threshold", "nudge.nudge_default_braindead_prompt",
		"agent.nudge.nudge_enable", "agent.nudge.nudge_auto_extract", "agent.nudge.nudge_cooldown",
		"agent.nudge.nudge_max_per_batch", "agent.nudge.nudge_max_per_turn", "agent.nudge.nudge_extraction_model",
		"agent.nudge.nudge_pre_answer_gate", "agent.nudge.nudge_pre_answer_min_tools",
		"agent.nudge.nudge_default_enable", "agent.nudge.nudge_default_frequency", "agent.nudge.nudge_default_scratchpad_frequency",
		"agent.nudge.nudge_default_braindead_threshold", "agent.nudge.nudge_default_braindead_prompt",
	}
)

// registerLiveAppliers wires the tranche-1 appliers. Called once from main
// after every agent (and its periodic runner) is set up. Each applier looks
// agents up in the registry when it runs, so an agent hot-added by
// /agents new (#963) is covered too.
func registerLiveAppliers(la *liveApply, agents *agentRegistry) {
	la.register(liveApplyLoggingAddrs, func(fresh *config.Config) error {
		log.SetLevel(log.ParseLevel(fresh.Logging.Level))
		return nil
	})

	la.register(liveApplyUsageHoldAddrs, func(fresh *config.Config) error {
		applyUsageHoldThreshold(fresh)
		return nil
	})

	la.register(liveApplyCCLaunchAddrs, func(*config.Config) error { return nil })

	la.register(liveApplySkillsAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			if inst, _ := agents.Get(freshAcfg.ID); inst != nil {
				applySkillExtraDirs(inst.skillsDirs, fresh, freshAcfg)
			}
		}
		return nil
	})

	la.register(liveApplyDebugAddrs, func(fresh *config.Config) error {
		pprofGate.Store(config.DerefBool(fresh.Debug.EnablePprof))
		log.SetExtra("ccstream", config.DerefBool(fresh.Debug.ExtraCcstreamLogging))
		log.SetExtra("inbox", config.DerefBool(fresh.Debug.ExtraInboxLogging))
		log.SetExtra("telegram", config.DerefBool(fresh.Debug.ExtraTelegramLogging))
		log.DebugLogKeySuffix = config.DerefBool(fresh.Debug.LogAPIKeySuffix)
		return nil
	})

	la.register(liveApplyPeriodicAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			inst, _ := agents.Get(freshAcfg.ID)
			if inst == nil {
				continue
			}
			// reflection.notify_on_skill_creation has two consumers: the scheduler
			// handle (below) and the memory-formation sites, which read it live via
			// a.reflection() off the resolved snapshot. A field maps to ONE applier,
			// so this applier also refreshes the snapshot (#1241).
			inst.resolved.Store(config.Resolve(fresh, freshAcfg))
			if inst.kaRunner == nil || inst.periodicRederive == nil {
				continue
			}
			inst.kaRunner.UpdateSettings(inst.periodicRederive(fresh, freshAcfg))
		}
		return nil
	})

	la.register(liveApplyResolvedAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			if inst, _ := agents.Get(freshAcfg.ID); inst != nil {
				inst.resolved.Store(config.Resolve(fresh, freshAcfg))
			}
		}
		return nil
	})

	// Warning-queue injection levels + rate limit are derived handles (the
	// queues hold their own live state, not read from the resolved snapshot on
	// every push), so reconfigure them explicitly on change (#1225).
	la.register(liveApplyWarningAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			inst, _ := agents.Get(freshAcfg.ID)
			if inst == nil || inst.ag == nil {
				continue
			}
			applyWarningQueueLevels(inst.ag, config.Resolve(fresh, freshAcfg), fresh)
		}
		return nil
	})

	// Nudge scheduler is a derived handle: all rules are built once and firing
	// is gated on these settings, so a [nudge] edit reconfigures it in
	// place (no rebuild, counters preserved) — #1228.
	la.register(liveApplyNudgeAddrs, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			inst, _ := agents.Get(freshAcfg.ID)
			if inst == nil || inst.ag == nil || inst.ag.Nudger == nil {
				continue
			}
			inst.ag.Nudger.Configure(nudgeSettings(config.Resolve(fresh, freshAcfg).Nudge))
		}
		return nil
	})

	// Map-typed sections (groups, groups.calls, groups.fallbacks,
	// system.webhooks): user-defined keys, so they can't flow through the
	// hot-tag/ConfigField coverage above (config.AllFields() never produces
	// a "groups.myteam" entry) — registerMapSection is the dedicated
	// dynamic-key path (see liveApply.mapSectionAppliers doc). Groups.*
	// and Webhooks already ride along inside ResolvedAgentConfig via
	// inst.resolved.Store below, same as the ordinary applier above; the
	// extra step here is GroupResolver, a derived handle that holds its
	// own copy (mutex-guarded, see config.GroupResolver.Update) rather than
	// reading resolved live on every call.
	//
	// "agent" (distinct from config.MapFieldSections()'s global section
	// names) covers per-agent overrides of the same fields — e.g.
	// Apply("agent", "groups.myteam") for a "agent.groups.myteam=..." edit
	// (see matchMapField's agent-prefix handling in internal/config/fields.go).
	// The SAME applier body already covers it: config.Resolve(fresh, freshAcfg)
	// merges the per-agent override in regardless of which agent's edit
	// triggered the reload, since it just re-resolves every agent.
	mapSections := append([]string{"agent"}, config.MapFieldSections()...)
	la.registerMapSection(mapSections, func(fresh *config.Config) error {
		for _, freshAcfg := range fresh.Agents {
			inst, _ := agents.Get(freshAcfg.ID)
			if inst == nil {
				continue
			}
			resolved := config.Resolve(fresh, freshAcfg)
			inst.resolved.Store(resolved)
			if inst.ag != nil && inst.ag.GroupResolver != nil {
				inst.ag.GroupResolver.Update(resolved.Groups, fresh.Models, fresh.HasAPIAgent())
			}
		}
		return nil
	})

	la.register(liveApplyPinSummaryAddrs, func(fresh *config.Config) error {
		var appCfg *config.AppSpecific
		if p := fresh.Platform("app"); p != nil {
			appCfg = p.App
		}
		app.ConfigurePinSummary(appCfg)
		return nil
	})

	la.register(liveApplyAutoNameAddrs, func(fresh *config.Config) error {
		var appCfg *config.AppSpecific
		if p := fresh.Platform("app"); p != nil {
			appCfg = p.App
		}
		app.ConfigureAutoName(appCfg)
		return nil
	})

	la.register(liveApplyChatSectionsAddrs, func(fresh *config.Config) error {
		var appCfg *config.AppSpecific
		if p := fresh.Platform("app"); p != nil {
			appCfg = p.App
		}
		app.ConfigureChatSections(appCfg)
		return nil
	})

	la.register(liveApplyEndpointsAddrs, func(fresh *config.Config) error {
		if gwClients != nil {
			gwClients.Reconfigure(fresh)
		}
		return nil
	})

	// [[modelinfo]] is the first live object[] section. Reset to built-in
	// defaults, then re-apply all config entries — handles adds, modifies,
	// and removes cleanly.
	la.register(liveApplyModelInfoAddrs, func(fresh *config.Config) error {
		modelinfo.ResetToBuiltIn()
		config.ApplyModelInfo(fresh.ModelInfo)
		// The ledger's views price from a copy of these rates; re-render it so
		// SQL and Go keep pricing alike (#2111 §11.11).
		if l := accounting.Live(); l != nil {
			return l.RenderRates()
		}
		return nil
	})
}

// applySkillExtraDirs swaps the extra dirs in an agent's live skill dirs for
// fresh's (global [skills] extra_dirs, then the agent's own). Only Extra
// changes: the shared dir and skills_dir are restart-only, so a pending file
// edit to either must not ride in on an extra_dirs edit. A nil ld (an agent
// built without skills) is a no-op.
func applySkillExtraDirs(ld *skills.LiveDirs, fresh *config.Config, freshAcfg config.AgentConfig) {
	if ld == nil {
		return
	}
	ld.Set(ld.Get().WithExtra(fresh.SkillExtraDirs(freshAcfg)))
}

// loadGatewayConfig reads foci.toml the way the gateway sees it at startup:
// config.Load, then the provider platform-default layer (each registered
// provider's DefaultPlatformConfig, merged into every global and per-agent
// platform). A reload that skips the second step resolves platform fields
// differently from startup (#2749).
func loadGatewayConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path, delegator.RegisteredNames())
	if err != nil {
		return nil, err
	}
	config.ApplyProviderDefaults(cfg, providerPlatformDefaults)
	return cfg, nil
}
