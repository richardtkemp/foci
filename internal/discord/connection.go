package discord

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/chatmeta"
	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tooldetail"
	"foci/internal/turn"
	"foci/internal/voice"

	"github.com/bwmarrin/discordgo"
)

var _ platform.Sender = (*Bot)(nil)
var _ platform.SessionNotifier = (*Bot)(nil) // #911: per-session compaction-notice routing
var _ platform.ButtonSender = (*Bot)(nil)

// messageSession is the subset of the discordgo session API the bot uses for
// message I/O (send/edit/delete/typing/interaction responses). It mirrors the
// telegram package's botClient seam: *discordgo.Session satisfies it in
// production, and tests supply a fake. Gateway lifecycle concerns (AddHandler,
// State, Token) stay on the concrete *discordgo.Session.
type messageSession interface {
	ChannelMessageSend(channelID string, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEdit(channelID, messageID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
	ChannelTyping(channelID string, options ...discordgo.RequestOption) error
	InteractionRespond(interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error
}

var _ messageSession = (*discordgo.Session)(nil)

// attachment is a downloaded file ready for the agent (image, PDF, or convertible document).
type attachment struct {
	data      []byte
	mediaType string
	savedPath string // non-empty if saved to disk
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg         *discordgo.Message
	userID      string
	text        string
	attachments []attachment
}

// Bot wraps the Discord API with agent integration.
// Messages are received via WebSocket gateway and processed on a worker goroutine.
type Bot struct {
	log                *log.ComponentLogger
	session            *discordgo.Session // Discord gateway session (handlers, state, token)
	api                messageSession     // message I/O seam; the session in production, a fake in tests
	handler            platform.MessageHandler
	commands           *command.Registry
	dispatcher         *dispatch.Dispatcher
	lastMsgStore       *command.LastMessageStore
	allowedUsers       map[string]bool // Discord user ID strings
	agentID            string
	sessionKey         string // override session key (facet bots)
	sessionMu          sync.RWMutex
	isSecondary        bool
	pool               *Pool
	OnSessionKeyChange func(botID, sessionKey string)
	OnUserMessage      func()
	OnTurnComplete     func()
	OnTurnEnd          func()

	transcriber voice.STT
	tts         voice.TTS

	mq       *platform.MessageQueue // shared message queue (commands + receive funnel)
	agentRef *agent.Agent           // per-agent inbox + Enqueue access; nil for tests, set in agent_setup
	// turnActive is true while Bot.Drive is executing an agent turn.
	// Replaces the old turnCancel-as-activity-indicator after TODO #746
	// moved cancellable ctx ownership into agent.driveOnce.
	turnActive atomic.Bool
	channelID  int64 // last known channel ID (stored as int64 from snowflake)
	channelMu  sync.Mutex

	sessionIndex platform.SessionIndex
	chatmeta     *chatmeta.Resolver

	display       BotDisplayConfig
	fileMode      os.FileMode          // permission bits for saved files (media, etc.)
	toolStore     turn.ToolResultStore // tool-call display state (in-memory + optional SQLite write-through)
	thinkingStore sync.Map             // message ID (int64) -> thinkingEntry; ephemeral

	pendingNotifsMu sync.Mutex // protects pendingNotifs
	pendingNotifs   []string   // notifications buffered during active turns; drained after turn ends

	displayOverrideFn DisplayOverrideFn // per-session display override callback; nil = use bot defaults

	// Discord-specific
	guildID        string // restrict to this guild
	requireMention bool   // require @mention in guild channels
	autoThread     bool   // create threads for facets
	botUserID      string // the bot's own user ID (for mention stripping)

	typingMu     sync.Mutex
	typingCancel context.CancelFunc // non-nil while typing ticker is running
}

// BotDisplayConfig groups all display-related settings. Write-once at startup
// via ApplyAgentDisplaySettings; never mutated after. Per-session overrides
// are handled separately via DisplayOverrideFn.
type BotDisplayConfig struct {
	ShowToolCalls         string        // "off", "preview", "full"
	ShowThinking          string        // "off", "compact", "true"
	StreamOutput          bool          // stream model output to Discord in real-time
	StreamUpdateInterval  time.Duration // duration between Discord message edits during streaming (default 1200ms)
	DisplayWidth          int           // character width for dividers (default 60)
	ToolCallPreviewChars  int           // max chars for tool call preview (default 450)
	MessagesInLog         bool          // log user message content to event log
	ReceivedFilesDir      string        // if non-empty, save received files to this directory
	InjectedMessageHeader string        // prepended to injected (system) messages; empty disables
}

// resolveDisplay snapshots all display settings for a turn with the given session key.
// Applies per-session overrides from displayOverrideFn on top of bot defaults.
func (b *Bot) resolveDisplay(sessionKey string) turn.TurnDisplay {
	d := b.display
	if b.displayOverrideFn != nil {
		ov := b.displayOverrideFn(sessionKey)
		if ov.ShowToolCalls != "" {
			d.ShowToolCalls = ov.ShowToolCalls
		}
		if ov.ShowThinking != "" {
			d.ShowThinking = ov.ShowThinking
		}
		if ov.DisplayWidth > 0 {
			d.DisplayWidth = ov.DisplayWidth
		}
		switch ov.StreamOutput {
		case "true":
			d.StreamOutput = true
		case "false":
			d.StreamOutput = false
		}
	}
	return turn.TurnDisplay{
		ShowToolCalls: d.ShowToolCalls,
		ShowThinking:  d.ShowThinking,
		StreamOutput:  d.StreamOutput,
		DisplayWidth:  d.DisplayWidth,
		RenderOpts:    display.RenderOpts{MaxWidth: d.DisplayWidth},
	}
}

// DisplayOverrides holds per-session overrides for display settings.
// Empty strings / zero values mean "not overridden, use bot default".
type DisplayOverrides struct {
	ShowToolCalls string // "off"/"preview"/"full"
	ShowThinking  string // "off"/"compact"/"true"
	StreamOutput  string // "true"/"false"
	DisplayWidth  int    // 0 = not overridden
}

// DisplayOverrideFn returns per-session display overrides for the given session key.
type DisplayOverrideFn func(sessionKey string) DisplayOverrides

// defaultLogger is used when a Bot is constructed without a ComponentLogger
// (e.g. in tests that build the struct literal directly).
var defaultLogger = log.NewComponentLogger("discord")

// logger returns the bot's ComponentLogger, falling back to a package-level
// default so that test-constructed bots never nil-deref.
func (b *Bot) logger() *log.ComponentLogger {
	if b.log != nil {
		return b.log
	}
	return defaultLogger
}

// NewBot creates a new Discord bot.
// agentID is used for per-chat session key derivation (agent:ID:chat:CHATID).
// For secondary (facet) bots, pass agentID="" -- their session key is set dynamically via SetSessionKey.
func NewBot(dg *discordgo.Session, allowedUsers []string, handler platform.MessageHandler, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) *Bot {
	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}

	lg := log.NewComponentLogger("discord:" + agentID)
	bot := &Bot{
		log:          lg,
		session:      dg,
		api:          dg,
		handler:      handler,
		commands:     cmds,
		lastMsgStore: lastMsgStore,
		allowedUsers: allowed,
		agentID:      agentID,
		chatmeta: &chatmeta.Resolver{
			AgentID:      agentID,
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return lg },
		},
	}
	bot.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size:   64,
		Logger: lg,
	})
	return bot
}

