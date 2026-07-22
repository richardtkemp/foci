package config

// Resolved* types are concrete (non-pointer) versions of the pointer-based
// config sub-types. Produced by Resolve() with all defaults baked in.
// Consumers read fields directly — no DerefBool/DerefStr/DerefInt needed.

type ResolvedLoop struct {
	MaxOutputTokens               int
	MaxToolLoops                  int
	DuplicateMessages             bool
	BatchPartialAssistantMessages bool
	BatchPartialJoiner            string
	Streaming                     bool
}

func resolveLoop(m AgentLoopConfig) ResolvedLoop {
	return ResolvedLoop{
		MaxOutputTokens:               DerefInt(m.MaxOutputTokens),
		MaxToolLoops:                  DerefInt(m.MaxToolLoops),
		DuplicateMessages:             DerefBool(m.DuplicateMessages),
		BatchPartialAssistantMessages: DerefBool(m.BatchPartialAssistantMessages),
		BatchPartialJoiner:            DerefStr(m.BatchPartialJoiner),
		Streaming:                     DerefBoolDefault(m.Streaming, true),
	}
}

type ResolvedBehavior struct {
	SteerMode             bool // default true
	GroupThrottle         string
	TurnLockWarnThreshold string
	EnableStopAliases     bool // default true
	StopAliases           []string
}

func resolveBehavior(m BehaviorConfig) ResolvedBehavior {
	return ResolvedBehavior{
		SteerMode:             DerefBool(m.SteerMode),
		GroupThrottle:         DerefStr(m.GroupThrottle),
		TurnLockWarnThreshold: DerefStr(m.TurnLockWarnThreshold),
		EnableStopAliases:     DerefBool(m.EnableStopAliases),
		StopAliases:           m.StopAliases,
	}
}

type ResolvedVoice struct {
	TTS             string
	STT             string
	TTSRate         float64
	TTSReplacements map[string]string
	STTReplacements map[string]string
}

func resolveVoice(m VoiceConfig) ResolvedVoice {
	return ResolvedVoice{
		TTS:             DerefStr(m.TTS),
		STT:             DerefStr(m.STT),
		TTSRate:         DerefFloat(m.TTSRate),
		TTSReplacements: m.TTSReplacements,
		STTReplacements: m.STTReplacements,
	}
}

type ResolvedNudge struct {
	NudgeEnable                     bool // default true
	NudgeAutoExtract                bool // default true
	NudgeCooldown                   int
	NudgeMaxPerBatch                int
	NudgeMaxPerTurn                 int // 0 = unlimited
	NudgeExtractionModel            string
	NudgePreAnswerGate              bool
	NudgePreAnswerMinTools          int
	NudgeDefaultEnable              bool // default true
	NudgeDefaultFrequency           int
	NudgeDefaultScratchpadFrequency int
	NudgeDefaultBraindeadThreshold  int
	NudgeDefaultBraindeadPrompt     string
}

func resolveNudge(m NudgeConfig) ResolvedNudge {
	return ResolvedNudge{
		NudgeEnable:                     DerefBool(m.NudgeEnable),
		NudgeAutoExtract:                DerefBool(m.NudgeAutoExtract),
		NudgeCooldown:                   DerefInt(m.NudgeCooldown),
		NudgeMaxPerBatch:                DerefInt(m.NudgeMaxPerBatch),
		NudgeMaxPerTurn:                 DerefInt(m.NudgeMaxPerTurn),
		NudgeExtractionModel:            DerefStr(m.NudgeExtractionModel),
		NudgePreAnswerGate:              DerefBool(m.NudgePreAnswerGate),
		NudgePreAnswerMinTools:          DerefInt(m.NudgePreAnswerMinTools),
		NudgeDefaultEnable:              DerefBool(m.NudgeDefaultEnable),
		NudgeDefaultFrequency:           DerefInt(m.NudgeDefaultFrequency),
		NudgeDefaultScratchpadFrequency: DerefInt(m.NudgeDefaultScratchpadFrequency),
		NudgeDefaultBraindeadThreshold:  DerefInt(m.NudgeDefaultBraindeadThreshold),
		NudgeDefaultBraindeadPrompt:     DerefStr(m.NudgeDefaultBraindeadPrompt),
	}
}

type ResolvedSystem struct {
	SystemFiles []string
	Webhooks    map[string]string
}

func resolveSystem(m SystemConfig) ResolvedSystem {
	return ResolvedSystem{
		SystemFiles: m.SystemFiles,
		Webhooks:    m.Webhooks,
	}
}

