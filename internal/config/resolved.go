package config

// ResolvedAgentConfig holds all config sections pre-merged and dereferenced
// via the agent→global cascade. Computed once per agent at startup via
// Resolve(). All fields have defaults baked in — consumers read directly
// without DerefBool/DerefStr/DerefInt.
type ResolvedAgentConfig struct {
	Loop            ResolvedLoop
	Behavior        ResolvedBehavior
	Voice           ResolvedVoice
	Nudge           ResolvedNudge
	System          ResolvedSystem
	Tools           ResolvedTool
	Summary         ResolvedSummary
	Compaction      ResolvedCompaction
	Debug           ResolvedDebug
	Environment     ResolvedEnvironment
	Groups          GroupsConfig
	Keepalive       ResolvedKeepalive
	Background      ResolvedBackground
	MemorySearch    ResolvedMemorySearch
	Reflection      ResolvedReflection
	Scheduler       ResolvedScheduler
	Browser         ResolvedBrowser
	Mana            ResolvedMana
	Display         ResolvedDisplay
	Notify          ResolvedNotify

	Permissions ResolvedPermissions

	// Webhooks is the merged System.Webhooks map (global base + agent overlay).
	Webhooks map[string]string

	// Per-platform 4-layer resolved display and notify.
	platformDisplay map[string]ResolvedDisplay
	platformNotify  map[string]ResolvedNotify
}

// PlatformDisplay returns the 4-layer resolved display config for a platform.
// Zero-value fields mean "not configured at any cascade level".
func (r *ResolvedAgentConfig) PlatformDisplay(name string) ResolvedDisplay {
	if d, ok := r.platformDisplay[name]; ok {
		return d
	}
	return ResolvedDisplay{}
}

// PlatformNotify returns the 4-layer resolved notify config for a platform.
// Defaults (e.g. StartupNotify=true) are baked in.
func (r *ResolvedAgentConfig) PlatformNotify(name string) ResolvedNotify {
	if n, ok := r.platformNotify[name]; ok {
		return n
	}
	return ResolvedNotify{}
}

// Resolve computes a ResolvedAgentConfig by merging all config sections
// (per-agent → global), dereferencing pointer fields, and applying defaults.
// Call once per agent at startup; the result is treated as immutable.
func Resolve(cfg *Config, acfg AgentConfig) *ResolvedAgentConfig {
	// Merge groups: agent groups overlay global groups (per-key map merge).
	gc := GroupsConfig{
		Groups:    MergeMaps(cfg.Groups.Groups, acfg.Groups.Groups),
		Calls:     MergeMaps(cfg.Groups.Calls, acfg.Groups.Calls),
		Fallbacks: MergeMaps(cfg.Groups.Fallbacks, acfg.Groups.Fallbacks),
	}

	// Multi-platform fallback display: agent → global → all platform defaults.
	displayLayers := []DisplayConfig{acfg.Display, cfg.Display}
	for _, p := range cfg.Platforms {
		displayLayers = append(displayLayers, p.Display)
	}

	// Per-platform 4-layer resolution for display and notify.
	platformNames := make(map[string]bool)
	for _, p := range acfg.Platforms {
		platformNames[p.ID] = true
	}
	for _, p := range cfg.Platforms {
		platformNames[p.ID] = true
	}

	platformDisplay := make(map[string]ResolvedDisplay, len(platformNames))
	platformNotify := make(map[string]ResolvedNotify, len(platformNames))
	for name := range platformNames {
		platformDisplay[name] = resolveDisplay(Merge(
			acfg.Platform(name).SafeDisplay(),
			acfg.Display,
			cfg.Platform(name).SafeDisplay(),
			cfg.Display,
		))
		platformNotify[name] = resolveNotify(Merge(
			acfg.Platform(name).SafeNotify(),
			acfg.Notify,
			cfg.Platform(name).SafeNotify(),
			cfg.Notify,
		))
	}

	return &ResolvedAgentConfig{
		Loop:            resolveLoop(Merge(acfg.Loop, cfg.AgentLoop)),
		Behavior:        resolveBehavior(Merge(acfg.Behavior, cfg.Behavior)),
		Voice:           resolveVoice(Merge(acfg.Voice, cfg.Voice)),
		Nudge:           resolveNudge(Merge(acfg.Nudge, cfg.Nudge)),
		System:          resolveSystem(Merge(acfg.System, cfg.System)),
		Tools:           resolveTool(Merge(acfg.Tools.ToolConfig, cfg.Tools.ToolConfig)),
		Summary:         resolveSummary(Merge(acfg.Tools.SummaryConfig, cfg.Tools.SummaryConfig)),
		Compaction:      resolveCompaction(Merge(acfg.Sessions.CompactionConfig, cfg.Sessions.CompactionConfig)),
		Debug:           resolveDebug(Merge(acfg.Debug, cfg.Debug)),
		Environment:     resolveEnvironment(Merge(acfg.Environment, cfg.Environment)),
		Groups:          gc,
		Keepalive:       resolveKeepalive(Merge(acfg.Keepalive, cfg.Keepalive)),
		Background:      resolveBackground(Merge(acfg.Background, cfg.Background)),
		Scheduler:       resolveScheduler(Merge(acfg.Scheduler, cfg.Scheduler)),
		MemorySearch:    resolveMemorySearch(Merge(acfg.Memory, cfg.Memory)),
		Reflection:      resolveReflection(Merge(acfg.Reflection, cfg.Reflection)),
		Browser:         resolveBrowser(Merge(acfg.Browser, cfg.Browser)),
		Mana:            resolveMana(Merge(acfg.Mana, cfg.Mana)),
		Display:         resolveDisplay(Merge(displayLayers...)),
		Notify:          resolveNotify(Merge(acfg.Notify, cfg.Notify)),
		Permissions:     resolvePermissions(acfg.Permissions, cfg.Permissions),
		Webhooks:        MergeMaps(cfg.System.Webhooks, acfg.System.Webhooks),
		platformDisplay: platformDisplay,
		platformNotify:  platformNotify,
	}
}
