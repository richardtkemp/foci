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
	ToolDetailStore *ToolDetailStore
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

// resolveAllowedUsers returns the effective allowed user list for an agent.
// Priority: per-agent platform config > global.
func resolveAllowedUsers(acfg config.AgentConfig, cfg *config.Config) []string {
	tg := acfg.GetTelegramPlatform()
	if tg != nil && len(tg.AllowedUsers) > 0 {
		return tg.AllowedUsers
	}
	return cfg.Telegram.AllowedUsers
}

// setupTelegramBots creates and registers Telegram bots for an agent.
func setupTelegramBots(mgr *BotManager, p AgentSetupParams) {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	tg := acfg.GetTelegramPlatform()
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

	primaryBot.SetCommandContext(p.CommandContext)

	if p.SessionIndex != nil {
		primaryBot.SetSessionIndex(p.SessionIndex)
		primaryBot.RestoreState()
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
	primaryBot.SetStopAliases(cfg.Telegram.StopAliases, cfg.Telegram.EnableStopAliases)
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
	var facetBots []string
	if len(tg.FacetBots) > 0 {
		facetBots = tg.FacetBots
	}
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
			STTProvider:     p.ResolveSTT(p.STTMap, cfg.STT, acfg.STT, voice.MergeReplacements(cfg.Defaults.STTReplacements, acfg.STTReplacements)),
			TTSProvider:     p.ResolveTTS(p.TTSMap, cfg.TTS, acfg.TTS, acfg.TTSRate, voice.MergeReplacements(cfg.Defaults.TTSReplacements, acfg.TTSReplacements)),
			StopAliases:     cfg.Telegram.StopAliases,
			EnableStopAlias: cfg.Telegram.EnableStopAliases,
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
		ttl, _ := time.ParseDuration(cfg.Telegram.FacetSessionTTL)
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
	StopAliases     []string
	EnableStopAlias bool
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
	ToolDetailStore *ToolDetailStore
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
	bot.SetStopAliases(mc.StopAliases, mc.EnableStopAlias)
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
// falling back to global config when the agent field is nil/empty.
func ApplyAgentDisplaySettings(bot *Bot, acfg config.AgentConfig, cfg *config.Config) {
	tg := acfg.GetTelegramPlatform()
	d := bot.display // start from current (preserves ToolCallPreviewChars set earlier)

	switch {
	case tg != nil && tg.ShowToolCalls != nil:
		d.ShowToolCalls = string(*tg.ShowToolCalls)
	case acfg.ShowToolCalls != nil:
		d.ShowToolCalls = string(*acfg.ShowToolCalls)
	case cfg.Telegram.ShowToolCalls != nil:
		d.ShowToolCalls = string(*cfg.Telegram.ShowToolCalls)
	}
	switch {
	case tg != nil && tg.ShowThinking != nil:
		d.ShowThinking = string(*tg.ShowThinking)
	case acfg.ShowThinking != nil:
		d.ShowThinking = string(*acfg.ShowThinking)
	case cfg.Telegram.ShowThinking != nil:
		d.ShowThinking = string(*cfg.Telegram.ShowThinking)
	}
	switch {
	case tg != nil && tg.DisplayWidth != nil:
		d.DisplayWidth = *tg.DisplayWidth
	case cfg.Telegram.DisplayWidth != nil:
		d.DisplayWidth = *cfg.Telegram.DisplayWidth
	}
	switch {
	case tg != nil && tg.TableWrapLines != nil:
		d.TableWrapLines = *tg.TableWrapLines
	case cfg.Telegram.TableWrapLines != nil:
		d.TableWrapLines = *cfg.Telegram.TableWrapLines
	}
	switch {
	case tg != nil && tg.TableStyle != nil:
		d.TableStyle = *tg.TableStyle
	case cfg.Telegram.TableStyle != nil:
		d.TableStyle = *cfg.Telegram.TableStyle
	}
	if acfg.MessagesInLog != nil {
		d.MessagesInLog = *acfg.MessagesInLog
	} else {
		d.MessagesInLog = cfg.Logging.MessagesInLog
	}
	switch {
	case tg != nil && tg.ReceivedFilesDir != "":
		d.ReceivedFilesDir = tg.ReceivedFilesDir
	case cfg.Telegram.ReceivedFilesDir != "":
		d.ReceivedFilesDir = cfg.Telegram.ReceivedFilesDir
	}
	if acfg.InjectedMessageHeader != "" {
		d.InjectedMessageHeader = acfg.InjectedMessageHeader
	} else {
		d.InjectedMessageHeader = cfg.Defaults.InjectedMessageHeader
	}
	d.SteerMode = acfg.SteerMode
	switch {
	case tg != nil && tg.StreamOutput != nil:
		d.StreamOutput = *tg.StreamOutput
	default:
		d.StreamOutput = cfg.Telegram.StreamOutput
	}
	streamInterval := ""
	if tg != nil && tg.StreamInterval != "" {
		streamInterval = tg.StreamInterval
	} else {
		streamInterval = cfg.Telegram.StreamUpdateInterval
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
