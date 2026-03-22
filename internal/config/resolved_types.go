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
	CacheTTL                      string
}

func resolveLoop(m AgentLoopConfig) ResolvedLoop {
	return ResolvedLoop{
		MaxOutputTokens:               DerefInt(m.MaxOutputTokens),
		MaxToolLoops:                  DerefInt(m.MaxToolLoops),
		DuplicateMessages:             DerefBool(m.DuplicateMessages),
		BatchPartialAssistantMessages: DerefBool(m.BatchPartialAssistantMessages),
		BatchPartialJoiner:            DerefStr(m.BatchPartialJoiner),
		CacheTTL:                      DerefStr(m.CacheTTL),
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
		SteerMode:             m.SteerMode == nil || *m.SteerMode,
		GroupThrottle:         DerefStr(m.GroupThrottle),
		TurnLockWarnThreshold: DerefStr(m.TurnLockWarnThreshold),
		EnableStopAliases:     m.EnableStopAliases == nil || *m.EnableStopAliases,
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
		NudgeEnable:                     m.NudgeEnable == nil || *m.NudgeEnable,
		NudgeAutoExtract:                m.NudgeAutoExtract == nil || *m.NudgeAutoExtract,
		NudgeCooldown:                   DerefInt(m.NudgeCooldown),
		NudgeMaxPerBatch:                DerefInt(m.NudgeMaxPerBatch),
		NudgePreAnswerGate:              DerefBool(m.NudgePreAnswerGate),
		NudgePreAnswerMinTools:          DerefInt(m.NudgePreAnswerMinTools),
		NudgeDefaultEnable:              m.NudgeDefaultEnable == nil || *m.NudgeDefaultEnable,
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
		AutoSummarise:        m.AutoSummarise == nil || *m.AutoSummarise,
		SummaryContextTurns:  DerefInt(m.SummaryContextTurns),
		SummaryContextChars:  DerefInt(m.SummaryContextChars),
		MaxSummaryInputChars: DerefInt(m.MaxSummaryInputChars),
		MaxImagePixels:       DerefInt(m.MaxImagePixels),
	}
}

type ResolvedCompaction struct {
	CompactionThreshold                     float64 // default 0.8
	CompactionSummaryPrompt                 string
	CompactionHandoffMsg                    string
	CompactionPreserveMessages              int
	CompactionEffort                        string
	FacetNoCompact                          bool
	AutocompactBeforeManaRefresh            bool
	AutocompactBeforeManaRefreshThreshold   string
	AutocompactBeforeManaRefreshFactor      float64
	AutocompactBeforeManaRefreshPreserve    *int // nil = use percentage fallback
	AutocompactBeforeManaRefreshPreservePct float64
}

func resolveCompaction(m CompactionConfig) ResolvedCompaction {
	threshold := DerefFloat(m.CompactionThreshold)
	if threshold == 0 {
		threshold = 0.8
	}
	return ResolvedCompaction{
		CompactionThreshold:                     threshold,
		CompactionSummaryPrompt:                 DerefStr(m.CompactionSummaryPrompt),
		CompactionHandoffMsg:                    DerefStr(m.CompactionHandoffMsg),
		CompactionPreserveMessages:              DerefInt(m.CompactionPreserveMessages),
		CompactionEffort:                        DerefStr(m.CompactionEffort),
		FacetNoCompact:                          DerefBool(m.FacetNoCompact),
		AutocompactBeforeManaRefresh:            DerefBool(m.AutocompactBeforeManaRefresh),
		AutocompactBeforeManaRefreshThreshold:   DerefStr(m.AutocompactBeforeManaRefreshThreshold),
		AutocompactBeforeManaRefreshFactor:      DerefFloat(m.AutocompactBeforeManaRefreshFactor),
		AutocompactBeforeManaRefreshPreserve:    m.AutocompactBeforeManaRefreshPreserve, // keep *int
		AutocompactBeforeManaRefreshPreservePct: DerefFloat(m.AutocompactBeforeManaRefreshPreservePct),
	}
}

type ResolvedDebug struct {
	LogAPIKeySuffix bool
	MessagesInLog   bool
}

func resolveDebug(m DebugConfig) ResolvedDebug {
	return ResolvedDebug{
		LogAPIKeySuffix: DerefBool(m.LogAPIKeySuffix),
		MessagesInLog:   DerefBool(m.MessagesInLog),
	}
}

type ResolvedGroups struct {
	Powerful  string
	Fast      string
	Cheap     string
	Calls     map[string]string
	Fallbacks map[string]string
}

func resolveGroups(m GroupsConfig) ResolvedGroups {
	return ResolvedGroups{
		Powerful:  DerefStr(m.Powerful),
		Fast:      DerefStr(m.Fast),
		Cheap:     DerefStr(m.Cheap),
		Calls:     m.Calls,
		Fallbacks: m.Fallbacks,
	}
}

type ResolvedKeepalive struct {
	Enabled  bool
	Interval string
	Prompt   string
}

