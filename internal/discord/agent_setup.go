package discord

import (
	"context"
	"fmt"
	"time"

	"foci/internal/agent"
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

var (
	discordLog = log.NewComponentLogger("discord")
)

// AgentSetupParams holds all dependencies needed to set up Discord bots for an agent.
type AgentSetupParams struct {
	Agent           platform.MessageHandler
	Commands        *command.Registry
	CommandContext  command.CommandContext
	LastMsgStore    *command.LastMessageStore
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
	SecretStore     *secrets.Store
	Sessions        *session.Store
	SessionIndex    *session.SessionIndex
	ToolDetailStore *tooldetail.Store
	STT             voice.STT
	TTS             voice.TTS
	STTMap          map[string]voice.STT
	TTSMap          map[string]voice.TTS
	Ctx             context.Context //nolint:containedctx

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

	// ResolvedLive lets config-derived handles (the group throttle) subscribe
	// to live config edits and rebuild themselves.
	ResolvedLive *config.LiveValue[*config.ResolvedAgentConfig]
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
			// bot.display is a per-bot baked fallback layer, overlaid by the
			// per-session override path (DisplayOverrideFn/session_meta.go).
			// The hot-tagged fields (stream_output, messages_in_log) also
			// live-update via the OnChange below.
			ApplyAgentDisplaySettings(dBot, p.Resolved.PlatformDisplay("discord"), p.Resolved.Debug) // static-cfg:ignore: see comment above
			dBot.fileMode, _ = config.ParseFileMode(p.GlobalConfig.FileMode)
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

// resolveDiscordAllowedUsersOnly resolves access.allowed_users_only for Discord:
// per-agent platform flag, else global platform flag, else true (strict).
func resolveDiscordAllowedUsersOnly(acfg config.AgentConfig, cfg *config.Config) bool {
	if p := acfg.Platform("discord"); p != nil && p.Access.AllowedUsersOnly != nil {
		return *p.Access.AllowedUsersOnly
	}
	if gp := cfg.Platform("discord"); gp != nil && gp.Access.AllowedUsersOnly != nil {
		return *gp.Access.AllowedUsersOnly
	}
	return true
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
		discordLog.Errorf("agent %q: create session: %v (agent will run without discord)", acfg.ID, err)
		return
	}

	// Configure intents
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsGuildMessageReactions

	// Open the websocket connection
	if err := dg.Open(); err != nil {
		discordLog.Errorf("agent %q: open gateway: %v (agent will run without discord)", acfg.ID, err)
		return
	}

	allowedUsers := resolveDiscordAllowedUsers(acfg, cfg)
	primaryBot := NewBot(dg, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, acfg.ID)
	primaryBot.SetAllowedUsersOnly(resolveDiscordAllowedUsersOnly(acfg, cfg))

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
	bc := p.Resolved.Behavior // static-cfg:ignore: initial construction value; group_throttle live-updates via the OnChange below, steer_mode via LiveConfigFn (bucket D)
	primaryBot.mq.SetRequireMention(primaryBot.requireMention)

	if gt := newGroupThrottle(bc.GroupThrottle, primaryBot); gt != nil {
		primaryBot.mq.SetThrottle(gt)
		discordLog.Infof("agent %q: group throttle = %s", acfg.ID, bc.GroupThrottle)
	}

	if p.ResolvedLive != nil {
		p.ResolvedLive.OnChange(func(old, fresh *config.ResolvedAgentConfig) {
			if fresh.Behavior.GroupThrottle == old.Behavior.GroupThrottle {
				return
			}
			if oldThrottle := primaryBot.mq.GetThrottle(); oldThrottle != nil {
				oldThrottle.Stop()
			}
			primaryBot.mq.SetThrottle(newGroupThrottle(fresh.Behavior.GroupThrottle, primaryBot))
			discordLog.Infof("agent %q: group throttle live-updated to %q", acfg.ID, fresh.Behavior.GroupThrottle)
		})

		// Hot-tagged display/debug fields (display.stream_output,
		// display.table_wrap_lines/table_style, debug.messages_in_log): re-run
		// ApplyAgentDisplaySettings with the fresh resolved values on any change.
		p.ResolvedLive.OnChange(func(old, fresh *config.ResolvedAgentConfig) {
			oldDC, freshDC := old.PlatformDisplay("discord"), fresh.PlatformDisplay("discord")
			if oldDC.StreamOutput == freshDC.StreamOutput &&
				oldDC.TableWrapLines == freshDC.TableWrapLines &&
				oldDC.TableStyle == freshDC.TableStyle &&
				old.Debug.MessagesInLog == fresh.Debug.MessagesInLog {
				return
			}
			ApplyAgentDisplaySettings(primaryBot, freshDC, fresh.Debug)
			discordLog.Infof("agent %q: display settings live-updated", acfg.ID)
		})
	}

	// Wire the bot to the agent's Inbox subsystem (Phase 6 — TODO #739).
	// See telegram/agent_setup.go for the architecture rationale. The
	// agent owns the per-session message queue, steer buffer, in-flight
	// flag, and worker goroutines. Steer authority lives entirely on the
	// agent (a.SetInboxSteerMode); MessageQueue handles only filter +
	// throttle.
	if a, ok := p.Agent.(*agent.Agent); ok && a != nil {
		primaryBot.agentRef = a
		a.SetInboxSteerMode(bc.SteerMode)
		a.StartInbox(p.Ctx)
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
	primaryBot.updateDisplay(func(d BotDisplayConfig) BotDisplayConfig {
		d.ToolCallPreviewChars = cfg.Tools.ToolCallPreviewChars
		return d
	})
	ApplyAgentDisplaySettings(primaryBot, p.Resolved.PlatformDisplay("discord"), p.Resolved.Debug) // static-cfg:ignore: see comment on the ConfigureFacetConn call above
	primaryBot.fileMode, _ = config.ParseFileMode(p.GlobalConfig.FileMode)

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
			discordLog.Infof("agent %q: facet session TTL = %v", acfg.ID, ttl)
		}
		if p.ReclaimHook != nil {
			pool.ReclaimHook = p.ReclaimHook
		}
	}
}

