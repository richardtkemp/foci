package telegram

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
)

// AgentSetupParams holds all dependencies needed to set up platform bots for an agent.
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
	// See platform.AgentConnectionParams for details.
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

// SetupAgent creates and registers platform bots for an agent.
// Returns the result containing a DefaultSessionKeyFn, or nil if no platform was configured.
//
// Note: notification callback wiring (CacheBustAlert, ManaWarnFunc, etc.) and
// ag.AddPlatform are handled by the caller (agents.go wireAgentPlatformCallbacks),
// not here — this keeps the platform layer decoupled from agent internals.
func SetupAgent(mgr *BotManager, p AgentSetupParams) *platform.SetupResult {
	acfg := p.AgentConfig

	setupTelegramBots(mgr, p)

	// Return result with default session key function wired to the primary bot.
	bot := mgr.PrimaryBot(acfg.ID)
	if bot == nil {
		return nil
	}
	return &platform.SetupResult{
		DefaultSessionKeyFn: bot.DefaultSessionKey,
		ConfigureFacetConn: func(conn platform.Connection) {
			tBot, ok := conn.(*Bot)
			if !ok {
				return
			}
			tBot.SetHandlerAndCommands(p.Agent, p.Commands)
			tBot.SetCommandContext(p.CommandContext)
			// bot.display is a per-bot baked fallback layer; the live
			// per-session override path is DisplayOverrideFn/session_meta.go.
			// Converting this fallback itself needs its own dedicated pass.
			ApplyAgentDisplaySettings(tBot, p.Resolved.PlatformDisplay("telegram"), p.Resolved.Debug, acfg.Platform("telegram")) // static-cfg:ignore: see comment above
			tBot.fileMode, _ = config.ParseFileMode(p.GlobalConfig.FileMode)
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

// resolveAllowedUsers merges per-agent and global allowed users for Telegram.
// Agent users are added to global users (deduplicated).
func resolveAllowedUsers(acfg config.AgentConfig, cfg *config.Config) []string {
	var agentUsers, globalUsers []string
	if p := acfg.Platform("telegram"); p != nil {
		agentUsers = p.Access.AllowedUsers
	}
	if gp := cfg.Platform("telegram"); gp != nil {
		globalUsers = gp.Access.AllowedUsers
	}
	return config.SuperveneSlice(agentUsers, globalUsers, func(s string) string { return s })
}

// resolveAllowedUsersOnly resolves access.allowed_users_only for Telegram:
// per-agent platform flag, else global platform flag, else true (strict).
func resolveAllowedUsersOnly(acfg config.AgentConfig, cfg *config.Config) bool {
	if p := acfg.Platform("telegram"); p != nil && p.Access.AllowedUsersOnly != nil {
		return *p.Access.AllowedUsersOnly
	}
	if gp := cfg.Platform("telegram"); gp != nil && gp.Access.AllowedUsersOnly != nil {
		return *gp.Access.AllowedUsersOnly
	}
	return true
}

// setupTelegramBots creates and registers Telegram bots for an agent.
func setupTelegramBots(mgr *BotManager, p AgentSetupParams) {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	tg := acfg.Platform("telegram")
	if tg == nil || tg.Bot == "" {
		return
	}
	botName := tg.Bot
	botSecret := tg.BotSecret

	telegramToken := config.ResolveBotToken(botName, botSecret, p.SecretStore)
	if telegramToken == "" {
		return
	}

	allowedUsers := resolveAllowedUsers(acfg, cfg)
	allowedOnly := resolveAllowedUsersOnly(acfg, cfg)
	primaryBot, err := NewBot(telegramToken, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, acfg.ID,
		telegramAPIBaseOf(tg))
	if err != nil {
		log.Errorf("telegram", "agent %q: create bot: %v (agent will run without platform)", acfg.ID, err)
		return
	}
	primaryBot.SetAllowedUsersOnly(allowedOnly)

	// Resolve require_mention: per-agent platform > global platform (default true).
	reqMention := true
	if tg.Access.RequireMention != nil {
		reqMention = *tg.Access.RequireMention
	}
	primaryBot.requireMention = reqMention

	// Resolve behavior config from pre-merged config.
	bc := p.Resolved.Behavior // static-cfg:ignore: initial construction value; group_throttle live-updates via the OnChange below, steer_mode via LiveConfigFn (bucket D)
	if gt := newGroupThrottle(bc.GroupThrottle, primaryBot); gt != nil {
		primaryBot.mq.SetThrottle(gt)
		log.Infof("telegram", "agent %q: group throttle = %s", acfg.ID, bc.GroupThrottle)
	}
	primaryBot.mq.SetRequireMention(reqMention)

	if p.ResolvedLive != nil {
		p.ResolvedLive.OnChange(func(old, fresh *config.ResolvedAgentConfig) {
			if fresh.Behavior.GroupThrottle == old.Behavior.GroupThrottle {
				return
			}
			if oldThrottle := primaryBot.mq.GetThrottle(); oldThrottle != nil {
				oldThrottle.Stop()
			}
			primaryBot.mq.SetThrottle(newGroupThrottle(fresh.Behavior.GroupThrottle, primaryBot))
			log.Infof("telegram", "agent %q: group throttle live-updated to %q", acfg.ID, fresh.Behavior.GroupThrottle)
		})
	}

	// Wire the bot to the agent's Inbox subsystem (Phase 6 — TODO #739).
	// The agent owns the per-session message queue, steer buffer,
	// in-flight flag, and worker goroutines. The bot's pump drains the
	// platform queue and calls a.Enqueue; each session's worker calls
	// back into Bot.Drive for renderer/sink construction.
	//
	// Steer authority now lives entirely on the agent (a.SetInboxSteerMode);
	// MessageQueue's filter logic deals only with platform-side concerns
	// (require_mention, throttle).
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
	primaryBot.display.ToolCallPreviewChars = cfg.Tools.ToolCallPreviewChars
	ApplyAgentDisplaySettings(primaryBot, p.Resolved.PlatformDisplay("telegram"), p.Resolved.Debug, acfg.Platform("telegram")) // static-cfg:ignore: see comment on the ConfigureFacetConn call above
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

	// Per-agent facet bots
	facetBots := tg.FacetBots
	for _, facetName := range facetBots {
		facetToken := config.ResolveBotToken(facetName, "", p.SecretStore)
		if facetToken == "" {
			log.Errorf("telegram", "agent %q: facet bot %q: token not found", acfg.ID, facetName)
			continue
		}
		facetBot, err := NewBot(facetToken, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, "",
			telegramAPIBaseOf(tg))
		if err != nil {
			log.Errorf("telegram", "agent %q: create facet bot %q: %v", acfg.ID, facetName, err)
			continue
		}
		facetBot.SetAllowedUsersOnly(allowedOnly)
		ConfigureFacetBot(facetBot, FacetBotConfig{
			STTProvider:     p.ResolveSTT(p.STTMap, cfg.STT, config.DerefStr(acfg.Voice.STT), voice.MergeReplacements(cfg.Voice.STTReplacements, acfg.Voice.STTReplacements)),
			TTSProvider:     p.ResolveTTS(p.TTSMap, cfg.TTS, config.DerefStr(acfg.Voice.TTS), config.DerefFloat(acfg.Voice.TTSRate), voice.MergeReplacements(cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)),
			AgentConfig:     acfg,
			GlobalConfig:    cfg,
			Resolved:        p.Resolved, // static-cfg:ignore: plumbing — see comment on the ConfigureFacetConn call above
			ToolDetailStore: p.ToolDetailStore,
			SessionIndex:    p.SessionIndex,
		})
		mgr.AddFacet(acfg.ID, facetBot)
	}
	if pool := mgr.Pool(acfg.ID); pool != nil && pool.Size() > 0 {
		log.Infof("telegram", "agent %q: %d per-agent facet bots ready", acfg.ID, pool.Size())
	}

	// Configure session TTL for per-agent facet pool
	if pool := mgr.Pool(acfg.ID); pool != nil {
		ttl, _ := time.ParseDuration(tg.FacetSessionTTL)
		if ttl > 0 {
			pool.SetSessionTTL(ttl, p.Sessions)
			log.Infof("telegram", "agent %q: facet session TTL = %v", acfg.ID, ttl)
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

// FacetBotConfig holds common settings applied to every facet bot.
type FacetBotConfig struct {
	STTProvider     voice.STT
	TTSProvider     voice.TTS
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
	Resolved        *config.ResolvedAgentConfig
	ToolDetailStore *tooldetail.Store
	SessionIndex    *session.SessionIndex
}

// ConfigureFacetBot applies the standard facet bot settings.
func ConfigureFacetBot(bot *Bot, mc FacetBotConfig) {
	if mc.STTProvider != nil {
		bot.SetTranscriber(mc.STTProvider)
	}
	if mc.TTSProvider != nil {
		bot.SetTTS(mc.TTSProvider)
	}
	ApplyAgentDisplaySettings(bot, mc.Resolved.PlatformDisplay("telegram"), mc.Resolved.Debug, mc.AgentConfig.Platform("telegram")) // static-cfg:ignore: see comment on the ConfigureFacetConn call in SetupAgent
	bot.fileMode, _ = config.ParseFileMode(mc.GlobalConfig.FileMode)
	if mc.ToolDetailStore != nil {
		bot.SetToolDetailStore(mc.ToolDetailStore)
	}
	if mc.SessionIndex != nil {
		idx := mc.SessionIndex
		bot.OnSessionKeyChange = func(username, sessionKey string) {
			key := "facet:" + username
			if sessionKey == "" {
				_ = idx.DeleteAgentMetadata("_system", key)
			} else {
				_ = idx.SetAgentMetadata("_system", key, sessionKey)
			}
		}
	}
}

// ApplyAgentDisplaySettings sets per-agent display settings on a bot
// using pre-resolved config values.
func ApplyAgentDisplaySettings(bot *Bot, dc config.ResolvedDisplay, dbg config.ResolvedDebug, tg *config.PlatformConfig) {
	d := bot.display // start from current (preserves ToolCallPreviewChars set earlier)

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
	if dc.StreamOutput {
		d.StreamOutput = true
	}
	if dc.StreamInterval != "" {
		if dur, err := time.ParseDuration(dc.StreamInterval); err == nil && dur > 0 {
			d.StreamUpdateInterval = dur
		}
	}
	if dc.InjectedMessageHeader != "" {
		d.InjectedMessageHeader = dc.InjectedMessageHeader
	}

	// Telegram-specific fields (not in DisplayConfig)
	if tg != nil && tg.Telegram != nil {
		if tg.Telegram.TableWrapLines != nil {
			d.TableWrapLines = *tg.Telegram.TableWrapLines
		}
		if tg.Telegram.TableStyle != nil {
			d.TableStyle = *tg.Telegram.TableStyle
		}
		if tg.Telegram.LongPollTimeout != "" {
			if dur, err := time.ParseDuration(tg.Telegram.LongPollTimeout); err == nil && dur > 0 {
				bot.longPollTimeout = dur
			}
		}
	}

	d.MessagesInLog = dbg.MessagesInLog

	bot.display = d
}
