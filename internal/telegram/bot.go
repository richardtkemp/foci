package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"foci/internal/command"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/state"
	"foci/internal/voice"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

var _ platform.Sender = (*Bot)(nil)

// botClient abstracts Telegram API methods for testability.
type botClient interface {
	SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error)
	EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error)
	SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error)
	SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error)
	SendVideo(chatId int64, video gotgbot.InputFileOrString, opts *gotgbot.SendVideoOpts) (*gotgbot.Message, error)
	SendPhoto(chatId int64, photo gotgbot.InputFileOrString, opts *gotgbot.SendPhotoOpts) (*gotgbot.Message, error)
	SendAudio(chatId int64, audio gotgbot.InputFileOrString, opts *gotgbot.SendAudioOpts) (*gotgbot.Message, error)
	SendAnimation(chatId int64, animation gotgbot.InputFileOrString, opts *gotgbot.SendAnimationOpts) (*gotgbot.Message, error)
	SendChatAction(chatId int64, action string, opts *gotgbot.SendChatActionOpts) (bool, error)
	GetFile(fileId string, opts *gotgbot.GetFileOpts) (*gotgbot.File, error)
	SetMyCommands(commands []gotgbot.BotCommand, opts *gotgbot.SetMyCommandsOpts) (bool, error)
	AnswerCallbackQuery(callbackQueryId string, opts *gotgbot.AnswerCallbackQueryOpts) (bool, error)
	DeleteMessage(chatId int64, messageId int64, opts *gotgbot.DeleteMessageOpts) (bool, error)
}

// sessionIndexInterface abstracts session index operations for testability.
type sessionIndexInterface interface {
	GetChatMetadata(agentID string, chatID int64, key string) (string, error)
	SetChatMetadata(agentID string, chatID int64, key, value string) error
}

// attachment is a downloaded file ready for the agent (image, PDF, or convertible document).
type attachment struct {
	data      []byte
	mediaType string
	savedPath string // non-empty if saved to disk
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg         *gotgbot.Message
	userID      string
	text        string
	attachments []attachment
}

// Bot wraps the Telegram bot API with agent integration.
// Messages are received on one goroutine and processed on another.
// Slash commands execute immediately on the receiver goroutine.
type Bot struct {
	log                *log.ComponentLogger
	api                *gotgbot.Bot // for receiving updates (Run)
	client             botClient    // for sending messages and files (mockable in tests)
	handler            platform.MessageHandler
	commands           *command.Registry
	dispatcher         *Dispatcher               // platform-aware command dispatch (nil = use legacy dispatch)
	lastMsgStore       *command.LastMessageStore // for // repeat command
	allowedUsers       map[string]bool
	agentID            string                            // agent ID for session key derivation
	sessionKey         string                            // override session key (facet secondary bots only)
	sessionMu          sync.RWMutex                      // protects sessionKey (mutable for secondary bots)
	isSecondary        bool                              // true for secondary bots (facet)
	pool               *Pool                             // back-reference to pool (secondary bots only)
	OnSessionKeyChange func(username, sessionKey string) // fires after SetSessionKey (fork/release)
	OnUserMessage      func()                            // fires on each inbound user message (for keepalive interaction tracking)
	OnTurnComplete     func()                            // fires after each agent turn completes (for cache warming tracking)
	OnTurnEnd          func()                            // fires after turn's final message is sent and cleanup is done
	botToken           string                            // for building file download URLs

	transcriber       voice.STT // nil = voice notes not supported
	tts               voice.TTS // nil = TTS not available
	stopAliases       []string  // aliases for /stop command
	enableStopAliases bool      // whether to use stop aliases (default true)

	queue          chan queuedMessage // receiver → agent worker
	turnCancel     context.CancelFunc // cancel the current agent turn
	turnMu         sync.Mutex         // protects turnCancel
	chatID         int64              // last known chat ID (for notifications)
	chatMu     sync.Mutex

	chatSessionKeys map[int64]string      // cache of chat ID → session key (prevents regenerating keys on every message)
	chatKeysMu      sync.RWMutex          // protects chatSessionKeys
	sessionIndex    sessionIndexInterface // nil = no session key persistence across restarts

	stateStore  *state.Store // nil = no persistence
	stateKey    string       // state key prefix (e.g. "bot:mybot")
	display     BotDisplayConfig
	toolResults sync.Map         // message ID (int64) → toolResultEntry; for inline keyboard expansion
	thinkingStore sync.Map       // message ID (int64) → thinkingEntry; ephemeral, for inline keyboard expansion
	toolDetailStore *ToolDetailStore // nil = no persistence; write-through to SQLite

	steerMu    sync.Mutex // protects steerParts
	steerParts []string   // pending steer messages; drained atomically

	pendingNotifsMu sync.Mutex // protects pendingNotifs
	pendingNotifs   []string   // notifications buffered during active turns; drained after turn ends

	displayOverrideFn DisplayOverrideFn // per-session display override callback; nil = use bot defaults
}

