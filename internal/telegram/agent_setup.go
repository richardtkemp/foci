package telegram

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
)

// AgentSetupParams holds all dependencies needed to set up platform bots for an agent.
type AgentSetupParams struct {
	Agent          platform.MessageHandler
	Commands       *command.Registry
	CommandContext command.CommandContext
	LastMsgStore   *command.LastMessageStore
	AgentConfig    config.AgentConfig
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
}

// SetupAgent creates and registers platform bots for an agent.
// Returns the result containing a DefaultSessionKeyFn, or nil if no platform was configured.
//
// Note: notification callback wiring (CacheBustAlert, ManaWarnFunc, etc.) and
// ag.AddPlatform are handled by the caller (agents.go wireAgentPlatformCallbacks),
// not here — this keeps the platform layer decoupled from agent internals.
func SetupAgent(mgr *BotManager, p AgentSetupParams) *platform.SetupResult {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

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
			ApplyAgentDisplaySettings(tBot, acfg, cfg)
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
	primaryBot, err := NewBot(telegramToken, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, acfg.ID)
	if err != nil {
		log.Errorf("telegram", "agent %q: create bot: %v (agent will run without platform)", acfg.ID, err)
		return
	}

	// Resolve require_mention: per-agent platform > global platform (default true).
	reqMention := true
	if tg.Access.RequireMention != nil {
		reqMention = *tg.Access.RequireMention
	}
	primaryBot.requireMention = reqMention

	// Resolve behavior config via Merge cascade.
	bc := config.Merge(acfg.Defaults.Behavior, cfg.Defaults.Behavior)
	throttleStr := config.DerefStr(bc.GroupThrottle)
	if dur, err := time.ParseDuration(throttleStr); err == nil && dur > 0 {
		gt := platform.NewGroupThrottle(dur, func(msgs []platform.QueuedMessage) {
			for _, m := range msgs {
				primaryBot.mq.PushFlushed(m)
			}
		}, primaryBot.log)
		primaryBot.mq.SetThrottle(gt)
		log.Infof("telegram", "agent %q: group throttle = %v", acfg.ID, dur)
	}
	primaryBot.mq.SetRequireMention(reqMention)
	steerMode := bc.SteerMode == nil || *bc.SteerMode // default true
	primaryBot.mq.SetSteerMode(steerMode)

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

	// Per-agent facet bots
	facetBots := tg.FacetBots
	for _, facetName := range facetBots {
		facetToken := config.ResolveBotToken(facetName, "", p.SecretStore)
		if facetToken == "" {
			log.Errorf("telegram", "agent %q: facet bot %q: token not found", acfg.ID, facetName)
			continue
		}
		facetBot, err := NewBot(facetToken, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, "")
		if err != nil {
			log.Errorf("telegram", "agent %q: create facet bot %q: %v", acfg.ID, facetName, err)
			continue
		}
		ConfigureFacetBot(facetBot, FacetBotConfig{
			STTProvider:     p.ResolveSTT(p.STTMap, cfg.STT, config.DerefStr(acfg.Defaults.Voice.STT), voice.MergeReplacements(cfg.Defaults.Voice.STTReplacements, acfg.Defaults.Voice.STTReplacements)),
			TTSProvider:     p.ResolveTTS(p.TTSMap, cfg.TTS, config.DerefStr(acfg.Defaults.Voice.TTS), config.DerefFloat(acfg.Defaults.Voice.TTSRate), voice.MergeReplacements(cfg.Defaults.Voice.TTSReplacements, acfg.Defaults.Voice.TTSReplacements)),
			AgentConfig:     acfg,
			GlobalConfig:    cfg,
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

// FacetBotConfig holds common settings applied to every facet bot.
type FacetBotConfig struct {
	STTProvider     voice.STT
	TTSProvider     voice.TTS
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
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
	ApplyAgentDisplaySettings(bot, mc.AgentConfig, mc.GlobalConfig)
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

// ApplyAgentDisplaySettings sets per-agent display settings on a bot,
// resolving the full 4-level cascade for DisplayConfig via Merge.
func ApplyAgentDisplaySettings(bot *Bot, acfg config.AgentConfig, cfg *config.Config) {
	tg := acfg.Platform("telegram")
	dc := config.Merge(
		tg.SafeDisplay(),
		acfg.Defaults.Display,
		cfg.Platform("telegram").SafeDisplay(),
		cfg.Defaults.Display,
	)
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

	// Telegram-specific fields (not in DisplayConfig)
	if tg != nil && tg.Telegram != nil {
		if tg.Telegram.TableWrapLines != nil {
			d.TableWrapLines = *tg.Telegram.TableWrapLines
		}
		if tg.Telegram.TableStyle != nil {
			d.TableStyle = *tg.Telegram.TableStyle
		}
	}

	// MessagesInLog is not in DisplayConfig — resolve via Merge.
	dbg := config.Merge(acfg.Debug, cfg.Debug)
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
