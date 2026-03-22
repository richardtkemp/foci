package config

// ResolvedAgentConfig holds all config sections pre-merged via the
// agent→global cascade. Computed once per agent at startup via Resolve().
// Consumers read from this instead of calling Merge() themselves.
type ResolvedAgentConfig struct {
	Loop            AgentLoopConfig
	Behavior        BehaviorConfig
	Voice           VoiceConfig
	Nudge           NudgeConfig
	System          SystemConfig
	Tools           ToolConfig
	Summary         SummaryConfig
	Compaction      CompactionConfig
	Debug           DebugConfig
	Groups          GroupsConfig
	Keepalive       KeepaliveConfig
	Background      BackgroundConfig
	MemoryFormation MemoryFormationConfig
	Browser         BrowserConfig
	Mana            ManaConfig

	// Display is the multi-platform fallback display config:
	// agent → global defaults → all platform defaults (any platform).
	Display DisplayConfig

	// Notify is the base 2-layer notify config (agent → global defaults).
	Notify NotifyConfig

	// Webhooks is the merged System.Webhooks map (global base + agent overlay).
	Webhooks map[string]string

	// Per-platform resolved display and notify (4-layer cascade).
	platformDisplay map[string]DisplayConfig
	platformNotify  map[string]NotifyConfig
}

// PlatformDisplay returns the 4-layer resolved DisplayConfig for a platform.
// Falls back to the base Display if the platform has no specific resolution.
func (r *ResolvedAgentConfig) PlatformDisplay(name string) DisplayConfig {
	if d, ok := r.platformDisplay[name]; ok {
		return d
	}
	return r.Display
}

// PlatformNotify returns the 4-layer resolved NotifyConfig for a platform.
// Falls back to the base Notify if the platform has no specific resolution.
func (r *ResolvedAgentConfig) PlatformNotify(name string) NotifyConfig {
	if n, ok := r.platformNotify[name]; ok {
		return n
	}
	return r.Notify
}

// Resolve computes a ResolvedAgentConfig by merging all config sections
// (per-agent → global). Call once per agent at startup; the result is
// treated as immutable.
func Resolve(cfg *Config, acfg AgentConfig) *ResolvedAgentConfig {
	gc := Merge(acfg.Groups, cfg.Groups)
	gc.Calls = MergeMaps(cfg.Groups.Calls, acfg.Groups.Calls)
	gc.Fallbacks = MergeMaps(cfg.Groups.Fallbacks, acfg.Groups.Fallbacks)

	// Base 2-layer notify.
	baseNotify := Merge(acfg.Notify, cfg.Defaults.Notify)

	// Multi-platform fallback display: agent → global → all platform defaults.
	displayLayers := []DisplayConfig{acfg.Display, cfg.Defaults.Display}
	for _, p := range cfg.Platforms {
		displayLayers = append(displayLayers, p.DisplayConfig)
	}
	fallbackDisplay := Merge(displayLayers...)

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
		Loop:            Merge(acfg.Loop, cfg.Defaults.Loop),
		Behavior:        Merge(acfg.Behavior, cfg.Defaults.Behavior),
		Voice:           Merge(acfg.Voice, cfg.Defaults.Voice),
		Nudge:           Merge(acfg.Nudge, cfg.Defaults.Nudge),
		System:          Merge(acfg.System, cfg.Defaults.System),
		Tools:           Merge(acfg.Tools.ToolConfig, cfg.Tools.ToolConfig),
		Summary:         Merge(acfg.Tools.SummaryConfig, cfg.Tools.SummaryConfig),
		Compaction:      Merge(acfg.Sessions.CompactionConfig, cfg.Sessions.CompactionConfig),
		Debug:           Merge(acfg.Debug, cfg.Debug),
		Groups:          gc,
		Keepalive:       Merge(acfg.Keepalive, cfg.Keepalive),
		Background:      Merge(acfg.Background, cfg.Background),
		MemoryFormation: Merge(acfg.MemoryFormation, cfg.MemoryFormation),
		Browser:         Merge(acfg.Browser, cfg.Browser),
		Mana:            Merge(acfg.Mana, cfg.Mana),
		Display:         fallbackDisplay,
		Notify:          baseNotify,
		Webhooks:        MergeMaps(cfg.Defaults.System.Webhooks, acfg.System.Webhooks),
		platformDisplay: platformDisplay,
		platformNotify:  platformNotify,
	}
}
