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
	Groups          ResolvedGroups
	Keepalive       ResolvedKeepalive
	Background      ResolvedBackground
	MemoryFormation ResolvedMemoryFormation
	Browser         ResolvedBrowser
	Mana            ResolvedMana
	Display         ResolvedDisplay
	Notify          ResolvedNotify

	// Webhooks is the merged System.Webhooks map (global base + agent overlay).
	Webhooks map[string]string

	// Per-platform resolved display and notify (pointer-based for PATCH semantics).
	platformDisplay map[string]DisplayConfig
	platformNotify  map[string]NotifyConfig
}

// PlatformDisplay returns the 4-layer resolved DisplayConfig for a platform.
// Returns pointer-based type for use in ApplyAgentDisplaySettings PATCH patterns.
// Falls back to zero DisplayConfig if the platform has no specific resolution.
func (r *ResolvedAgentConfig) PlatformDisplay(name string) DisplayConfig {
	if d, ok := r.platformDisplay[name]; ok {
		return d
	}
	return DisplayConfig{}
}

// PlatformNotify returns the 4-layer resolved NotifyConfig for a platform.
// Returns pointer-based type — callers use accessor methods (StartupNotifyEnabled, etc.).
// Falls back to zero NotifyConfig if the platform has no specific resolution.
func (r *ResolvedAgentConfig) PlatformNotify(name string) NotifyConfig {
	if n, ok := r.platformNotify[name]; ok {
		return n
	}
	return NotifyConfig{}
}

// Resolve computes a ResolvedAgentConfig by merging all config sections
// (per-agent → global), dereferencing pointer fields, and applying defaults.
// Call once per agent at startup; the result is treated as immutable.
func Resolve(cfg *Config, acfg AgentConfig) *ResolvedAgentConfig {
	// Merge and resolve groups with per-key map merge.
	gc := Merge(acfg.Groups, cfg.Groups)
	gc.Calls = MergeMaps(cfg.Groups.Calls, acfg.Groups.Calls)
	gc.Fallbacks = MergeMaps(cfg.Groups.Fallbacks, acfg.Groups.Fallbacks)

	// Multi-platform fallback display: agent → global → all platform defaults.
	displayLayers := []DisplayConfig{acfg.Display, cfg.Defaults.Display}
	for _, p := range cfg.Platforms {
		displayLayers = append(displayLayers, p.DisplayConfig)
	}

	// Per-platform 4-layer resolution for display and notify.
	platformNames := make(map[string]bool)
	for _, p := range acfg.Platforms {
		platformNames[p.ID] = true
	}
	for _, p := range cfg.Platforms {
		platformNames[p.ID] = true
	}

	platformDisplay := make(map[string]DisplayConfig, len(platformNames))
	platformNotify := make(map[string]NotifyConfig, len(platformNames))
	for name := range platformNames {
		platformDisplay[name] = Merge(
			acfg.Platform(name).SafeDisplay(),
			acfg.Display,
			cfg.Platform(name).SafeDisplay(),
			cfg.Defaults.Display,
		)
		platformNotify[name] = Merge(
			acfg.Platform(name).SafeNotify(),
			acfg.Notify,
			cfg.Platform(name).SafeNotify(),
			cfg.Defaults.Notify,
		)
	}

	return &ResolvedAgentConfig{
		Loop:            resolveLoop(Merge(acfg.Loop, cfg.Defaults.Loop)),
		Behavior:        resolveBehavior(Merge(acfg.Behavior, cfg.Defaults.Behavior)),
		Voice:           resolveVoice(Merge(acfg.Voice, cfg.Defaults.Voice)),
		Nudge:           resolveNudge(Merge(acfg.Nudge, cfg.Defaults.Nudge)),
		System:          resolveSystem(Merge(acfg.System, cfg.Defaults.System)),
		Tools:           resolveTool(Merge(acfg.Tools.ToolConfig, cfg.Tools.ToolConfig)),
		Summary:         resolveSummary(Merge(acfg.Tools.SummaryConfig, cfg.Tools.SummaryConfig)),
		Compaction:      resolveCompaction(Merge(acfg.Sessions.CompactionConfig, cfg.Sessions.CompactionConfig)),
		Debug:           resolveDebug(Merge(acfg.Debug, cfg.Debug)),
		Groups:          resolveGroups(gc),
		Keepalive:       resolveKeepalive(Merge(acfg.Keepalive, cfg.Keepalive)),
		Background:      resolveBackground(Merge(acfg.Background, cfg.Background)),
		MemoryFormation: resolveMemoryFormation(Merge(acfg.MemoryFormation, cfg.MemoryFormation)),
		Browser:         resolveBrowser(Merge(acfg.Browser, cfg.Browser)),
		Mana:            resolveMana(Merge(acfg.Mana, cfg.Mana)),
		Display:         resolveDisplay(Merge(displayLayers...)),
		Notify:          resolveNotify(Merge(acfg.Notify, cfg.Defaults.Notify)),
		Webhooks:        MergeMaps(cfg.Defaults.System.Webhooks, acfg.System.Webhooks),
		platformDisplay: platformDisplay,
		platformNotify:  platformNotify,
	}
}