func resolveKeepalive(m KeepaliveConfig) ResolvedKeepalive {
	return ResolvedKeepalive{
		Enabled:  DerefBool(m.Enabled),
		Interval: DerefStr(m.Interval),
		Prompt:   DerefStr(m.Prompt),
	}
}

type ResolvedBackground struct {
	Enabled  bool
	Interval string
	Prompt   string
}

func resolveBackground(m BackgroundConfig) ResolvedBackground {
	return ResolvedBackground{
		Enabled:  DerefBool(m.Enabled),
		Interval: DerefStr(m.Interval),
		Prompt:   DerefStr(m.Prompt),
	}
}

type ResolvedMemoryFormation struct {
	IntervalEnabled       bool // default true
	Interval              string
	IntervalPrompt        string
	ConsolidationEnabled  bool // default true
	ConsolidationInterval string
	ConsolidationPrompt   string
	SessionEndEnabled     bool // default true
	SessionEndPrompt      string
	CompactionEnabled     bool // default true
	CompactionPrompt      string
}

func resolveMemoryFormation(m MemoryFormationConfig) ResolvedMemoryFormation {
	return ResolvedMemoryFormation{
		IntervalEnabled:       m.IntervalEnabled == nil || *m.IntervalEnabled,
		Interval:              DerefStr(m.Interval),
		IntervalPrompt:        DerefStr(m.IntervalPrompt),
		ConsolidationEnabled:  m.ConsolidationEnabled == nil || *m.ConsolidationEnabled,
		ConsolidationInterval: DerefStr(m.ConsolidationInterval),
		ConsolidationPrompt:   DerefStr(m.ConsolidationPrompt),
		SessionEndEnabled:     m.SessionEndEnabled == nil || *m.SessionEndEnabled,
		SessionEndPrompt:      DerefStr(m.SessionEndPrompt),
		CompactionEnabled:     m.CompactionEnabled == nil || *m.CompactionEnabled,
		CompactionPrompt:      DerefStr(m.CompactionPrompt),
	}
}

type ResolvedBrowser struct {
	Enabled        bool
	Headless       bool
	TimeoutSec     int
	UserDataDir    string
	ExecutablePath string
	Incognito      bool
	DOMStableSec   float64 // default 1.0
	DOMStableDiff  float64 // default 0.2
}

func resolveBrowser(m BrowserConfig) ResolvedBrowser {
	domStable := DerefFloat(m.DOMStableSec)
	if domStable <= 0 {
		domStable = 1.0
	}
	domDiff := DerefFloat(m.DOMStableDiff)
	if domDiff <= 0 {
		domDiff = 0.2
	}
	return ResolvedBrowser{
		Enabled:        DerefBool(m.Enabled),
		Headless:       DerefBool(m.Headless),
		TimeoutSec:     DerefInt(m.TimeoutSec),
		UserDataDir:    DerefStr(m.UserDataDir),
		ExecutablePath: DerefStr(m.ExecutablePath),
		Incognito:      DerefBool(m.Incognito),
		DOMStableSec:   domStable,
		DOMStableDiff:  domDiff,
	}
}

type ResolvedMana struct {
	Name             string
	Thresholds       []int
	RestoreThreshold int
	InvestInterval   string
}

func resolveMana(m ManaConfig) ResolvedMana {
	return ResolvedMana{
		Name:             DerefStr(m.Name),
		Thresholds:       m.Thresholds,
		RestoreThreshold: DerefInt(m.RestoreThreshold),
		InvestInterval:   DerefStr(m.InvestInterval),
	}
}

type ResolvedDisplay struct {
	ShowToolCalls         string // ToolCallDisplay as string; "" = not configured
	ShowThinking          string // ShowThinking as string; "" = not configured
	StreamOutput          bool
	StreamInterval        string
	Streaming             bool
	DisplayWidth          int
	ReceivedFilesDir      string
	InjectedMessageHeader string
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
		Streaming:             DerefBool(m.Streaming),
		DisplayWidth:          DerefInt(m.DisplayWidth),
		ReceivedFilesDir:      DerefStr(m.ReceivedFilesDir),
		InjectedMessageHeader: DerefStr(m.InjectedMessageHeader),
	}
}

type ResolvedNotify struct {
	InjectAgentWarnings InjectionLevel // default InjectionOff
	InjectChatWarnings  InjectionLevel // default InjectionOff
	StartupNotify       bool           // default true
	CompactionNotify    bool           // default true
	TaskListNotify      bool           // default true
	CompactionDebug     bool           // default false
}

func resolveNotify(m NotifyConfig) ResolvedNotify {
	return ResolvedNotify{
		InjectAgentWarnings: m.InjectAgentWarningsLevel(),
		InjectChatWarnings:  m.InjectChatWarningsLevel(),
		StartupNotify:       m.StartupNotifyEnabled(),
		CompactionNotify:    m.CompactionNotifyEnabled(),
		TaskListNotify:      m.TaskListNotifyEnabled(),
		CompactionDebug:     m.CompactionDebugEnabled(),
	}
}
