package telegram

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tooldetail"
	"foci/internal/voice"
)

// telegramProvider implements platform.MessagingProvider for Telegram.
type telegramProvider struct {
	mgr             *BotManager
	connMgr         platform.ConnectionManager
	toolDetailStore *tooldetail.Store
	deps            platform.ProviderDeps

	// restoreMu guards restore, the RestoreFacetSessions params kept for facet
	// bots that connect after it ran (all of them, since connects start with
	// StartAll — #2043).
	restoreMu sync.Mutex
	restore   *platform.RestoreParams
}

// Compile-time checks.
var (
	_ platform.Connection        = (*Bot)(nil)
	_ platform.ConnectionManager = (*platform.ConnectionManagerAdapter[*Bot])(nil)
)

func (p *telegramProvider) Name() string { return "telegram" }

func (p *telegramProvider) IsConfigured(cfg *config.Config) (bool, string) {
	tg := cfg.Platform("telegram")
	if tg == nil {
		return false, "no [[platforms]] entry with id=\"telegram\""
	}
	if (tg.Access.AllowedUsersOnly == nil || *tg.Access.AllowedUsersOnly) && len(tg.Access.AllowedUsers) == 0 {
		return false, "access.allowed_users is empty (set allowed_users or set allowed_users_only=false)"
	}
	return true, ""
}

func (p *telegramProvider) Init(deps platform.ProviderDeps) error {
	p.mgr = NewBotManager()
	p.connMgr = platform.NewConnectionManagerAdapter[*Bot](p.mgr)
	p.deps = deps

	// Create tool detail store
	dbPath := deps.Config.DataPath("tool_details.db")
	store, err := tooldetail.NewStore(dbPath)
	if err != nil {
		telegramLog.Errorf("create tool detail store: %v (inline keyboard expansion will not persist)", err)
	} else {
		p.toolDetailStore = store
	}

	return nil
}

func (p *telegramProvider) ConnectionManager() platform.ConnectionManager {
	return p.connMgr
}

func (p *telegramProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
	cmds, _ := params.Commands.(*command.Registry)
	cc, _ := params.CommandContext.(command.CommandContext)
	lastMsgStore, _ := params.LastMsgStore.(*command.LastMessageStore)

	return SetupAgent(p.mgr, AgentSetupParams{
		Agent:             params.Handler,
		Commands:          cmds,
		CommandContext:    cc,
		LastMsgStore:      lastMsgStore,
		AgentConfig:       params.AgentConfig,
		GlobalConfig:      p.deps.Config,
		SecretStore:       p.deps.SecretStore,
		Sessions:          p.deps.Sessions,
		SessionIndex:      p.deps.SessionIndex,
		ToolDetailStore:   p.toolDetailStore,
		STT:               params.STT,
		TTS:               params.TTS,
		STTMap:            p.deps.STTMap,
		TTSMap:            p.deps.TTSMap,
		Ctx:               p.deps.Ctx,
		ResolveTTS:        p.deps.ResolveTTS,
		ResolveSTT:        p.deps.ResolveSTT,
		ReclaimHook:       params.ReclaimHook,
		DisplayOverrideFn: params.DisplayOverrideFn,
		Resolved:          params.Resolved, // static-cfg:ignore: plumbing — see comment on the ConfigureFacetConn call in agent_setup.go
		ResolvedLive:      params.ResolvedLive,
		OnFacetConnected:  p.restoreConnectedFacet,
	})
}

