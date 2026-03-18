package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/command"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/voice"

	"github.com/bwmarrin/discordgo"
)

var _ platform.Sender = (*Bot)(nil)

// sessionIndexInterface abstracts session index operations for testability.
type sessionIndexInterface interface {
	GetChatMetadata(agentID, platform string, chatID int64, key string) (string, error)
	SetChatMetadata(agentID, platform string, chatID int64, key, value string) error
	GetAgentMetadata(agentID, key string) (string, error)
	SetAgentMetadata(agentID, key, value string) error
	DeleteAgentMetadata(agentID, key string) error
}

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
	session            *discordgo.Session  // Discord API session (shared across bots for same token)
	handler            platform.MessageHandler
	commands           *command.Registry
	dispatcher         *Dispatcher
	lastMsgStore       *command.LastMessageStore
	allowedUsers       map[string]bool     // Discord user ID strings
	agentID            string
	sessionKey         string              // override session key (facet bots)
	sessionMu          sync.RWMutex
	isSecondary        bool
	pool               *Pool
	OnSessionKeyChange func(botID, sessionKey string)
	OnUserMessage      func()
	OnTurnComplete     func()
	OnTurnEnd          func()

	transcriber voice.STT
	tts         voice.TTS

	queue      chan queuedMessage
	turnCancel context.CancelFunc
	turnMu     sync.Mutex
	channelID  int64 // last known channel ID (stored as int64 from snowflake)
	channelMu  sync.Mutex

	chatSessionKeys map[int64]string
	chatKeysMu      sync.RWMutex
	sessionIndex    sessionIndexInterface

	display         BotDisplayConfig
	toolResults     sync.Map         // message ID (int64) -> toolResultEntry; for button expansion
	thinkingStore   sync.Map         // message ID (int64) -> thinkingEntry; ephemeral
	toolDetailStore *ToolDetailStore // nil = no persistence; write-through to SQLite

	steerMu    sync.Mutex // protects steerParts
	steerParts []string   // pending steer messages; drained atomically

	pendingNotifsMu sync.Mutex // protects pendingNotifs
	pendingNotifs   []string   // notifications buffered during active turns; drained after turn ends

	displayOverrideFn DisplayOverrideFn // per-session display override callback; nil = use bot defaults

	// Discord-specific
	guildID        string // restrict to this guild
	requireMention bool   // require @mention in guild channels
	autoThread     bool   // create threads for facets
	botUserID      string // the bot's own user ID (for mention stripping)
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
	SteerMode             bool          // user messages route to buffer during active turns
}

// turnDisplay holds resolved display settings for a single turn.
// Resolved once at turn start to avoid repeated override lookups.
type turnDisplay struct {
	showToolCalls string
	showThinking  string
	streamOutput  bool
	displayWidth  int
	renderOpts    display.RenderOpts
}

// resolveDisplay snapshots all display settings for a turn with the given session key.
// Applies per-session overrides from displayOverrideFn on top of bot defaults.
func (b *Bot) resolveDisplay(sessionKey string) turnDisplay {
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
	return turnDisplay{
		showToolCalls: d.ShowToolCalls,
		showThinking:  d.ShowThinking,
		streamOutput:  d.StreamOutput,
		displayWidth:  d.DisplayWidth,
		renderOpts:    display.RenderOpts{MaxWidth: d.DisplayWidth},
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

	return &Bot{
		log:             log.NewComponentLogger("discord:" + agentID),
		session:         dg,
		handler:         handler,
		commands:        cmds,
		lastMsgStore:    lastMsgStore,
		allowedUsers:    allowed,
		agentID:         agentID,
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
	}
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

// appendSteer adds text to the steer buffer. Called by the receiver goroutine.
func (b *Bot) appendSteer(text string) {
	b.steerMu.Lock()
	b.steerParts = append(b.steerParts, text)
	b.steerMu.Unlock()
}

// drainSteer returns all pending steer parts and clears the buffer.
// Called by the agent loop via SteerCheckFunc. Returns nil if no messages are pending.
func (b *Bot) drainSteer() []string {
	b.steerMu.Lock()
	defer b.steerMu.Unlock()
	if len(b.steerParts) == 0 {
		return nil
	}
	parts := b.steerParts
	b.steerParts = nil
	return parts
}

// SetToolDetailStore sets the persistent store for tool call details.
// On startup, loads entries <48h old into the in-memory map.
func (b *Bot) SetToolDetailStore(store *ToolDetailStore) {
	b.toolDetailStore = store
	if store == nil {
		return
	}
	entries, err := store.LoadAll()
	if err != nil {
		b.logger().Warnf("load tool details: %v", err)
		return
	}
	for id, entry := range entries {
		b.toolResults.Store(id, entry)
	}
	if len(entries) > 0 {
		b.logger().Infof("restored %d tool call details from disk", len(entries))
	}
}

// SetCommandContext configures the command dispatcher with the unified CommandContext.
func (b *Bot) SetCommandContext(cc command.CommandContext) {
	cc.StopFunc = b.cancelTurn
	cc.IsSecondaryBot = b.isSecondary
	if b.isSecondary {
		cc.ReleaseFunc = func() {
			if b.pool != nil {
				b.pool.Release(b)
			}
			b.logger().Infof("secondary bot released")
		}
	}
	b.dispatcher = NewDispatcher(b.commands, cc, b.agentID)
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

// RestoreState restores bot state (channelID, default channel) from the session index.
// Must be called after SetSessionIndex.
func (b *Bot) RestoreState() {
	if b.sessionIndex == nil || b.agentID == "" {
		return
	}

	// Restore channelID from persisted state
	if raw, err := b.sessionIndex.GetAgentMetadata(b.agentID, "bot_channel_id"); err == nil && raw != "" {
		var channelID int64
		if _, err := fmt.Sscanf(raw, "%d", &channelID); err == nil && channelID != 0 {
			b.SetChatID(channelID)
			b.logger().Infof("restored channel ID %d from state", channelID)
		}
	}

	// Restore default channel from persisted state (for per-chat session routing)
	if raw, err := b.sessionIndex.GetAgentMetadata(b.agentID, "default_channel"); err == nil && raw != "" {
		b.logger().Infof("restored default channel for agent %s", b.agentID)
	}
}

// SetSessionIndex sets the session index for persisting chat-to-session-key mappings.
// Must be called before the bot starts receiving messages to ensure session continuity across restarts.
func (b *Bot) SetSessionIndex(idx sessionIndexInterface) {
	b.sessionIndex = idx
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
