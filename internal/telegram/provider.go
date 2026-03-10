package telegram

import (
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/state"
)

// telegramProvider implements platform.MessagingProvider for Telegram.
type telegramProvider struct {
	mgr            *BotManager
	connMgr        *ConnectionManagerAdapter
	toolDetailStore *ToolDetailStore
	deps           platform.ProviderDeps
}

func (p *telegramProvider) Name() string { return "telegram" }

func (p *telegramProvider) IsConfigured(cfg *config.Config) bool {
	return len(cfg.Telegram.AllowedUsers) > 0
}

func (p *telegramProvider) Init(deps platform.ProviderDeps) error {
	p.mgr = NewBotManager()
	p.connMgr = &ConnectionManagerAdapter{BotManager: p.mgr}
	p.deps = deps

	// Create tool detail store
	dbPath := deps.Config.DataPath("tool_details.db")
	store, err := NewToolDetailStore(dbPath)
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
	lastMsgStore, _ := params.LastMsgStore.(*command.LastMessageStore)

	return SetupAgent(p.mgr, AgentSetupParams{
		Agent:           params.Handler,
		Commands:        cmds,
		LastMsgStore:    lastMsgStore,
		AgentConfig:     params.AgentConfig,
		GlobalConfig:    p.deps.Config,
		SecretStore:     p.deps.SecretStore,
		Sessions:        p.deps.Sessions,
		StateStore:      p.deps.StateStore,
		SessionIndex:    p.deps.SessionIndex,
		ToolDetailStore: p.toolDetailStore,
		STT:             params.STT,
		TTS:             params.TTS,
		STTMap:          p.deps.STTMap,
		TTSMap:          p.deps.TTSMap,
		Ctx:             p.deps.Ctx,
		ResolveTTS:      p.deps.ResolveTTS,
		ResolveSTT:      p.deps.ResolveSTT,
		ReclaimHook:     params.ReclaimHook,
	})
}

func (p *telegramProvider) SetupSharedMultiball(params platform.SharedMultiballParams) {
	cfg := p.deps.Config
	if len(cfg.Telegram.MultiballBots) == 0 || len(params.AgentOrder) == 0 {
		return
	}

	cmds, _ := params.FirstCommands.(*command.Registry)
	firstACfg := params.FirstAgentConfig
	sharedSTT := p.deps.ResolveSTT(p.deps.STTMap, firstACfg.STT)
	sharedTTS := p.deps.ResolveTTS(p.deps.TTSMap, cfg.TTS, firstACfg.TTS, firstACfg.TTSRate)

	for _, botName := range cfg.Telegram.MultiballBots {
		mbToken := config.ResolveBotToken(botName, "", p.deps.SecretStore)
		if mbToken == "" {
			log.Errorf("telegram", "shared multiball bot %q: token not found", botName)
			continue
		}
		mbBot, err := NewBot(mbToken, cfg.Telegram.AllowedUsers,
			params.FirstHandler, cmds, command.NewLastMessageStore(), "")
		if err != nil {
			log.Errorf("telegram", "shared multiball bot %q: create: %v", botName, err)
			continue
		}
		ConfigureMultiballBot(mbBot, MultiballBotConfig{
			STTProvider:     sharedSTT,
			TTSProvider:     sharedTTS,
			StopAliases:     cfg.Telegram.StopAliases,
			EnableStopAlias: cfg.Telegram.EnableStopAliases,
			AgentConfig:     firstACfg,
			GlobalConfig:    cfg,
			ToolDetailStore: p.toolDetailStore,
			StateStore:      p.deps.StateStore,
		})
		p.mgr.AddSharedMultiball(mbBot)
	}

	if pool := p.mgr.SharedPool(); pool != nil && pool.Size() > 0 {
		sessionTTL, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
		if sessionTTL > 0 {
			pool.SetSessionTTL(sessionTTL, p.deps.Sessions)
		}
		if params.ReclaimHook != nil {
			pool.ReclaimHook = params.ReclaimHook
		}
		log.Infof("telegram", "%d shared multiball bots ready", pool.Size())
	}
}

func (p *telegramProvider) RestoreMultiballSessions(params platform.RestoreParams) {
	if p.deps.StateStore == nil {
		return
	}
	restoreMultiballSessions(p.mgr, p.deps.StateStore, p.deps.Sessions, p.deps.Config, params)
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
	}
}

func (p *telegramProvider) ToolDetailStore() platform.ToolDetailStore {
	if p.toolDetailStore == nil {
		return nil
	}
	return p.toolDetailStore
}

func (p *telegramProvider) Close() error {
	if p.toolDetailStore != nil {
		p.toolDetailStore.ExpireAndVacuum()
		return p.toolDetailStore.Close()
	}
	return nil
}

// restoreMultiballSessions restores persisted multiball session mappings after restart.
func restoreMultiballSessions(
	mgr *BotManager,
	stateStore *state.Store,
	sessions *session.Store,
	cfg *config.Config,
	params platform.RestoreParams,
) {
	type poolInfo struct {
		pool *Pool
		name string
	}
	var pools []poolInfo
	for _, id := range params.AgentOrder {
		if pool := mgr.Pool(id); pool != nil {
			pools = append(pools, poolInfo{pool: pool, name: "agent:" + id})
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
			var savedKey string
			if !stateStore.Get("multiball:"+username, &savedKey) || savedKey == "" {
				return
			}

			if sessions.LastActivity(savedKey) == "n/a" {
				log.Infof("telegram", "multiball restore: @%s session %s no longer exists, cleaning up", username, savedKey)
				_ = stateStore.Delete("multiball:" + username)
				return
			}

			bot.SetSessionKeyDirect(savedKey)

			agentID := extractAgentID(savedKey)
			if handler, commands, acfg, ok := params.Resolver(agentID); ok {
				cmds, _ := commands.(*command.Registry)
				bot.SetHandlerAndCommands(handler, cmds)
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
			log.Infof("telegram", "multiball restore: @%s → %s", username, savedKey)
		})
	}
	if restored > 0 {
		log.Infof("telegram", "restored %d multiball session(s) from state", restored)
	}
}

// Compile-time check.
var _ platform.MessagingProvider = (*telegramProvider)(nil)
