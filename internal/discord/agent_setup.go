package discord

import (
	"context"
	"fmt"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/tooldetail"
	"foci/internal/voice"

	"github.com/bwmarrin/discordgo"
)

// AgentSetupParams holds all dependencies needed to set up Discord bots for an agent.
type AgentSetupParams struct {
	Agent          platform.MessageHandler
	Commands       *command.Registry
	CommandContext command.CommandContext
	LastMsgStore   *command.LastMessageStore
	AgentConfig    config.AgentConfig
	GlobalConfig   *config.Config
	SecretStore    *secrets.Store
	Sessions       *session.Store
	SessionIndex   *session.SessionIndex
	ToolDetailStore *tooldetail.Store
	STT            voice.STT
	TTS            voice.TTS
	STTMap         map[string]voice.STT
	TTSMap         map[string]voice.TTS
	Ctx            context.Context //nolint:containedctx

	// ReclaimHook is called when a facet session is reclaimed.
	ReclaimHook func(sessionKey string)

	// DisplayOverrideFn returns per-session display overrides.
	DisplayOverrideFn func(sessionKey string) platform.DisplaySettings

	// ResolveTTS resolves the TTS provider for a given agent config.
	ResolveTTS func(ttsMap map[string]voice.TTS, ttsEntries []config.TTSConfig, ttsID string, rate float64, replacements map[string]string) voice.TTS

	// ResolveSTT resolves the STT provider for a given agent config.
	ResolveSTT func(sttMap map[string]voice.STT, sttEntries []config.STTConfig, agentSTT string, replacements map[string]string) voice.STT
}

// SetupAgent creates and registers Discord bots for an agent.
// Returns the result containing a DefaultSessionKeyFn, or nil if no platform was configured.
func SetupAgent(mgr *BotManager, p AgentSetupParams) *platform.SetupResult {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	setupDiscordBots(mgr, p)

	// Return result with default session key function wired to the primary bot.
	bot := mgr.PrimaryBot(acfg.ID)
	if bot == nil {
		return nil
	}
	return &platform.SetupResult{
		DefaultSessionKeyFn: bot.DefaultSessionKey,
		ConfigureFacetConn: func(conn platform.Connection) {
			dBot, ok := conn.(*Bot)
			if !ok {
				return
			}
			dBot.SetHandlerAndCommands(p.Agent, p.Commands)
			dBot.SetCommandContext(p.CommandContext)
			ApplyAgentDisplaySettings(dBot, acfg, cfg)
		},
		DisplayDefaultsFn: func() platform.DisplaySettings {
			soStr := "off"
			if bot.StreamOutputDefault() {
				soStr = "on"
			}
			dwStr := ""
			if dw := bot.DisplayWidthDefault(); dw > 0 {
				dwStr = fmt.Sprintf("%d", dw)
			}
			return platform.DisplaySettings{
				ShowToolCalls: bot.ShowToolCallsDefault(),
				ShowThinking:  bot.ShowThinkingDefault(),
				StreamOutput:  soStr,
				DisplayWidth:  dwStr,
			}
		},
	}
}

// resolveDiscordAllowedUsers returns the effective allowed user list for an agent.
// Priority: per-agent platform config > global.
func resolveDiscordAllowedUsers(acfg config.AgentConfig, cfg *config.Config) []string {
	dc := acfg.GetDiscordPlatform()
	if dc != nil && len(dc.AllowedUsers) > 0 {
		return dc.AllowedUsers
	}
	return cfg.Discord.AllowedUsers
}

