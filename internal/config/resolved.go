package config

// ResolvedAgentConfig holds all config sections that participate in the
// standard 2-layer merge (per-agent → global). Computed once per agent at
// startup via Resolve(). Consumers read from this instead of calling Merge()
// themselves.
//
// Platform-aware 4-layer cascades (Display, Notify) are NOT included here;
// they have genuinely different resolution logic and remain as separate
// Merge calls at their use sites.
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

	// Webhooks is the merged System.Webhooks map (global base + agent overlay).
	Webhooks map[string]string
}

// Resolve computes a ResolvedAgentConfig by merging each 2-layer config
// section (per-agent → global). Call once per agent at startup; the result
// is treated as immutable.
func Resolve(cfg *Config, acfg AgentConfig) *ResolvedAgentConfig {
	gc := Merge(acfg.Groups, cfg.Groups)
	gc.Calls = MergeMaps(cfg.Groups.Calls, acfg.Groups.Calls)
	gc.Fallbacks = MergeMaps(cfg.Groups.Fallbacks, acfg.Groups.Fallbacks)

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
		Webhooks:        MergeMaps(cfg.Defaults.System.Webhooks, acfg.System.Webhooks),
	}
}