// SessionKeyForChannelID returns the session key for a given channel ID,
// accounting for facet/secondary bot routing. Exported so the urgent-steer
// dispatcher (installed at agent-setup time) can resolve a queued message's
// session without reaching into private bot state.
func (b *Bot) SessionKeyForChannelID(channelID int64) string {
	if b.isSecondary {
		return b.SessionKey()
	}
	if b.agentID != "" {
		return b.sessionKeyForMsg(channelID)
	}
	return b.SessionKey()
}

// SetTranscriber sets the STT provider for inbound voice notes.
func (b *Bot) SetTranscriber(t voice.STT) {
	b.transcriber = t
}

// SetTTS sets the TTS provider for outbound voice notes.
func (b *Bot) SetTTS(t voice.TTS) {
	b.tts = t
}

// SetDisplayOverrideFn sets the callback that provides per-session display overrides.
func (b *Bot) SetDisplayOverrideFn(fn DisplayOverrideFn) { b.displayOverrideFn = fn }

// ShowToolCallsDefault returns the bot's configured show_tool_calls default.
func (b *Bot) ShowToolCallsDefault() string { return b.display.ShowToolCalls }

// ShowThinkingDefault returns the bot's configured show_thinking default.
func (b *Bot) ShowThinkingDefault() string { return b.display.ShowThinking }