func (p *telegramProvider) SetupSharedFacet(params platform.SharedFacetParams) {
	cfg := p.deps.Config
	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil || len(tgPlat.FacetBots) == 0 || len(params.AgentOrder) == 0 {
		return
	}

	cmds, _ := params.FirstCommands.(*command.Registry)
	firstACfg := params.FirstAgentConfig
	sharedSTT := p.deps.ResolveSTT(p.deps.STTMap, cfg.STT, config.DerefStr(firstACfg.Voice.STT), voice.MergeReplacements(cfg.Voice.STTReplacements, firstACfg.Voice.STTReplacements))
	sharedTTS := p.deps.ResolveTTS(p.deps.TTSMap, cfg.TTS, config.DerefStr(firstACfg.Voice.TTS), config.DerefFloat(firstACfg.Voice.TTSRate), voice.MergeReplacements(cfg.Voice.TTSReplacements, firstACfg.Voice.TTSReplacements))

	for _, botName := range tgPlat.FacetBots {
		facetToken := config.ResolveBotToken(botName, "", p.deps.SecretStore)
		if facetToken == "" {
			telegramLog.Errorf("shared facet bot %q: token not found", botName)
			continue
		}
		facetBot := NewBot(facetToken, tgPlat.Access.AllowedUsers,
			params.FirstHandler, cmds, command.NewLastMessageStore(), "",
			telegramAPIBaseOf(tgPlat))
		if tgPlat.Access.AllowedUsersOnly != nil {
			facetBot.SetAllowedUsersOnly(*tgPlat.Access.AllowedUsersOnly)
		}
		ConfigureFacetBot(facetBot, FacetBotConfig{
			STTProvider:     sharedSTT,
			TTSProvider:     sharedTTS,
			AgentConfig:     firstACfg,
			GlobalConfig:    cfg,
			Resolved:        config.Resolve(cfg, firstACfg),
			ToolDetailStore: p.toolDetailStore,
			SessionIndex:    p.deps.SessionIndex,
		})
		p.mgr.AddSharedFacet(facetBot)
		p.mgr.ConnectBeforeRun(facetBot, fmt.Sprintf("shared facet bot %q: create", botName), connectThen(facetBot, p.restoreConnectedFacet))
	}

	if pool := p.mgr.SharedPool(); pool != nil && pool.Size() > 0 {
		sessionTTL, _ := time.ParseDuration(tgPlat.FacetSessionTTL)
		if sessionTTL > 0 {
			pool.SetSessionTTL(sessionTTL, p.deps.Sessions)
		}
		if params.ReclaimHook != nil {
			pool.ReclaimHook = params.ReclaimHook
		}
		telegramLog.Infof("%d shared facet bots registered", pool.Size())
	}
}

// RestoreFacetSessions restores facet bots that are already connected and
// keeps params for those that connect later: a facet's saved session is keyed
// by its username, which only getMe supplies (restoreConnectedFacet).
func (p *telegramProvider) RestoreFacetSessions(params platform.RestoreParams) {
	if p.deps.SessionIndex == nil {
		return
	}
	p.restoreMu.Lock()
	p.restore = &params
	p.restoreMu.Unlock()
	restoreFacetSessions(p.mgr, p.deps.SessionIndex, p.deps.Sessions, p.deps.Config, params)
}

// restoreConnectedFacet restores one facet bot's persisted session right after
// its background connect, before it is live. A no-op until
// RestoreFacetSessions has supplied the resolver (it runs before StartAll, so
// in production it always has).
func (p *telegramProvider) restoreConnectedFacet(bot *Bot) {
	p.restoreMu.Lock()
	params := p.restore
	p.restoreMu.Unlock()
	if params == nil || p.deps.SessionIndex == nil {
		return
	}
	facetMap, err := p.deps.SessionIndex.AgentMetadataByPrefix("_system", "facet:")
	if err != nil {
		telegramLog.Errorf("load facet sessions: %v", err)
		return
	}
	restoreFacetBot(bot, facetMap, p.mgr, p.deps.SessionIndex, p.deps.Sessions, p.deps.Config, *params)
}

func (p *telegramProvider) SetLifecycleCallback(agentID string, event platform.LifecycleEvent, fn func()) {
	// RegisteredPrimary: set up the callback on a bot that is still connecting.
	bot := p.mgr.RegisteredPrimary(agentID)
	if bot == nil {
		return
	}
	if event == platform.OnUserMessage {
		bot.OnUserMessage = fn
	}
}

func (p *telegramProvider) ToolDetailStore() platform.ToolDetailStore {
	if p.toolDetailStore == nil {
		return nil
	}
	return p.toolDetailStore
}

func (p *telegramProvider) AgentPreFlight(agentID string) []string {
	tokenSecret := "telegram." + agentID
	if _, ok := p.deps.SecretStore.Get(tokenSecret); !ok {
		return []string{fmt.Sprintf(
			"Secret `%s` not found — add it with `/secrets set %s <token>` before starting.",
			tokenSecret, tokenSecret,
		)}
	}
	return nil
}