// BotDisplayConfig groups all display-related settings. Write-once at startup
// via ApplyAgentDisplaySettings; never mutated after. Per-session overrides
// are handled separately via DisplayOverrideFn.
type BotDisplayConfig struct {
	ShowToolCalls         string        // "off", "preview", "full"
	ShowThinking          string        // "off", "compact", "true"
	StreamOutput          bool          // stream model output to Telegram in real-time
	StreamUpdateInterval  time.Duration // duration between Telegram message edits during streaming
	DisplayWidth          int           // character width for dividers (default 44)
	TableWrapLines        int           // max wrapped lines per table cell (default 5)
	TableStyle            string        // "pretty" (default) or "markdown"
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
		renderOpts:    display.RenderOpts{MaxWidth: d.DisplayWidth, WrapLines: d.TableWrapLines, Style: d.TableStyle},
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
var defaultLogger = log.NewComponentLogger("telegram")

// logger returns the bot's ComponentLogger, falling back to a package-level
// default so that test-constructed bots never nil-deref.
func (b *Bot) logger() *log.ComponentLogger {
	if b.log != nil {
		return b.log
	}
	return defaultLogger
}

// NewBot creates a new Telegram bot.
// agentID is used for per-chat session key derivation (agent:ID:chat:CHATID).
// For secondary (facet) bots, pass agentID="" — their session key is set dynamically via SetSessionKey.
func NewBot(token string, allowedUsers []string, handler platform.MessageHandler, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) (*Bot, error) {
	// Use a transport with enough connections for concurrent API calls.
	// The default http.Transport has MaxIdleConnsPerHost=2 which is too low:
	// GetUpdates long-poll holds 1 connection, the agent worker sends typing
	// indicators + tool call messages on another, leaving 0 for the receiver
	// goroutine to handle callback queries or slash commands.
	api, err := gotgbot.NewBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			Client: http.Client{
				Transport: &http.Transport{
					MaxIdleConnsPerHost: 8,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}

	return &Bot{
		log:             log.NewComponentLogger("telegram:" + agentID),
		api:             api,
		client:          api,
		handler:         handler,
		commands:        cmds,
		lastMsgStore:    lastMsgStore,
		allowedUsers:    allowed,
		agentID:         agentID,
		botToken:        token,
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
	}, nil
}

// SetTranscriber sets the STT provider for inbound voice notes.
func (b *Bot) SetTranscriber(t voice.STT) {
	b.transcriber = t
}

// SetTTS sets the TTS provider for outbound voice notes.
func (b *Bot) SetTTS(t voice.TTS) {
	b.tts = t
}

// SetStopAliases sets the aliases for the /stop command and whether to enable them.
func (b *Bot) SetStopAliases(aliases []string, enabled bool) {
	b.stopAliases = aliases
	b.enableStopAliases = enabled
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

// tableOpts returns the RenderOpts using the bot's default display settings.
// For turn-aware rendering with per-session overrides, use resolveDisplay().renderOpts.
func (b *Bot) tableOpts() display.RenderOpts {
	return display.RenderOpts{MaxWidth: b.display.DisplayWidth, WrapLines: b.display.TableWrapLines, Style: b.display.TableStyle}
}

// streamInterval returns the configured stream update interval, defaulting to 250ms.
func (b *Bot) streamInterval() time.Duration {
	if b.display.StreamUpdateInterval > 0 {
		return b.display.StreamUpdateInterval
	}
	return 250 * time.Millisecond
}

// appendSteer adds text to the steer buffer. Called by the receiver goroutine.
func (b *Bot) appendSteer(text string) {
	b.steerMu.Lock()
	b.steerParts = append(b.steerParts, text)
	b.steerMu.Unlock()
}

// drainSteer returns all pending steer text joined with newlines and clears the buffer.
// Called by the agent loop via SteerCheckFunc. Returns "" if no messages are pending.
func (b *Bot) drainSteer() string {
	b.steerMu.Lock()
	defer b.steerMu.Unlock()
	if len(b.steerParts) == 0 {
		return ""
	}
	text := strings.Join(b.steerParts, "\n")
	b.steerParts = nil
	return text
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

// DisplaySettings returns the bot's default display settings for inspection/testing.
func (b *Bot) DisplaySettings() (showToolCalls, showThinking string, displayWidth int, messagesInLog bool, receivedFilesDir string, injectedMessageHeader string) {
	d := b.display
	return d.ShowToolCalls, d.ShowThinking, d.DisplayWidth, d.MessagesInLog, d.ReceivedFilesDir, d.InjectedMessageHeader
}

// SetStateStore configures persistent state for this bot.
// key is used as the prefix for state keys (e.g. "bot:mybot").
func (b *Bot) SetStateStore(store *state.Store, key string) {
	b.stateStore = store
	b.stateKey = key

	// Restore chatID from persisted state
	var chatID int64
	if store.Get(key+":chatid", &chatID) && chatID != 0 {
		b.SetChatID(chatID)
		b.logger().Infof("restored chat ID %d from state", chatID)
	}

	// Restore default chat from persisted state (for per-chat session routing)
	if b.agentID != "" {
		var defaultChat int64
		if store.Get("agent/"+b.agentID+"/default_chat", &defaultChat) && defaultChat != 0 {
			b.logger().Infof("restored default chat %d for agent %s", defaultChat, b.agentID)
		}
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

// SetAgentAndCommands re-wires the bot to a different agent and command registry.
// Only safe to call on idle secondary bots (no active session key) between
// pool acquisition and setting the session key.
func (b *Bot) SetHandlerAndCommands(handler platform.MessageHandler, cmds *command.Registry) {
	b.handler = handler
	b.commands = cmds
}
