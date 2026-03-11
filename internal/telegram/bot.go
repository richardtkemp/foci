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
}

// sessionIndexInterface abstracts session index operations for testability.
type sessionIndexInterface interface {
	GetChatMetadata(agentID string, chatID int64, key string) (string, error)
	SetChatMetadata(agentID string, chatID int64, key, value string) error
}

// attachment is a downloaded image ready for the agent.
type attachment struct {
	data      []byte
	mediaType string
	savedPath string // non-empty if saved to disk
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg    *gotgbot.Message
	userID string
	text   string
	images []attachment
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
	sessionKey         string                            // override session key (multiball secondary bots only)
	sessionMu          sync.RWMutex                      // protects sessionKey (mutable for secondary bots)
	isSecondary        bool                              // true for secondary bots (multiball)
	pool               *Pool                             // back-reference to pool (secondary bots only)
	OnSessionKeyChange func(username, sessionKey string) // fires after SetSessionKey (fork/release)
	OnUserMessage      func()                            // fires on each inbound user message (for keepalive interaction tracking)
	OnTurnComplete     func()                            // fires after each agent turn completes (for cache warming tracking)
	botToken           string                            // for building file download URLs

	transcriber       voice.STT // nil = voice notes not supported
	tts               voice.TTS // nil = TTS not available
	stopAliases       []string  // aliases for /stop command
	enableStopAliases bool      // whether to use stop aliases (default true)

	queue      chan queuedMessage // receiver → agent worker
	turnCancel context.CancelFunc // cancel the current agent turn
	turnMu     sync.Mutex         // protects turnCancel
	chatID     int64              // last known chat ID (for notifications)
	chatMu     sync.Mutex

	chatSessionKeys map[int64]string      // cache of chat ID → session key (prevents regenerating keys on every message)
	chatKeysMu      sync.RWMutex          // protects chatSessionKeys
	sessionIndex    sessionIndexInterface // nil = no session key persistence across restarts

	stateStore            *state.Store     // nil = no persistence
	stateKey              string           // state key prefix (e.g. "bot:mybot")
	toolCallPreviewChars  int              // max chars for tool call preview (default 450)
	showToolCalls         string           // tool call display mode: "off", "preview", "full"
	showThinking          string           // thinking display mode: "off", "compact", "true"
	displayWidth          int              // character width for dividers (default 44)
	tableWrapLines        int              // max wrapped lines per table cell (default 5)
	tableStyle            string           // table style: "pretty" (default) or "markdown"
	messagesInLog         bool             // log user message content to event log (default false for privacy)
	receivedFilesDir      string           // if non-empty, save received files to this directory
	injectedMessageHeader string           // prepended to injected (system) messages; empty disables
	toolResults           sync.Map         // message ID (int64) → toolResultEntry; for inline keyboard expansion
	thinkingStore         sync.Map         // message ID (int64) → thinkingEntry; ephemeral, for inline keyboard expansion
	toolDetailStore       *ToolDetailStore // nil = no persistence; write-through to SQLite

	steerMode  bool       // steer mode enabled: user messages route to buffer during active turns
	steerMu    sync.Mutex // protects steerParts
	steerParts []string   // pending steer messages; drained atomically

	streamOutput         bool          // stream model output to Telegram in real-time
	streamUpdateInterval time.Duration // duration between Telegram message edits during streaming
}

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
// For secondary (multiball) bots, pass agentID="" — their session key is set dynamically via SetSessionKey.
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

// SetToolCallPreviewChars sets the max characters for tool call param preview.
func (b *Bot) SetToolCallPreviewChars(n int) {
	b.toolCallPreviewChars = n
}

// SetShowToolCalls controls how tool call messages are displayed in Telegram.
// Accepts "off", "preview", or "full".
func (b *Bot) SetShowToolCalls(mode string) {
	b.showToolCalls = mode
}

// SetShowThinking controls how thinking blocks are displayed in Telegram.
// Accepts "off", "compact", or "true".
func (b *Bot) SetShowThinking(mode string) {
	b.showThinking = mode
}

// SetDisplayWidth sets the character width used for divider lines.
func (b *Bot) SetDisplayWidth(width int) {
	b.displayWidth = width
}

// SetTableWrapLines sets the max wrapped lines per table cell.
func (b *Bot) SetTableWrapLines(n int) {
	b.tableWrapLines = n
}

// SetTableStyle sets the table rendering style ("pretty" or "markdown").
func (b *Bot) SetTableStyle(style string) {
	b.tableStyle = style
}

// tableOpts returns the RenderOpts for this bot's display settings.
func (b *Bot) tableOpts() display.RenderOpts {
	return display.RenderOpts{MaxWidth: b.displayWidth, WrapLines: b.tableWrapLines, Style: b.tableStyle}
}

// SetMessagesInLog controls whether user message content is logged to the event log.
func (b *Bot) SetMessagesInLog(v bool) {
	b.messagesInLog = v
}

// SetReceivedFilesDir configures auto-saving of received files to disk.
// Empty string disables saving.
func (b *Bot) SetReceivedFilesDir(dir string) {
	b.receivedFilesDir = dir
}

// SetInjectedMessageHeader sets the header prepended to injected (system) messages.
// Empty string disables the header.
func (b *Bot) SetInjectedMessageHeader(s string) {
	b.injectedMessageHeader = s
}

// SetSteerMode enables or disables steer mode.
// When enabled, user messages route to the steer buffer during active turns
// instead of queuing behind the turn lock.
func (b *Bot) SetSteerMode(enabled bool) {
	b.steerMode = enabled
}

// SetStreamOutput enables or disables real-time streaming of model output to Telegram.
func (b *Bot) SetStreamOutput(enabled bool) { b.streamOutput = enabled }

// SetStreamUpdateInterval sets the duration between Telegram message edits during streaming.
func (b *Bot) SetStreamUpdateInterval(d time.Duration) { b.streamUpdateInterval = d }

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

// SetDeps configures the command dispatcher with platform-agnostic dependencies.
// When set, the bot uses DispatchV2 for command execution.
func (b *Bot) SetDeps(deps command.Deps) {
	b.dispatcher = NewDispatcher(b.commands, deps, b.agentID)
	b.dispatcher.SetSessionKeyFunc(b.sessionKeyForMsg)
}

// DisplaySettings returns the current display settings for inspection/testing.
func (b *Bot) DisplaySettings() (showToolCalls, showThinking string, displayWidth int, messagesInLog bool, receivedFilesDir string, injectedMessageHeader string) {
	return b.showToolCalls, b.showThinking, b.displayWidth, b.messagesInLog, b.receivedFilesDir, b.injectedMessageHeader
}

// NewBotForTest creates a Bot without connecting to the Telegram API.
// For use in tests outside the telegram package.
func NewBotForTest() *Bot {
	return &Bot{
		log:             log.NewComponentLogger("telegram:test"),
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
	}
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
		if store.Get("agent:"+b.agentID+":default_chat", &defaultChat) && defaultChat != 0 {
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