type ResolvedTool struct {
	ExecAutoBackground  int
	MaxConcurrentSpawns int
	ExploreMaxDepth     int
	MaxUploadFileSize   int64
	MaxFileReadBytes    int64
	HTTPMaxSpillBytes   int64
	TmuxAutopilot       bool
	TmuxWatchThreshold  string
	TmuxSessionTTL      string
	SearchProvider      string
	FetchProvider       string
	TodoFormat          string
}

func resolveTool(m ToolConfig) ResolvedTool {
	return ResolvedTool{
		ExecAutoBackground:  DerefInt(m.ExecAutoBackground),
		MaxConcurrentSpawns: DerefInt(m.MaxConcurrentSpawns),
		ExploreMaxDepth:     DerefInt(m.ExploreMaxDepth),
		MaxUploadFileSize:   DerefInt64(m.MaxUploadFileSize),
		MaxFileReadBytes:    DerefInt64(m.MaxFileReadBytes),
		HTTPMaxSpillBytes:   DerefInt64(m.HTTPMaxSpillBytes),
		TmuxAutopilot:       DerefBool(m.TmuxAutopilot),
		TmuxWatchThreshold:  DerefStr(m.TmuxWatchThreshold),
		TmuxSessionTTL:      DerefStr(m.TmuxSessionTTL),
		SearchProvider:      DerefStr(m.SearchProvider),
		FetchProvider:       DerefStr(m.FetchProvider),
		TodoFormat:          DerefStr(m.TodoFormat),
	}
}

type ResolvedSummary struct {
	MaxResultChars       int
	MaxSummaryChars      int
	AutoSummarise        bool // default true
	SummaryContextTurns  int
	SummaryContextChars  int
	MaxSummaryInputChars int
	MaxImagePixels       int
}

func resolveSummary(m SummaryConfig) ResolvedSummary {
	return ResolvedSummary{
		MaxResultChars:       DerefInt(m.MaxResultChars),
		MaxSummaryChars:      DerefInt(m.MaxSummaryChars),
		AutoSummarise:        DerefBool(m.AutoSummarise),
		SummaryContextTurns:  DerefInt(m.SummaryContextTurns),
		SummaryContextChars:  DerefInt(m.SummaryContextChars),
		MaxSummaryInputChars: DerefInt(m.MaxSummaryInputChars),
		MaxImagePixels:       DerefInt(m.MaxImagePixels),
	}
}

type ResolvedCompaction struct {
	CompactionThreshold        float64 // flat fraction anchor (0.8 when unset)
	CompactionThresholdSet     bool    // true = user pinned a flat %; false = use the non-linear curve
	CompactionHandoffMsg       string
	CompactionPreserveMessages int
	FacetNoCompact             bool
	ReloadOnCompact            bool
}

func resolveCompaction(m CompactionConfig) ResolvedCompaction {
	threshold := 0.8
	if m.CompactionThreshold != nil {
		threshold = *m.CompactionThreshold
	}
	return ResolvedCompaction{
		CompactionThreshold:        threshold,
		CompactionThresholdSet:     m.CompactionThreshold != nil,
		CompactionHandoffMsg:       DerefStr(m.CompactionHandoffMsg),
		CompactionPreserveMessages: DerefInt(m.CompactionPreserveMessages),
		FacetNoCompact:             DerefBool(m.FacetNoCompact),
		ReloadOnCompact:            m.ReloadOnCompact == nil || *m.ReloadOnCompact, // default ON
	}
}

type ResolvedDebug struct {
	LogAPIKeySuffix bool
	MessagesInLog   bool
	CacheBustDetect bool
}

func resolveDebug(m DebugConfig) ResolvedDebug {
	return ResolvedDebug{
		LogAPIKeySuffix: DerefBool(m.LogAPIKeySuffix),
		MessagesInLog:   DerefBool(m.MessagesInLog),
		CacheBustDetect: DerefBool(m.CacheBustDetect),
	}
}

type ResolvedEnvironment struct {
	Enabled  bool
	DocsPath string
}

func resolveEnvironment(m EnvironmentConfig) ResolvedEnvironment {
	return ResolvedEnvironment{
		Enabled:  DerefBool(m.Enabled),
		DocsPath: DerefStr(m.DocsPath),
	}
}

type ResolvedKeepalive struct {
	Enabled          bool
	Interval         string
	Prompt           string
	WarmOpenAppChats bool
	MaxUserIdle      string
	ForceInSession   bool // force in-session (no backend fork) even on a fork-capable backend
}

func resolveKeepalive(m KeepaliveConfig) ResolvedKeepalive {
	return ResolvedKeepalive{
		Enabled:          DerefBool(m.Enabled),
		Interval:         DerefStr(m.Interval),
		Prompt:           DerefStr(m.Prompt),
		WarmOpenAppChats: DerefBool(m.WarmOpenAppChats),
		MaxUserIdle:      DerefStr(m.MaxUserIdle),
		ForceInSession:   DerefBool(m.ForceInSession),
	}
}