// newGroupThrottle parses durStr and returns a throttle that flushes into
// bot's message queue, or nil if durStr is empty/unparseable/non-positive
// (throttling disabled).
func newGroupThrottle(durStr string, bot *Bot) *platform.GroupThrottle {
	dur, err := time.ParseDuration(durStr)
	if err != nil || dur <= 0 {
		return nil
	}
	return platform.NewGroupThrottle(dur, func(msgs []platform.QueuedMessage) {
		for _, m := range msgs {
			bot.mq.PushFlushed(m)
		}
	}, bot.log)
}

// ApplyAgentDisplaySettings sets per-agent display settings on a bot using
// pre-resolved config values. Called once at bot setup, and again from the
// OnChange hook below whenever a hot-tagged field (stream_output,
// messages_in_log) changes live.
func ApplyAgentDisplaySettings(bot *Bot, dc config.ResolvedDisplay, dbg config.ResolvedDebug) {
	bot.updateDisplay(func(d BotDisplayConfig) BotDisplayConfig {
		if dc.ShowToolCalls != "" {
			d.ShowToolCalls = dc.ShowToolCalls
		}
		if dc.ShowThinking != "" {
			d.ShowThinking = dc.ShowThinking
		}
		if dc.DisplayWidth != 0 {
			d.DisplayWidth = dc.DisplayWidth
		}
		if dc.ReceivedFilesDir != "" {
			d.ReceivedFilesDir = dc.ReceivedFilesDir
		}
		// dc is always the fully-resolved cascade value (not a partial layer),
		// so this assigns unconditionally — needed for a live re-apply to be
		// able to turn a previously-true value back off.
		d.StreamOutput = dc.StreamOutput
		if dc.StreamInterval != "" {
			if dur, err := time.ParseDuration(dc.StreamInterval); err == nil && dur > 0 {
				d.StreamUpdateInterval = dur
			}
		}
		if dc.InjectedMessageHeader != "" {
			d.InjectedMessageHeader = dc.InjectedMessageHeader
		}
		if dc.TableWrapLines != 0 {
			d.TableWrapLines = dc.TableWrapLines
		}
		if dc.TableStyle != "" {
			d.TableStyle = dc.TableStyle
		}

		d.MessagesInLog = dbg.MessagesInLog
		return d
	})
}
