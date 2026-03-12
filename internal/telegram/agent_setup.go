package telegram

import (
	"context"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/voice"
)

// AgentSetupParams holds all dependencies needed to set up platform bots for an agent.
type AgentSetupParams struct {
	Agent           platform.MessageHandler
	Commands        *command.Registry
	LastMsgStore    *command.LastMessageStore
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
	SecretStore     *secrets.Store
	Sessions        *session.Store
	StateStore      *state.Store
	SessionIndex    *session.SessionIndex
	ToolDetailStore *ToolDetailStore
	STT             voice.STT
	TTS             voice.TTS
	STTMap          map[string]voice.STT
	TTSMap          map[string]voice.TTS
	Ctx             context.Context //nolint:containedctx

	// ReclaimHook is called when a multiball session is reclaimed.
	ReclaimHook func(sessionKey string)

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
		ConfigureMultiballConn: func(conn platform.Connection) {
			tBot, ok := conn.(*Bot)
			if !ok {
				return
			}
			tBot.SetHandlerAndCommands(p.Agent, p.Commands)
			ApplyAgentDisplaySettings(tBot, acfg, cfg)
		},
	}
}

// resolveAllowedUsers returns the effective allowed user list for an agent.
// Priority: per-agent platform config > per-agent deprecated field > global.
func resolveAllowedUsers(acfg config.AgentConfig, cfg *config.Config) []string {
	tg := acfg.GetTelegramPlatform()
	if tg != nil && len(tg.AllowedUsers) > 0 {
		return tg.AllowedUsers
	}
	if len(acfg.AllowedUsers) > 0 {
		return acfg.AllowedUsers
	}
	return cfg.Telegram.AllowedUsers
}

// setupTelegramBots creates and registers Telegram bots for an agent.
func setupTelegramBots(mgr *BotManager, p AgentSetupParams) {
	acfg := p.AgentConfig
	cfg := p.GlobalConfig

	tg := acfg.GetTelegramPlatform()
	var botName, botSecret string
	switch {
	case tg != nil && tg.Bot != "":
		botName = tg.Bot
		botSecret = tg.BotSecret
	default:
		botName = acfg.TelegramBot
		botSecret = acfg.BotSecret
	}

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

	if p.StateStore != nil {
		botKey := "bot:" + botName
		if botKey == "bot:" {
			botKey = "bot:" + acfg.ID
		}
		primaryBot.SetStateStore(p.StateStore, botKey)
	}
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
	primaryBot.SetStopAliases(cfg.Telegram.StopAliases, cfg.Telegram.EnableStopAliases)
	primaryBot.SetToolCallPreviewChars(cfg.Tools.ToolCallPreviewChars)
	ApplyAgentDisplaySettings(primaryBot, acfg, cfg)

	mgr.AddPrimary(acfg.ID, primaryBot)

	// Per-agent multiball bots
	var multiballBots []string
	if tg != nil && len(tg.MultiballBots) > 0 {
		multiballBots = tg.MultiballBots
	} else {
		multiballBots = acfg.MultiballBots
	}
	for _, mbName := range multiballBots {
		mbToken := config.ResolveBotToken(mbName, "", p.SecretStore)
		if mbToken == "" {
			log.Errorf("telegram", "agent %q: multiball bot %q: token not found", acfg.ID, mbName)
			continue
		}
		mbBot, err := NewBot(mbToken, allowedUsers, p.Agent, p.Commands, p.LastMsgStore, "")
		if err != nil {
			log.Errorf("telegram", "agent %q: create multiball bot %q: %v", acfg.ID, mbName, err)
			continue
		}
		ConfigureMultiballBot(mbBot, MultiballBotConfig{
			STTProvider:     p.ResolveSTT(p.STTMap, cfg.STT, acfg.STT, voice.MergeReplacements(cfg.Defaults.STTReplacements, acfg.STTReplacements)),
			TTSProvider:     p.ResolveTTS(p.TTSMap, cfg.TTS, acfg.TTS, acfg.TTSRate, voice.MergeReplacements(cfg.Defaults.TTSReplacements, acfg.TTSReplacements)),
			StopAliases:     cfg.Telegram.StopAliases,
			EnableStopAlias: cfg.Telegram.EnableStopAliases,
			AgentConfig:     acfg,
			GlobalConfig:    cfg,
			ToolDetailStore: p.ToolDetailStore,
			StateStore:      p.StateStore,
		})
		mgr.AddMultiball(acfg.ID, mbBot)
	}
	if pool := mgr.Pool(acfg.ID); pool != nil && pool.Size() > 0 {
		log.Infof("telegram", "agent %q: %d per-agent multiball bots ready", acfg.ID, pool.Size())
	}

	// Configure session TTL for per-agent multiball pool
	if pool := mgr.Pool(acfg.ID); pool != nil {
		ttl, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
		if ttl > 0 {
			pool.SetSessionTTL(ttl, p.Sessions)
			log.Infof("telegram", "agent %q: multiball session TTL = %v", acfg.ID, ttl)
		}
		if p.ReclaimHook != nil {
			pool.ReclaimHook = p.ReclaimHook
		}
	}
}

