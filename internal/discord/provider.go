package discord

import (
	"fmt"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tooldetail"
)

// discordProvider implements platform.MessagingProvider for Discord.
type discordProvider struct {
	mgr             *BotManager
	connMgr         *ConnectionManagerAdapter
	toolDetailStore *tooldetail.Store
	deps            platform.ProviderDeps
}

func (p *discordProvider) Name() string { return "discord" }

func (p *discordProvider) IsConfigured(cfg *config.Config) (bool, string) {
	dc := cfg.Platform("discord")
	if dc == nil {
		return false, "no [[platforms]] entry with id=\"discord\""
	}
	if (dc.Access.AllowedUsersOnly == nil || *dc.Access.AllowedUsersOnly) && len(dc.Access.AllowedUsers) == 0 {
		return false, "access.allowed_users is empty (set allowed_users or set allowed_users_only=false)"
	}
	return true, ""
}

func (p *discordProvider) Init(deps platform.ProviderDeps) error {
	p.mgr = NewBotManager()
	p.connMgr = &ConnectionManagerAdapter{BotManager: p.mgr}
	p.deps = deps

	// Create tool detail store
	dbPath := deps.Config.DataPath("discord_tool_details.db")
	store, err := tooldetail.NewStore(dbPath)
	if err != nil {
		log.Errorf("discord", "create tool detail store: %v (inline button expansion will not persist)", err)
	} else {
		p.toolDetailStore = store
	}

	return nil
}

func (p *discordProvider) ConnectionManager() platform.ConnectionManager {
	return p.connMgr
}

func (p *discordProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
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
		Resolved:          params.Resolved,
	})
}

func (p *discordProvider) SetupSharedFacet(_ platform.SharedFacetParams) {
	// Discord uses threads for facets, not separate bots.
	// No-op for now.
}

func (p *discordProvider) RestoreFacetSessions(params platform.RestoreParams) {
	if p.deps.SessionIndex == nil {
		return
	}
	restoreFacetSessions(p.mgr, p.deps.SessionIndex, p.deps.Sessions, p.deps.Config, params)
}

func (p *discordProvider) SetLifecycleCallback(agentID string, event platform.LifecycleEvent, fn func()) {
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

func (p *discordProvider) ToolDetailStore() platform.ToolDetailStore {
	if p.toolDetailStore == nil {
		return nil
	}
	return p.toolDetailStore
}

func (p *discordProvider) AgentPreFlight(agentID string) []string {
	tokenSecret := "discord." + agentID
	if _, ok := p.deps.SecretStore.Get(tokenSecret); !ok {
		return []string{fmt.Sprintf(
			"Secret `%s` not found -- add it with `/secrets set %s <token>` before starting.",
			tokenSecret, tokenSecret,
		)}
	}
	return nil
}

func (p *discordProvider) DefaultPlatformConfig() config.PlatformConfig {
	off := config.ToolCallOff
	thinkOff := config.ShowThinkingOff
	dw := 60
	so := false
	rm := true
	sn := true
	at := true
	return config.PlatformConfig{
		ID: "discord",
		Notify: config.NotifyConfig{
			StartupNotify: &sn,
		},
		Display: config.DisplayConfig{
			ShowToolCalls:  &off,
			ShowThinking:   &thinkOff,
			StreamOutput:   &so,
			StreamInterval: config.Ptr[string]("1200ms"),
			DisplayWidth:   &dw,
		},
		Access: config.AccessConfig{
			RequireMention: &rm,
		},
		FacetSessionTTL:  "60m",
		MessageQueueSize: 64,
		Discord: &config.DiscordSpecific{
			AutoThread: &at,
		},
	}
}

func (p *discordProvider) ValidateConfig(_ config.PlatformConfig) []string {
	return nil
}

func (p *discordProvider) Close() error {
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
	// Load all discord_facet mappings at once
	facetMap, err := idx.AgentMetadataByPrefix("_system", "discord_facet:")
	if err != nil {
		log.Errorf("discord", "load facet sessions: %v", err)
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
			botID := bot.Username()
			if botID == "" {
				return
			}
			savedKey, ok := facetMap["discord_facet:"+botID]
			if !ok || savedKey == "" {
				return
			}

			if sessions.LastActivity(savedKey) == "n/a" {
				log.Infof("discord", "facet restore: %s session %s no longer exists, cleaning up", botID, savedKey)
				_ = idx.DeleteAgentMetadata("_system", "discord_facet:"+botID)
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
				rc := config.Resolve(cfg, acfg)
				ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("discord"), rc.Debug)
			}

			if agentID != "" {
				if primary := mgr.PrimaryBot(agentID); primary != nil {
					if channelID := primary.ChatID(); channelID != 0 {
						bot.SetChatID(channelID)
					}
				}
			}

			restored++
			log.Infof("discord", "facet restore: %s -> %s", botID, savedKey)
		})
	}
	if restored > 0 {
		log.Infof("discord", "restored %d facet session(s) from state", restored)
	}
}

// Compile-time check.
var _ platform.MessagingProvider = (*discordProvider)(nil)