type ResolvedScheduler struct {
	TickInterval string // default "30s"
}

func resolveScheduler(m SchedulerConfig) ResolvedScheduler {
	return ResolvedScheduler{
		TickInterval: DerefStr(m.TickInterval),
	}
}

type ResolvedBackground struct {
	Enabled          bool
	Interval         string
	Prompt           string
	CanRunBackground string
	ForceInSession   bool // force in-session (no backend fork) even on a fork-capable backend
}

func resolveBackground(m BackgroundConfig) ResolvedBackground {
	return ResolvedBackground{
		Enabled:          DerefBool(m.Enabled),
		Interval:         DerefStr(m.Interval),
		Prompt:           DerefStr(m.Prompt),
		CanRunBackground: DerefStr(m.CanRunBackground),
		ForceInSession:   DerefBool(m.ForceInSession),
	}
}

type ResolvedReflection struct {
	IntervalEnabled       bool // default true
	Interval              string
	IntervalPrompt        string
	SessionEndEnabled     bool // default true
	SessionEndPrompt      string
	CompactionEnabled     bool // default true
	CompactionPrompt      string
	BackendQuietPeriod    string // default "5m"
	NotifyOnSkillCreation bool   // default true
	ForceInSession        bool   // force interval reflection in-session (no backend fork) even on a fork-capable backend
}

func resolveReflection(m ReflectionConfig) ResolvedReflection {
	return ResolvedReflection{
		IntervalEnabled:       DerefBool(m.IntervalEnabled),
		Interval:              DerefStr(m.Interval),
		IntervalPrompt:        DerefStr(m.IntervalPrompt),
		SessionEndEnabled:     DerefBool(m.SessionEndEnabled),
		SessionEndPrompt:      DerefStr(m.SessionEndPrompt),
		CompactionEnabled:     DerefBool(m.CompactionEnabled),
		CompactionPrompt:      DerefStr(m.CompactionPrompt),
		BackendQuietPeriod:    DerefStr(m.BackendQuietPeriod),
		NotifyOnSkillCreation: DerefBool(m.NotifyOnSkillCreation),
		ForceInSession:        DerefBool(m.ForceInSession),
	}
}

type ResolvedMaintenance struct {
	ConsolidationEnabled        bool // default true
	ConsolidationTime           string
	ConsolidationPrompt         string
	ConsolidationMaxIdle        string // default "1h"; skip consolidation if idle longer
	ResetTime                   string // "" = disabled
	ResetIdleGuard              string // default "55m"
	ConsolidationForceInSession bool   // force consolidation in-session/independent (no backend fork) even on a fork-capable backend
}

func resolveMaintenance(m MaintenanceConfig) ResolvedMaintenance {
	return ResolvedMaintenance{
		ConsolidationEnabled:        DerefBool(m.ConsolidationEnabled),
		ConsolidationTime:           DerefStr(m.ConsolidationTime),
		ConsolidationPrompt:         DerefStr(m.ConsolidationPrompt),
		ConsolidationMaxIdle:        DerefStr(m.ConsolidationMaxIdle),
		ResetTime:                   DerefStr(m.ResetTime),
		ResetIdleGuard:              DerefStr(m.ResetIdleGuard),
		ConsolidationForceInSession: DerefBool(m.ConsolidationForceInSession),
	}
}

type ResolvedMemorySearch struct {
	SearchBackend      string
	ReindexDebounce    string
	ConversationWeight float64
	SearchLimit        int
	SweepInterval      string
	TemporalDecay      bool
	DecayHalfLife      float64
	DecayBoost         float64
	EvergreenPatterns  []string
}

func resolveMemorySearch(m MemoryConfig) ResolvedMemorySearch {
	return ResolvedMemorySearch{
		SearchBackend:      DerefStr(m.SearchBackend),
		ReindexDebounce:    DerefStr(m.ReindexDebounce),
		ConversationWeight: DerefFloat(m.ConversationWeight),
		SearchLimit:        DerefInt(m.SearchLimit),
		SweepInterval:      DerefStr(m.SweepInterval),
		TemporalDecay:      DerefBool(m.TemporalDecay),
		DecayHalfLife:      DerefFloat(m.DecayHalfLife),
		DecayBoost:         DerefFloat(m.DecayBoost),
		EvergreenPatterns:  m.EvergreenPatterns,
	}
}