// StreamOutputDefault returns the bot's configured stream_output default.
func (b *Bot) StreamOutputDefault() bool { return b.display.StreamOutput }

// DisplayWidthDefault returns the bot's configured display_width default.
func (b *Bot) DisplayWidthDefault() int { return b.display.DisplayWidth }

// streamInterval returns the configured stream update interval, defaulting to 1200ms.
func (b *Bot) streamInterval() time.Duration {
	if b.display.StreamUpdateInterval > 0 {
		return b.display.StreamUpdateInterval
	}
	return 1200 * time.Millisecond
}

// SetToolDetailStore sets the persistent store for tool call details.
// On startup, loads entries <48h old into the in-memory map.
func (b *Bot) SetToolDetailStore(store *tooldetail.Store) {
	b.toolStore.SetDetailStore(store, b.logger())
}

// SetCommandContext configures the command dispatcher with the unified CommandContext.
func (b *Bot) SetCommandContext(cc command.CommandContext) {
	cc.StopFunc = b.cancelTurn
	cc.IsSecondaryBot = b.isSecondary
	if b.isSecondary {
		cc.ReleaseFunc = func() {
			// Capture session key before pool.Release clears it.
			sk := b.SessionKey()
			if b.pool != nil {
				b.pool.Release(b)
			}
			// Close any delegated branch backend so the CC process doesn't leak.
			if sk != "" && cc.Agent != nil && cc.Agent.DelegatedManager != nil {
				cc.Agent.DelegatedManager.ResetSession(sk)
			}
			b.logger().Infof("secondary bot released")
		}
	}
	b.dispatcher = dispatch.NewDispatcher(b.commands, cc, b.agentID)
	b.dispatcher.SetSessionKeyFunc(b.dispatchSessionKey)
}

// dispatchSessionKey resolves the session key for command dispatch.
// Secondary bots with an override session key use it directly;
// primary bots resolve per-chat keys as usual.
func (b *Bot) dispatchSessionKey(chatID int64) string {
	if b.isSecondary {
		b.sessionMu.RLock()
		sk := b.sessionKey
		b.sessionMu.RUnlock()
		if sk != "" {
			return sk
		}
	}
	return b.sessionKeyForMsg(chatID)
}

// SetSessionIndex sets the session index for persisting chat-to-session-key mappings.
// Must be called before the bot starts receiving messages to ensure session continuity across restarts.
func (b *Bot) SetSessionIndex(idx platform.SessionIndex) {
	b.sessionIndex = idx
	if b.chatmeta != nil {
		b.chatmeta.Index = idx
	}
}

// SetSecondary marks this bot as a secondary bot in the given pool.
func (b *Bot) SetSecondary(pool *Pool) {
	b.isSecondary = true
	b.pool = pool
}

// SetHandlerAndCommands re-wires the bot to a different agent and command registry.
// Only safe to call on idle secondary bots (no active session key) between
// pool acquisition and setting the session key.
func (b *Bot) SetHandlerAndCommands(handler platform.MessageHandler, cmds *command.Registry) {
	b.handler = handler
	b.commands = cmds
}

// sanitizeError strips the bot token from error messages to prevent leaking into logs.
func (b *Bot) sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	if b.session == nil || b.session.Token == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), b.session.Token, "[REDACTED]")
}

// thinkingEntry stores thinking text and response markdown for compact mode toggle.
type thinkingEntry struct {
	responseText string // the original response (collapsed state)
	thinkingText string // raw thinking text
}