// MultiballBotConfig holds common settings applied to every multiball bot.
type MultiballBotConfig struct {
	STTProvider     voice.STT
	TTSProvider     voice.TTS
	StopAliases     []string
	EnableStopAlias bool
	AgentConfig     config.AgentConfig
	GlobalConfig    *config.Config
	ToolDetailStore *ToolDetailStore
	StateStore      *state.Store
}

// ConfigureMultiballBot applies the standard multiball bot settings.
func ConfigureMultiballBot(bot *Bot, mc MultiballBotConfig) {
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
	if mc.StateStore != nil {
		ss := mc.StateStore
		bot.OnSessionKeyChange = func(username, sessionKey string) {
			key := "multiball:" + username
			if sessionKey == "" {
				_ = ss.Delete(key)
			} else {
				_ = ss.Set(key, sessionKey)
			}
		}
	}
}

// ApplyAgentDisplaySettings sets per-agent display settings on a bot,
// falling back to global config when the agent field is nil/empty.
func ApplyAgentDisplaySettings(bot *Bot, acfg config.AgentConfig, cfg *config.Config) {
	tg := acfg.GetTelegramPlatform()

	switch {
	case tg != nil && tg.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*tg.ShowToolCalls))
	case acfg.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*acfg.ShowToolCalls))
	case cfg.Defaults.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*cfg.Defaults.ShowToolCalls))
	}
	switch {
	case tg != nil && tg.ShowThinking != nil:
		bot.SetShowThinking(string(*tg.ShowThinking))
	case acfg.ShowThinking != nil:
		bot.SetShowThinking(string(*acfg.ShowThinking))
	case cfg.Defaults.ShowThinking != nil:
		bot.SetShowThinking(string(*cfg.Defaults.ShowThinking))
	}
	switch {
	case tg != nil && tg.DisplayWidth != nil:
		bot.SetDisplayWidth(*tg.DisplayWidth)
	case acfg.DisplayWidth != nil:
		bot.SetDisplayWidth(*acfg.DisplayWidth)
	case cfg.Telegram.DisplayWidth != nil:
		bot.SetDisplayWidth(*cfg.Telegram.DisplayWidth)
	}
	switch {
	case tg != nil && tg.TableWrapLines != nil:
		bot.SetTableWrapLines(*tg.TableWrapLines)
	case acfg.TableWrapLines != nil:
		bot.SetTableWrapLines(*acfg.TableWrapLines)
	case cfg.Telegram.TableWrapLines != nil:
		bot.SetTableWrapLines(*cfg.Telegram.TableWrapLines)
	}
	switch {
	case tg != nil && tg.TableStyle != nil:
		bot.SetTableStyle(*tg.TableStyle)
	case acfg.TableStyle != nil:
		bot.SetTableStyle(*acfg.TableStyle)
	case cfg.Telegram.TableStyle != nil:
		bot.SetTableStyle(*cfg.Telegram.TableStyle)
	}
	if acfg.MessagesInLog != nil {
		bot.SetMessagesInLog(*acfg.MessagesInLog)
	} else {
		bot.SetMessagesInLog(cfg.Logging.MessagesInLog)
	}
	switch {
	case tg != nil && tg.ReceivedFilesDir != "":
		bot.SetReceivedFilesDir(tg.ReceivedFilesDir)
	case acfg.ReceivedFilesDir != "":
		bot.SetReceivedFilesDir(acfg.ReceivedFilesDir)
	case cfg.Telegram.ReceivedFilesDir != "":
		bot.SetReceivedFilesDir(cfg.Telegram.ReceivedFilesDir)
	}
	if acfg.InjectedMessageHeader != "" {
		bot.SetInjectedMessageHeader(acfg.InjectedMessageHeader)
	} else {
		bot.SetInjectedMessageHeader(cfg.Defaults.InjectedMessageHeader)
	}
	bot.SetSteerMode(acfg.SteerMode)
	switch {
	case tg != nil && tg.StreamOutput != nil:
		bot.SetStreamOutput(*tg.StreamOutput)
	default:
		bot.SetStreamOutput(cfg.Defaults.StreamOutput)
	}
	streamInterval := ""
	if tg != nil && tg.StreamInterval != "" {
		streamInterval = tg.StreamInterval
	} else {
		streamInterval = cfg.Defaults.StreamUpdateInterval
	}
	if d, err := time.ParseDuration(streamInterval); err == nil && d > 0 {
		bot.SetStreamUpdateInterval(d)
	}
}

// extractAgentID extracts the agent ID from a session key string.
func extractAgentID(sessionKey string) string {
	sk, err := session.ParseSessionKey(sessionKey)
	if err != nil {
		return ""
	}
	return sk.AgentID
}