func (p *telegramProvider) DefaultPlatformConfig() config.PlatformConfig {
	off := config.ToolCallOff
	thinkOff := config.ShowThinkingOff
	dw := 44
	twl := 5
	ts := "pretty"
	so := false
	rm := true
	sn := true
	return config.PlatformConfig{
		ID: "telegram",
		Notify: config.NotifyConfig{
			StartupNotify: &sn,
		},
		Display: config.DisplayConfig{
			ShowToolCalls:  &off,
			ShowThinking:   &thinkOff,
			StreamOutput:   &so,
			StreamInterval: config.Ptr[string]("250ms"),
			DisplayWidth:   &dw,
			TableWrapLines: &twl,
			TableStyle:     &ts,
		},
		Access: config.AccessConfig{
			RequireMention: &rm,
		},
		FacetSessionTTL: "60m",
		Telegram: &config.TelegramSpecific{
			LongPollTimeout: "30s",
		},
	}
}

func (p *telegramProvider) ValidateConfig(_ config.PlatformConfig) []string {
	return nil
}

func (p *telegramProvider) Close() error {
	if p.toolDetailStore != nil {
		p.toolDetailStore.ExpireAndVacuum()
		return p.toolDetailStore.Close()
	}
	return nil
}

// restoreFacetSessions restores persisted facet session mappings after restart.
func restoreFacetSessions(
	mgr *BotManager,
	idx *session.SessionIndex,
	sessions *session.Store,
	cfg *config.Config,
	params platform.RestoreParams,
) {
	// Load all facet mappings at once
	facetMap, err := idx.AgentMetadataByPrefix("_system", "facet:")
	if err != nil {
		telegramLog.Errorf("load facet sessions: %v", err)
		return
	}
	if len(facetMap) == 0 {
		return
	}

	type poolInfo struct {
		pool *Pool
		name string
	}
	var pools []poolInfo
	for _, id := range params.AgentOrder {
		if pool := mgr.Pool(id); pool != nil {
			pools = append(pools, poolInfo{pool: pool, name: "agent/" + id})
		}
	}
	if sp := mgr.SharedPool(); sp != nil {
		pools = append(pools, poolInfo{pool: sp, name: "shared"})
	}

	restored := 0
	for _, pi := range pools {
		pi.pool.ForEach(func(bot *Bot) {
			if restoreFacetBot(bot, facetMap, mgr, idx, sessions, cfg, params) {
				restored++
			}
		})
	}
	if restored > 0 {
		telegramLog.Infof("restored %d facet session(s) from state", restored)
	}
}

// restoreFacetBot reattaches one facet bot to its persisted session, if it has
// one that still exists. A bot with no username yet (not connected) is
// skipped; it is restored by restoreConnectedFacet when it connects.
func restoreFacetBot(
	bot *Bot,
	facetMap map[string]string,
	mgr *BotManager,
	idx *session.SessionIndex,
	sessions *session.Store,
	cfg *config.Config,
	params platform.RestoreParams,
) bool {
	username := bot.Username()
	if username == "" {
		return false
	}
	savedKey, ok := facetMap["facet:"+username]
	if !ok || savedKey == "" {
		return false
	}

	if sessions.LastActivity(savedKey) == "n/a" {
		telegramLog.Infof("facet restore: @%s session %s no longer exists, cleaning up", username, savedKey)
		_ = idx.DeleteAgentMetadata("_system", "facet:"+username)
		return false
	}

	bot.SetSessionKeyDirect(savedKey)

	agentID := session.AgentIDFromKey(savedKey)
	if handler, commands, commandContext, acfg, ok := params.Resolver(agentID); ok {
		cmds, _ := commands.(*command.Registry)
		bot.SetHandlerAndCommands(handler, cmds)
		if cc, ok := commandContext.(command.CommandContext); ok {
			bot.SetCommandContext(cc)
		}
		rc := config.Resolve(cfg, acfg)
		ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("telegram"), rc.Debug, rc.TelegramLongPollTimeout)
		bot.fileMode, _ = config.ParseFileMode(cfg.FileMode)
	}

	if agentID != "" {
		// RegisteredPrimary: the chat ID is persisted state, not a connection.
		if primary := mgr.RegisteredPrimary(agentID); primary != nil {
			if chatID := primary.ChatID(); chatID != 0 {
				bot.SetChatID(chatID)
			}
		}
	}

	telegramLog.Infof("facet restore: @%s → %s", username, savedKey)
	return true
}

// Compile-time check.
var _ platform.MessagingProvider = (*telegramProvider)(nil)
