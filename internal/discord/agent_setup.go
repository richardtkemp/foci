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

	// Resolved holds the pre-merged agent+global config.
	Resolved *config.ResolvedAgentConfig
}

// SetupAgent creates and registers Discord bots for an agent.
// Returns the result containing a DefaultSessionKeyFn, or nil if no platform was configured.
func SetupAgent(mgr *BotManager, p AgentSetupParams) *platform.SetupResult {
	acfg := p.AgentConfig

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
			ApplyAgentDisplaySettings(dBot, p.Resolved.PlatformDisplay("discord"), p.Resolved.Debug)
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

// resolveDiscordAllowedUsers merges per-agent and global allowed users for Discord.
// Agent users are added to global users (deduplicated).
func resolveDiscordAllowedUsers(acfg config.AgentConfig, cfg *config.Config) []string {
	var agentUsers, globalUsers []string
	if p := acfg.Platform("discord"); p != nil {
		agentUsers = p.Access.AllowedUsers
	}
	if gp := cfg.Platform("discord"); gp != nil {
		globalUsers = gp.Access.AllowedUsers
	}
	return config.SuperveneSlice(agentUsers, globalUsers, func(s string) string { return s })
}

// setupDiscordBots creates and registers Discord bots for an agent.
func setupDiscordBots(mgr *BotManager, p AgentSetupParams) {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	dc := acfg.Platform("discord")
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

	// Apply Discord-specific settings from resolved platform config
	if dc.Discord != nil && dc.Discord.GuildID != "" {
		primaryBot.guildID = dc.Discord.GuildID
	}

	requireMention := true
	if dc.Access.RequireMention != nil {
		requireMention = *dc.Access.RequireMention
	}
	primaryBot.requireMention = requireMention

	autoThread := true
	if dc.Discord != nil && dc.Discord.AutoThread != nil {
		autoThread = *dc.Discord.AutoThread
	}
	primaryBot.autoThread = autoThread

	// Resolve behavior config from pre-merged config.
	bc := p.Resolved.Behavior
	primaryBot.mq.SetRequireMention(primaryBot.requireMention)
	steerMode := bc.SteerMode == nil || *bc.SteerMode // default true
	primaryBot.mq.SetSteerMode(steerMode)

	throttleStr := config.DerefStr(bc.GroupThrottle)
	if dur, err := time.ParseDuration(throttleStr); err == nil && dur > 0 {
		gt := platform.NewGroupThrottle(dur, func(msgs []platform.QueuedMessage) {
			for _, m := range msgs {
				primaryBot.mq.PushFlushed(m)
			}
		}, primaryBot.log)
		primaryBot.mq.SetThrottle(gt)
		log.Infof("discord", "agent %q: group throttle = %v", acfg.ID, dur)
	}

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
	ApplyAgentDisplaySettings(primaryBot, p.Resolved.PlatformDisplay("discord"), p.Resolved.Debug)

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
		ttl, _ := time.ParseDuration(dc.FacetSessionTTL)
		if ttl > 0 {
			pool.SetSessionTTL(ttl, p.Sessions)
			log.Infof("discord", "agent %q: facet session TTL = %v", acfg.ID, ttl)
		}
		if p.ReclaimHook != nil {
			pool.ReclaimHook = p.ReclaimHook
		}
	}
}

// ApplyAgentDisplaySettings sets per-agent display settings on a bot
// using pre-resolved config values.
func ApplyAgentDisplaySettings(bot *Bot, dc config.DisplayConfig, dbg config.DebugConfig) {
	d := bot.display // start from current (preserves ToolCallPreviewChars set earlier)

	if dc.ShowToolCalls != nil {
		d.ShowToolCalls = string(*dc.ShowToolCalls)
	}
	if dc.ShowThinking != nil {
		d.ShowThinking = string(*dc.ShowThinking)
	}
	if dc.DisplayWidth != nil {
		d.DisplayWidth = *dc.DisplayWidth
	}
	if dc.ReceivedFilesDir != nil && *dc.ReceivedFilesDir != "" {
		d.ReceivedFilesDir = *dc.ReceivedFilesDir
	}
	if dc.StreamOutput != nil {
		d.StreamOutput = *dc.StreamOutput
	}
	if dc.StreamInterval != nil {
		if dur, err := time.ParseDuration(*dc.StreamInterval); err == nil && dur > 0 {
			d.StreamUpdateInterval = dur
		}
	}
	if dc.InjectedMessageHeader != nil {
		d.InjectedMessageHeader = *dc.InjectedMessageHeader
	}

	d.MessagesInLog = config.DerefBool(dbg.MessagesInLog)

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
