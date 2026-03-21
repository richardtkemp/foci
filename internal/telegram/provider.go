package telegram

import (
	"fmt"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tooldetail"
	"foci/internal/voice"
)

// telegramProvider implements platform.MessagingProvider for Telegram.
type telegramProvider struct {
	mgr            *BotManager
	connMgr        *ConnectionManagerAdapter
	toolDetailStore *tooldetail.Store
	deps           platform.ProviderDeps
}

func (p *telegramProvider) Name() string { return "telegram" }

func (p *telegramProvider) IsConfigured(cfg *config.Config) bool {
	tg := cfg.Platform("telegram")
	return tg != nil && len(tg.AllowedUsers) > 0
}

func (p *telegramProvider) Init(deps platform.ProviderDeps) error {
	p.mgr = NewBotManager()
	p.connMgr = &ConnectionManagerAdapter{BotManager: p.mgr}
	p.deps = deps

	// Create tool detail store
	dbPath := deps.Config.DataPath("tool_details.db")
	store, err := tooldetail.NewStore(dbPath)
	if err != nil {
		log.Errorf("telegram", "create tool detail store: %v (inline keyboard expansion will not persist)", err)
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
		Agent:          params.Handler,
		Commands:       cmds,
		CommandContext: cc,
		LastMsgStore:   lastMsgStore,
		AgentConfig:     params.AgentConfig,
		GlobalConfig:    p.deps.Config,
		SecretStore:     p.deps.SecretStore,
		Sessions:        p.deps.Sessions,
		SessionIndex:    p.deps.SessionIndex,
		ToolDetailStore: p.toolDetailStore,
		STT:             params.STT,
		TTS:             params.TTS,
		STTMap:          p.deps.STTMap,
		TTSMap:          p.deps.TTSMap,
		Ctx:             p.deps.Ctx,
		ResolveTTS:      p.deps.ResolveTTS,
		ResolveSTT:      p.deps.ResolveSTT,
		ReclaimHook:       params.ReclaimHook,
		DisplayOverrideFn: params.DisplayOverrideFn,
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
	sharedSTT := p.deps.ResolveSTT(p.deps.STTMap, cfg.STT, config.DerefStr(firstACfg.Defaults.STT), voice.MergeReplacements(cfg.Defaults.STTReplacements, firstACfg.Defaults.STTReplacements))
	sharedTTS := p.deps.ResolveTTS(p.deps.TTSMap, cfg.TTS, config.DerefStr(firstACfg.Defaults.TTS), config.DerefFloat(firstACfg.Defaults.TTSRate), voice.MergeReplacements(cfg.Defaults.TTSReplacements, firstACfg.Defaults.TTSReplacements))

	for _, botName := range tgPlat.FacetBots {
		facetToken := config.ResolveBotToken(botName, "", p.deps.SecretStore)
		if facetToken == "" {
			log.Errorf("telegram", "shared facet bot %q: token not found", botName)
			continue
		}
		facetBot, err := NewBot(facetToken, tgPlat.AllowedUsers,
			params.FirstHandler, cmds, command.NewLastMessageStore(), "")
		if err != nil {
			log.Errorf("telegram", "shared facet bot %q: create: %v", botName, err)
			continue
		}
		ConfigureFacetBot(facetBot, FacetBotConfig{
			STTProvider:     sharedSTT,
			TTSProvider:     sharedTTS,
			AgentConfig:     firstACfg,
			GlobalConfig:    cfg,
			ToolDetailStore: p.toolDetailStore,
			SessionIndex:    p.deps.SessionIndex,
		})
		p.mgr.AddSharedFacet(facetBot)
	}

	if pool := p.mgr.SharedPool(); pool != nil && pool.Size() > 0 {
		sessionTTL, _ := time.ParseDuration(tgPlat.FacetSessionTTL)
		if sessionTTL > 0 {
			pool.SetSessionTTL(sessionTTL, p.deps.Sessions)
		}
		if params.ReclaimHook != nil {
			pool.ReclaimHook = params.ReclaimHook
		}
		log.Infof("telegram", "%d shared facet bots ready", pool.Size())
	}
}

func (p *telegramProvider) RestoreFacetSessions(params platform.RestoreParams) {
	if p.deps.SessionIndex == nil {
		return
	}
	restoreFacetSessions(p.mgr, p.deps.SessionIndex, p.deps.Sessions, p.deps.Config, params)
}

func (p *telegramProvider) SetLifecycleCallback(agentID string, event platform.LifecycleEvent, fn func()) {
	bot := p.mgr.PrimaryBot(agentID)
	if bot == nil {
		return
	}
	switch event {
	case platform.OnUserMessage:
		bot.OnUserMessage = fn
	case platform.OnTurnComplete:
		bot.OnTurnComplete = fn
	case platform.OnTurnEnd:
		bot.OnTurnEnd = fn
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
		NotifyConfig: config.NotifyConfig{
			StartupNotify: &sn,
		},
		DisplayConfig: config.DisplayConfig{
			ShowToolCalls:  &off,
			ShowThinking:   &thinkOff,
			StreamOutput:   &so,
			StreamInterval: config.Ptr[string]("250ms"),
			DisplayWidth:   &dw,
		},
		AccessConfig: config.AccessConfig{
			RequireMention: &rm,
		},
		FacetSessionTTL:  "60m",
		MessageQueueSize: 64,
		Telegram: &config.TelegramSpecific{
			LongPollTimeout: "65s",
			TableWrapLines:  &twl,
			TableStyle:      &ts,
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
		log.Errorf("telegram", "load facet sessions: %v", err)
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
			username := bot.Username()
			if username == "" {
				return
			}
			savedKey, ok := facetMap["facet:"+username]
			if !ok || savedKey == "" {
				return
			}

			if sessions.LastActivity(savedKey) == "n/a" {
				log.Infof("telegram", "facet restore: @%s session %s no longer exists, cleaning up", username, savedKey)
				_ = idx.DeleteAgentMetadata("_system", "facet:"+username)
				return
			}

			bot.SetSessionKeyDirect(savedKey)

			agentID := extractAgentID(savedKey)
			if handler, commands, commandContext, acfg, ok := params.Resolver(agentID); ok {
				cmds, _ := commands.(*command.Registry)
				bot.SetHandlerAndCommands(handler, cmds)
				if cc, ok := commandContext.(command.CommandContext); ok {
					bot.SetCommandContext(cc)
				}
				ApplyAgentDisplaySettings(bot, acfg, cfg)
			}

			if agentID != "" {
				if primary := mgr.PrimaryBot(agentID); primary != nil {
					if chatID := primary.ChatID(); chatID != 0 {
						bot.SetChatID(chatID)
					}
				}
			}

			restored++
			log.Infof("telegram", "facet restore: @%s → %s", username, savedKey)
		})
	}
	if restored > 0 {
		log.Infof("telegram", "restored %d facet session(s) from state", restored)
	}
}

// Compile-time check.
var _ platform.MessagingProvider = (*telegramProvider)(nil)