// setupDiscordBots creates and registers Discord bots for an agent.
func setupDiscordBots(mgr *BotManager, p AgentSetupParams) {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	dc := acfg.GetDiscordPlatform()
	if dc == nil || dc.Bot == "" {
		return
	}
	botName := dc.Bot
	botSecret := dc.BotSecret

	discordToken := config.ResolveDiscordToken(botName, botSecret, p.SecretStore)
	if discordToken == "" {
		return
	}

	// Create a discordgo session
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Errorf("discord", "agent %q: create session: %v (agent will run without discord)", acfg.ID, err)
		return
	}

	// Configure intents
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsGuildMessageReactions

	// Open the websocket connection
	if err := dg.Open(); err != nil {
		log.Errorf("discord", "agent %q: open gateway: %v (agent will run without discord)", acfg.ID, err)
		return
	}

	allowedUsers := resolveDiscordAllowedUsers(acfg, cfg)
	primaryBot := NewBot(dg, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, acfg.ID)

	// Set bot user ID from the session
	if dg.State != nil && dg.State.User != nil {
		primaryBot.botUserID = dg.State.User.ID
	}

	// Apply Discord-specific settings
	if dc.GuildID != "" {
		primaryBot.guildID = dc.GuildID
	} else if cfg.Discord.GuildID != "" {
		primaryBot.guildID = cfg.Discord.GuildID
	}

	requireMention := cfg.Discord.RequireMention
	if dc.RequireMention != nil {
		requireMention = *dc.RequireMention
	}
	primaryBot.requireMention = requireMention

	autoThread := cfg.Discord.AutoThread
	if dc.AutoThread != nil {
		autoThread = *dc.AutoThread
	}
	primaryBot.autoThread = autoThread

	primaryBot.SetCommandContext(p.CommandContext)

	if p.SessionIndex != nil {
		primaryBot.SetSessionIndex(p.SessionIndex)
	}
	if p.ToolDetailStore != nil {
		primaryBot.SetToolDetailStore(p.ToolDetailStore)
	}

	if p.STT != nil {
		primaryBot.SetTranscriber(p.STT)
	}
	if p.TTS != nil {
		primaryBot.SetTTS(p.TTS)
	}
	primaryBot.display.ToolCallPreviewChars = cfg.Tools.ToolCallPreviewChars
	ApplyAgentDisplaySettings(primaryBot, acfg, cfg)

	if p.DisplayOverrideFn != nil {
		overrideFn := p.DisplayOverrideFn
		primaryBot.SetDisplayOverrideFn(func(sessionKey string) DisplayOverrides {
			ds := overrideFn(sessionKey)
			var dwi int
			if ds.DisplayWidth != "" {
				_, _ = fmt.Sscanf(ds.DisplayWidth, "%d", &dwi)
			}
			return DisplayOverrides{
				ShowToolCalls: ds.ShowToolCalls,
				ShowThinking:  ds.ShowThinking,
				StreamOutput:  ds.StreamOutput,
				DisplayWidth:  dwi,
			}
		})
	}

	mgr.AddPrimary(acfg.ID, primaryBot)

	// Configure session TTL for per-agent facet pool
	if pool := mgr.Pool(acfg.ID); pool != nil {
		ttl, _ := time.ParseDuration(cfg.Discord.FacetSessionTTL)
		if ttl > 0 {
			pool.SetSessionTTL(ttl, p.Sessions)
			log.Infof("discord", "agent %q: facet session TTL = %v", acfg.ID, ttl)
		}
		if p.ReclaimHook != nil {
			pool.ReclaimHook = p.ReclaimHook
		}
	}
}

// ApplyAgentDisplaySettings sets per-agent display settings on a bot,
// falling back to global config when the agent field is nil/empty.
func ApplyAgentDisplaySettings(bot *Bot, acfg config.AgentConfig, cfg *config.Config) {
	dc := acfg.GetDiscordPlatform()
	d := bot.display // start from current (preserves ToolCallPreviewChars set earlier)

	switch {
	case dc != nil && dc.ShowToolCalls != nil:
		d.ShowToolCalls = string(*dc.ShowToolCalls)
	case acfg.ShowToolCalls != nil:
		d.ShowToolCalls = string(*acfg.ShowToolCalls)
	case cfg.Discord.ShowToolCalls != nil:
		d.ShowToolCalls = string(*cfg.Discord.ShowToolCalls)
	}
	switch {
	case dc != nil && dc.ShowThinking != nil:
		d.ShowThinking = string(*dc.ShowThinking)
	case acfg.ShowThinking != nil:
		d.ShowThinking = string(*acfg.ShowThinking)
	case cfg.Discord.ShowThinking != nil:
		d.ShowThinking = string(*cfg.Discord.ShowThinking)
	}
	switch {
	case dc != nil && dc.DisplayWidth != nil:
		d.DisplayWidth = *dc.DisplayWidth
	case cfg.Discord.DisplayWidth != nil:
		d.DisplayWidth = *cfg.Discord.DisplayWidth
	}
	if acfg.MessagesInLog != nil {
		d.MessagesInLog = *acfg.MessagesInLog
	} else {
		d.MessagesInLog = cfg.Logging.MessagesInLog
	}
	switch {
	case dc != nil && dc.ReceivedFilesDir != "":
		d.ReceivedFilesDir = dc.ReceivedFilesDir
	case cfg.Discord.ReceivedFilesDir != "":
		d.ReceivedFilesDir = cfg.Discord.ReceivedFilesDir
	}
	if acfg.InjectedMessageHeader != "" {
		d.InjectedMessageHeader = acfg.InjectedMessageHeader
	} else {
		d.InjectedMessageHeader = cfg.Defaults.InjectedMessageHeader
	}
	d.SteerMode = acfg.SteerMode
	switch {
	case dc != nil && dc.StreamOutput != nil:
		d.StreamOutput = *dc.StreamOutput
	default:
		d.StreamOutput = cfg.Discord.StreamOutput
	}
	streamInterval := ""
	if dc != nil && dc.StreamInterval != "" {
		streamInterval = dc.StreamInterval
	} else {
		streamInterval = cfg.Discord.StreamUpdateInterval
	}
	if dur, err := time.ParseDuration(streamInterval); err == nil && dur > 0 {
		d.StreamUpdateInterval = dur
	}

	bot.display = d
}

// extractAgentID extracts the agent ID from a session key string.
func extractAgentID(sessionKey string) string {
	sk, err := session.ParseSessionKey(sessionKey)
	if err != nil {
		return ""
	}
	return sk.AgentID
}