type ResolvedBrowser struct {
	Enabled        bool
	Headless       bool
	TimeoutSec     int
	UserDataDir    string
	ExecutablePath string
	DOMStableSec   float64 // default 1.0
	DOMStableDiff  float64 // default 0.2
}

func resolveBrowser(m BrowserConfig) ResolvedBrowser {
	return ResolvedBrowser{
		Enabled:        DerefBool(m.Enabled),
		Headless:       DerefBool(m.Headless),
		TimeoutSec:     DerefInt(m.TimeoutSec),
		UserDataDir:    DerefStr(m.UserDataDir),
		ExecutablePath: DerefStr(m.ExecutablePath),
		DOMStableSec:   DerefFloat(m.DOMStableSec),
		DOMStableDiff:  DerefFloat(m.DOMStableDiff),
	}
}

type ResolvedDisplay struct {
	ShowToolCalls         string // ToolCallDisplay as string; "" = not configured
	ShowThinking          string // ShowThinking as string; "" = not configured
	StreamOutput          bool
	StreamInterval        string
	DisplayWidth          int
	TableWrapLines        int
	TableStyle            string
	ReceivedFilesDir      string
	InjectedMessageHeader string
	Statusline            string
}

func resolveDisplay(m DisplayConfig) ResolvedDisplay {
	var stc string
	if m.ShowToolCalls != nil {
		stc = string(*m.ShowToolCalls)
	}
	var st string
	if m.ShowThinking != nil {
		st = string(*m.ShowThinking)
	}
	return ResolvedDisplay{
		ShowToolCalls:         stc,
		ShowThinking:          st,
		StreamOutput:          DerefBool(m.StreamOutput),
		StreamInterval:        DerefStr(m.StreamInterval),
		DisplayWidth:          DerefInt(m.DisplayWidth),
		TableWrapLines:        DerefInt(m.TableWrapLines),
		TableStyle:            DerefStr(m.TableStyle),
		ReceivedFilesDir:      DerefStr(m.ReceivedFilesDir),
		InjectedMessageHeader: DerefStr(m.InjectedMessageHeader),
		Statusline:            DerefStr(m.Statusline),
	}
}

type ResolvedNotify struct {
	InjectAgentWarnings InjectionLevel // default InjectionOff
	InjectChatWarnings  InjectionLevel // default InjectionOff
	StartupNotify       bool           // default true
	CompactionNotify    bool           // default true
	TaskListNotify      bool           // default true
	CompactionDebug     bool           // default false
	WarningMaxPerWindow int            // default 3
}

func resolveNotify(m NotifyConfig) ResolvedNotify {
	return ResolvedNotify{
		InjectAgentWarnings: m.InjectAgentWarningsLevel(),
		InjectChatWarnings:  m.InjectChatWarningsLevel(),
		StartupNotify:       m.StartupNotifyEnabled(),
		CompactionNotify:    m.CompactionNotifyEnabled(),
		TaskListNotify:      m.TaskListNotifyEnabled(),
		CompactionDebug:     m.CompactionDebugEnabled(),
		WarningMaxPerWindow: DerefInt(m.WarningMaxPerWindow),
	}
}

type ResolvedPermissions struct {
	AutoApproveRules           []string // union of global + per-agent patterns
	AutoApproveCommonReadonly  bool     // enable built-in read-only allowlist (default true)
	AutoApproveCommonSafeWrite bool     // enable built-in safe-write allowlist (default false)
}

func resolvePermissions(agent, global PermissionsConfig) ResolvedPermissions {
	// Union: both global and agent rules apply (not first-non-nil).
	seen := make(map[string]bool, len(agent.AutoApprove)+len(global.AutoApprove))
	var rules []string
	for _, r := range agent.AutoApprove {
		if !seen[r] {
			rules = append(rules, r)
			seen[r] = true
		}
	}
	for _, r := range global.AutoApprove {
		if !seen[r] {
			rules = append(rules, r)
			seen[r] = true
		}
	}

	// Bool: standard cascade (agent → global → default true).
	commonReadonly := true
	if p := First(agent.AutoApproveCommonReadonly, global.AutoApproveCommonReadonly); p != nil {
		commonReadonly = *p
	}

	// Bool: standard cascade (agent → global → default false). Safe-write is
	// opt-in because the rules aren't path-scoped — enabling it trusts the
	// agent not to target paths outside its workspace.
	commonSafeWrite := false
	if p := First(agent.AutoApproveCommonSafeWrite, global.AutoApproveCommonSafeWrite); p != nil {
		commonSafeWrite = *p
	}

	return ResolvedPermissions{
		AutoApproveRules:           rules,
		AutoApproveCommonReadonly:  commonReadonly,
		AutoApproveCommonSafeWrite: commonSafeWrite,
	}
}
