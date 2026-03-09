package telegram

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
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

// SessionKey returns the current session key (thread-safe).
// For primary bots, this returns the session key for the default chat.
// For secondary bots, this returns the override session key (set by multiball).
func (b *Bot) SessionKey() string {
	b.sessionMu.RLock()
	defer b.sessionMu.RUnlock()
	if b.sessionKey != "" {
		return b.sessionKey
	}
	// Primary bot: derive from default chat
	if b.agentID != "" {
		chatID := b.defaultChatID()
		if chatID != 0 {
			return b.sessionKeyForMsg(chatID)
		}
	}
	return ""
}

// NewSessionKeyForChat creates a NEW session key for a specific chat ID.
// Each call generates a unique key. Use Bot.SessionKeyForChat for stable cached keys.
func NewSessionKeyForChat(agentID string, chatID int64) string {
	return session.NewChatSessionKey(agentID, chatID)
}

// SessionKeyForChat returns the stable, cached session key for a specific chat ID.
// Creates a new key on first call for a given chat, then returns the cached value.
func (b *Bot) SessionKeyForChat(chatID int64) string {
	return b.sessionKeyForMsg(chatID)
}

// sessionKeyForMsg returns the session key for the message's chat.
// Uses an in-memory cache and session index to avoid regenerating keys on every message.
// Persists new keys to the session index for continuity across restarts.
func (b *Bot) sessionKeyForMsg(chatID int64) string {
	// Check in-memory cache first
	b.chatKeysMu.RLock()
	if key, ok := b.chatSessionKeys[chatID]; ok {
		b.chatKeysMu.RUnlock()
		return key
	}
	b.chatKeysMu.RUnlock()

	// In-memory cache miss — check session index (persisted across restarts)
	if b.sessionIndex != nil && b.agentID != "" {
		if persistedKey, err := b.sessionIndex.GetChatMetadata(b.agentID, chatID, "session_key"); err == nil && persistedKey != "" {
			// Found persisted key — populate in-memory cache and return
			b.chatKeysMu.Lock()
			b.chatSessionKeys[chatID] = persistedKey
			b.chatKeysMu.Unlock()
			b.logger().Infof("restored session key for chat %d: %s", chatID, persistedKey)
			return persistedKey
		}
	}

	// No persisted key — generate new key and persist it
	key := NewSessionKeyForChat(b.agentID, chatID)
	b.chatKeysMu.Lock()
	b.chatSessionKeys[chatID] = key
	b.chatKeysMu.Unlock()

	// Persist to session index for future restarts
	if b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetChatMetadata(b.agentID, chatID, "session_key", key); err != nil {
			b.logger().Errorf("persist session key for chat %d: %v", chatID, err)
		} else {
			b.logger().Infof("persisted new session key for chat %d: %s", chatID, key)
		}
	}

	return key
}

// SetSessionKey changes the override session key (used for multiball fork/done).
// If OnSessionKeyChange is set, fires it outside the lock with the bot's username and new key.
func (b *Bot) SetSessionKey(key string) {
	b.sessionMu.Lock()
	cb := b.OnSessionKeyChange
	b.sessionKey = key
	b.sessionMu.Unlock()

	if cb != nil {
		cb(b.Username(), key)
	}
}

// SetSessionKeyDirect sets the session key without firing the OnSessionKeyChange callback.
// Used during restoration to avoid re-persisting an already-persisted value.
func (b *Bot) SetSessionKeyDirect(key string) {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	b.sessionKey = key
}

// DefaultChatID returns the default chat ID for this agent (used for proactive messages).
func (b *Bot) DefaultChatID() int64 {
	return b.defaultChatID()
}

// defaultChatID reads the default chat from the state store.
func (b *Bot) defaultChatID() int64 {
	if b.stateStore == nil || b.agentID == "" {
		return 0
	}
	var chatID int64
	if b.stateStore.Get("agent:"+b.agentID+":default_chat", &chatID) {
		return chatID
	}
	return 0
}

// setDefaultChat persists the default chat ID for this agent.
func (b *Bot) setDefaultChat(chatID int64) {
	if b.stateStore == nil || b.agentID == "" {
		return
	}
	if err := b.stateStore.Set("agent:"+b.agentID+":default_chat", chatID); err != nil {
		b.logger().Errorf("persist default chat: %v", err)
	}
}

// recordChatUsername persists the username for a chat ID (for /sessions display).
func (b *Bot) recordChatUsername(chatID int64, username string) {
	if b.stateStore == nil || b.agentID == "" || username == "" {
		return
	}
	key := fmt.Sprintf("agent:%s:chat:%d:username", b.agentID, chatID)
	if err := b.stateStore.Set(key, username); err != nil {
		b.logger().Errorf("persist chat username: %v", err)
	}
}

// DefaultSessionKey returns the session key for the default chat.
// Used by keepalive, cron, and other proactive features.
func (b *Bot) DefaultSessionKey() string {
	if b.agentID == "" {
		return ""
	}
	chatID := b.defaultChatID()
	if chatID == 0 {
		return ""
	}
	return b.sessionKeyForMsg(chatID)
}

// Username returns the bot's Telegram username.
// Returns "" if the API client is not set (e.g. test bots).
func (b *Bot) Username() string {
	if b.api == nil {
		return ""
	}
	return b.api.Username
}

// ChatID returns the last known chat ID.
func (b *Bot) ChatID() int64 {
	b.chatMu.Lock()
	defer b.chatMu.Unlock()
	return b.chatID
}

// SetChatID sets the chat ID (used for multiball notification delivery).
func (b *Bot) SetChatID(id int64) {
	b.chatMu.Lock()
	b.chatID = id
	b.chatMu.Unlock()
}

// RegisterCommands registers the bot's slash commands with Telegram via setMyCommands.
// This makes commands appear as autocomplete suggestions when the user types "/" in chat.
// Logs a warning on failure but does not return an error.
func (b *Bot) RegisterCommands() {
	var cmds []gotgbot.BotCommand

	// Add all registered commands from the registry (skip hidden)
	for _, cmd := range b.commands.All() {
		if cmd.Hidden {
			continue
		}
		desc := cmd.Description
		if desc == "" {
			desc = cmd.Name
		}
		cmds = append(cmds, gotgbot.BotCommand{
			Command:     cmd.Name,
			Description: desc,
		})
	}

	// Add special commands not in the registry
	cmds = append(cmds, gotgbot.BotCommand{Command: "stop", Description: "Cancel the current agent turn"})
	cmds = append(cmds, gotgbot.BotCommand{Command: "done", Description: "Detach a secondary bot from its session"})

	if _, err := b.client.SetMyCommands(cmds, nil); err != nil {
		b.logger().Warnf("setMyCommands: %s", b.sanitizeError(err))
		return
	}
	b.logger().Infof("registered %d commands with BotFather", len(cmds))
}

// Run starts the receiver and agent worker goroutines. Blocks until ctx is cancelled.
// If polling fails, it recovers and retries with backoff.
func (b *Bot) Run(ctx context.Context) {
	b.logger().Infof("bot started as @%s", b.api.Username)

	b.RegisterCommands()

	// Agent worker — processes queued messages sequentially
	go b.agentWorker(ctx)

	for ctx.Err() == nil {
		b.pollUpdates(ctx)

		if ctx.Err() != nil {
			return
		}

		b.logger().Warnf("polling interrupted, restarting in 5s...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// pollUpdates runs the telegram update polling loop. Returns if a panic
// occurs or ctx is cancelled. Caller should retry on return.
func (b *Bot) pollUpdates(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger().Errorf("panic in polling: %v\n%s", r, debug.Stack())
		}
	}()

	type updateResult struct {
		updates []gotgbot.Update
		err     error
	}

	var offset int64
	var consecutiveErrors int
	const errorEscalateThreshold = 5 // escalate to ERROR after this many consecutive failures

	// On exit, acknowledge processed updates so they aren't replayed on restart.
	// Telegram acknowledges updates implicitly when the next getUpdates has a
	// higher offset, so we must fire one final short-poll before returning.
	defer func() {
		if offset > 0 {
			_, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:         offset,
				Timeout:        0,
				AllowedUpdates: []string{"message", "callback_query"},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 5 * time.Second,
				},
			})
			if err != nil {
				b.logger().Errorf("failed to ack updates on shutdown: %s", b.sanitizeError(err))
			} else {
				b.logger().Infof("acknowledged updates up to offset %d", offset)
			}
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}

		// Poll in a goroutine so we can select on ctx.Done()
		ch := make(chan updateResult, 1)
		go func() {
			updates, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:         offset,
				Timeout:        60,
				AllowedUpdates: []string{"message", "callback_query"},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 65 * time.Second, // must exceed Telegram's long-poll timeout
				},
			})
			ch <- updateResult{updates, err}
		}()

		select {
		case <-ctx.Done():
			return
		case res := <-ch:
			if res.err != nil {
				consecutiveErrors++
				sanitized := b.sanitizeError(res.err)
				if consecutiveErrors >= errorEscalateThreshold {
					b.logger().Errorf("get updates (%d consecutive failures): %s", consecutiveErrors, sanitized)
				} else {
					b.logger().Debugf("get updates (transient): %s", sanitized)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
				continue
			}
			consecutiveErrors = 0

			for _, update := range res.updates {
				if update.UpdateId >= offset {
					offset = update.UpdateId + 1
				}
				if update.CallbackQuery != nil {
					b.handleCallbackQuery(ctx, update.CallbackQuery)
					continue
				}
				if update.Message == nil {
					continue
				}
				b.receiveMessage(ctx, update.Message)
			}
		}
	}
}

// isStopCommand returns true if the command matches /stop or any configured alias.
func (b *Bot) isStopCommand(cmd string) bool {
	if cmd == "/stop" {
		return true
	}
	if !b.enableStopAliases {
		return false
	}
	for _, alias := range b.stopAliases {
		if cmd == "/"+alias {
			return true
		}
	}
	return false
}

func formatUserInfo(user *gotgbot.User) string {
	id := fmt.Sprintf("%d", user.Id)
	if user.Username != "" {
		return fmt.Sprintf("%s (%s)", id, user.Username)
	}
	if user.FirstName != "" {
		return fmt.Sprintf("%s (%s)", id, user.FirstName)
	}
	return id
}

func (b *Bot) receiveMessage(ctx context.Context, msg *gotgbot.Message) {
	userID := fmt.Sprintf("%d", msg.From.Id)

	if !b.allowedUsers[userID] {
		b.logger().Warnf("rejected message from %s", formatUserInfo(msg.From))
		return
	}

	// Remember chat ID for notifications (cache bust alerts, etc.)
	b.chatMu.Lock()
	changed := b.chatID != msg.Chat.Id
	b.chatID = msg.Chat.Id
	b.chatMu.Unlock()

	if changed && b.stateStore != nil {
		if err := b.stateStore.Set(b.stateKey+":chatid", msg.Chat.Id); err != nil {
			b.logger().Errorf("persist chat ID: %v", err)
		}
	}

	// Per-chat session routing: set default chat on first message, record username
	if !b.isSecondary && b.agentID != "" {
		if b.defaultChatID() == 0 {
			b.setDefaultChat(msg.Chat.Id)
			b.logger().Infof("set default chat %d for agent %s", msg.Chat.Id, b.agentID)
		}
		if msg.From != nil {
			b.recordChatUsername(msg.Chat.Id, msg.From.Username)
		}
	}

	// Record last real user activity (for --if-active gating on CLI commands).
	// Only primary bots track this — secondary (multiball) bots don't count.
	if !b.isSecondary && b.agentID != "" && b.stateStore != nil {
		_ = b.stateStore.Set("agent:"+b.agentID+":last_user_activity", time.Now().Unix())
	}
	if b.OnUserMessage != nil {
		b.OnUserMessage()
	}

	// Get text from message or caption (photos use caption)
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// Include quoted message context when user replies to a specific message.
	// Prefer the specific quote text (user highlighted a portion) over the
	// full replied-to message, which may be very long.
	if msg.Quote != nil && msg.Quote.Text != "" {
		text = fmt.Sprintf("[Quoting: %s]\n\n%s", msg.Quote.Text, text)
	} else if msg.ReplyToMessage != nil {
		quoted := msg.ReplyToMessage.Text
		if quoted == "" {
			quoted = msg.ReplyToMessage.Caption
		}
		if quoted != "" {
			text = fmt.Sprintf("[Replying to: %s]\n\n%s", quoted, text)
		}
	}

	// Handle voice notes: download, transcribe, tag with [voice]
	if msg.Voice != nil && b.transcriber != nil {
		if data, err := b.downloadFile(msg.Voice.FileId); err != nil {
			b.logger().Errorf("download voice: %s", b.sanitizeError(err))
			if b.handler == nil || b.handler.Warnings() == nil {
				b.sendReply(msg, userID, "Could not download voice note — please try again.")
			}
		} else {
			transcript, err := b.transcriber.Transcribe(ctx, data, "voice.ogg")
			if err != nil {
				b.logger().Errorf("transcribe voice: %v", err)
				b.sendReply(msg, userID, "Could not transcribe voice note.")
				return
			}
			b.logger().Infof("voice transcription from %s: %s", formatUserInfo(msg.From), truncate(transcript, 100))
			text = "[voice] " + transcript
		}
	} else if msg.Voice != nil && b.transcriber == nil {
		b.sendReply(msg, userID, "Voice notes require an STT provider. Set groq.api_key in secrets.toml or configure [voice] stt_endpoint.")
		return
	}

	// Download images/PDFs from photos or documents
	var images []attachment
	if len(msg.Photo) > 0 {
		// Take the largest photo (last in the array)
		photo := msg.Photo[len(msg.Photo)-1]
		if att, ok := b.downloadAttachment(photo.FileId, "image/jpeg", msg.Chat.Id); ok {
			images = append(images, att)
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id); ok {
			images = append(images, att)
		}
	} else if msg.Document != nil && isPDFMIME(msg.Document.MimeType) {
		// PDFs under 32MB go through the content-block path (like images);
		// over-size PDFs fall back to save-to-disk via handleMediaMessage.
		const maxPDFSize = 32 * 1024 * 1024
		if msg.Document.FileSize > 0 && msg.Document.FileSize > maxPDFSize {
			text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "PDF", msg.Chat.Id, ".pdf")
		} else if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id); ok {
			if len(att.data) > maxPDFSize {
				// Downloaded size exceeded limit — save to disk instead
				if att.savedPath != "" {
					text = fmt.Sprintf("[PDF saved to: %s]\n\n%s", att.savedPath, text)
				}
			} else {
				images = append(images, att)
			}
		}
	}

	// Handle video messages
	if msg.Video != nil {
		text = b.handleMediaMessage(text, msg.Video.FileId, msg.Video.FileSize, "video", "Video", msg.Chat.Id, extForVideo(msg.Video.MimeType))
	}

	// Handle video notes (circular video messages)
	if msg.VideoNote != nil {
		text = b.handleMediaMessage(text, msg.VideoNote.FileId, msg.VideoNote.FileSize, "videonote", "Video", msg.Chat.Id, ".mp4")
	}

	// Handle non-image, non-PDF document attachments
	if msg.Document != nil && !isImageMIME(msg.Document.MimeType) && !isPDFMIME(msg.Document.MimeType) {
		ext := filepath.Ext(msg.Document.FileName)
		if ext == "" {
			ext = extForMIME(msg.Document.MimeType)
		}
		text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "Document", msg.Chat.Id, ext)
	}

	// Drop messages with no text and no images
	if text == "" && len(images) == 0 {
		return
	}

	logText := text
	if len(images) > 0 {
		logText = fmt.Sprintf("[%d image(s)] %s", len(images), text)
	}
	if b.messagesInLog {
		b.logger().Infof("message from %s: %s", formatUserInfo(msg.From), truncate(logText, 100))
	} else {
		b.logger().Debugf("message from %s", formatUserInfo(msg.From))
	}

	// Log received message — use per-chat session key for primary bots
	recvSession := b.SessionKey()
	if !b.isSecondary && b.agentID != "" {
		recvSession = b.sessionKeyForMsg(msg.Chat.Id)
	}
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    userID,
		Username:  msg.From.Username,
		ChatID:    msg.Chat.Id,
		Text:      logText,
		Session:   recvSession,
	})

	// Wizard intercept — route all messages to active wizard before normal dispatch
	if text != "" {
		if result, ok := b.commands.HandleMessage(text); ok {
			b.sendReply(msg, userID, result)
			return
		}
	}

	// Record the message for // (repeat) command
	if text != "" && !strings.HasPrefix(text, "/") {
		b.lastMsgStore.Record(userID, text)
	}

	// Drop stale slash commands (e.g. /restart replayed from the update
	// queue after a crash). Agent messages are still delivered since the
	// agent can reason about timeliness, but slash commands execute
	// unconditionally so stale ones must be dropped.
	if text != "" && strings.HasPrefix(text, "/") {
		if age := time.Since(time.Unix(int64(msg.Date), 0)); age > 30*time.Second {
			b.logger().Warnf("dropping stale command %q (age=%s)", strings.ToLower(text), age.Truncate(time.Second))
			return
		}
	}

	// Try dispatching the original message as a command (slash or dot-prefix).
	if b.tryDispatchCommand(ctx, msg, userID, text) {
		return
	}

	// Apply message transforms to non-command messages.
	// Transforms may produce a command (e.g. "m" → "/mana").
	if b.handler != nil {
		if transformed := b.handler.TransformMessage(text); transformed != text {
			text = transformed
			if b.tryDispatchCommand(ctx, msg, userID, text) {
				return
			}
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	// Replying would cause spurious "idle" messages on restart when stale
	// Telegram updates are replayed.
	if b.isSecondary && b.SessionKey() == "" {
		b.logger().Debugf("dropping message to idle secondary bot from %s", formatUserInfo(msg.From))
		return
	}

	// Steer mode: if a turn is active, route text to the steer buffer
	// so it gets injected between tool calls instead of queuing behind the turn lock.
	if b.steerMode && text != "" && len(images) == 0 {
		b.turnMu.Lock()
		active := b.turnCancel != nil
		b.turnMu.Unlock()
		if active {
			b.appendSteer(text)
			b.logger().Infof("steer: buffered message from %s", formatUserInfo(msg.From))
			return
		}
	}

	select {
	case b.queue <- queuedMessage{msg: msg, userID: userID, text: text, images: images}:
	default:
		b.logger().Warnf("message queue full, dropping message from %s", formatUserInfo(msg.From))
		b.sendReply(msg, userID, "Busy — message queue is full. Try again shortly.")
	}
}

// tryDispatchCommand tries to dispatch text as a slash or dot-command.
// Returns true if the message was handled (caller should return).
func (b *Bot) tryDispatchCommand(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
	if text == "" {
		return false
	}

	// /stop and /done are handled locally (not dispatched to command registry)
	if strings.HasPrefix(text, "/") {
		cmd := strings.ToLower(strings.TrimSpace(text))
		if b.isStopCommand(cmd) {
			b.cancelTurn()
			b.sendReply(msg, userID, "Stopped.")
			return true
		}
		if cmd == "/done" {
			return b.handleDoneCommand(msg, userID)
		}
	}

	// Try platform-aware dispatcher first (DispatchV2 path)
	if b.dispatcher != nil {
		return b.tryDispatchV2(ctx, msg, userID, text)
	}

	// Legacy dispatch path (original Dispatch method)
	return b.tryDispatchLegacy(ctx, msg, userID, text)
}

// tryDispatchV2 uses the platform-aware dispatcher (DispatchV2).
func (b *Bot) tryDispatchV2(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
	result := b.dispatcher.Dispatch(ctx, msg)
	if !result.Handled {
		return false
	}

	// Check for keyboard display
	if name, opts, ok := b.dispatcher.LookupKeyboard(ctx, text); ok {
		b.sendCommandKeyboard(msg.Chat.Id, name, opts)
		return true
	}

	if result.Response.Text != "" {
		b.sendReply(msg, userID, result.Response.Text)
	}
	if result.Response.DocPath != "" {
		_ = b.SendDocument(result.Response.DocPath)
	}
	return true
}

// tryDispatchLegacy uses the original command.Dispatch method.
func (b *Bot) tryDispatchLegacy(ctx context.Context, msg *gotgbot.Message, userID, text string) bool {
	// Dot-command alias (.xxx → /xxx) — easier to type on phone keyboards.
	// Only treated as a command if it matches a registered command.
	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		dotText := strings.TrimSpace(text)[1:] // strip leading dot, preserve case
		cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")
		if b.commands.Get(cmdName) != nil || b.isStopCommand("/"+cmdName) {
			dotCmd := "/" + dotText
			cmdCtx := b.commandContext(ctx, userID, msg.Chat.Id)
			if _, opts, ok := b.commands.LookupKeyboard(cmdCtx, dotCmd); ok {
				b.sendCommandKeyboard(msg.Chat.Id, cmdName, opts)
				return true
			}
			if result, ok := b.commands.Dispatch(cmdCtx, dotCmd); ok {
				b.logger().Debugf("command %s → %s dispatched", text, dotCmd)
				b.sendReply(msg, userID, result)
				return true
			}
		}
	}

	// Slash commands
	if strings.HasPrefix(text, "/") {
		cmdCtx := b.commandContext(ctx, userID, msg.Chat.Id)

		// Check for inline keyboard (bare command, no args)
		if name, opts, ok := b.commands.LookupKeyboard(cmdCtx, text); ok {
			b.logger().Debugf("command /%s showing keyboard (%d options)", name, len(opts))
			b.sendCommandKeyboard(msg.Chat.Id, name, opts)
			return true
		}

		if result, ok := b.commands.Dispatch(cmdCtx, text); ok {
			b.logger().Debugf("command %s dispatched", text)
			b.sendReply(msg, userID, result)
			return true
		}
	}

	return false
}

// handleDoneCommand handles the /done command for secondary bots.
func (b *Bot) handleDoneCommand(msg *gotgbot.Message, userID string) bool {
	if !b.isSecondary {
		b.sendReply(msg, userID, "Nothing to detach — this is the main session.")
		return true
	}
	sk := b.SessionKey()
	if sk == "" {
		b.sendReply(msg, userID, "Already idle.")
		return true
	}
	b.cancelTurn()
	if b.pool != nil {
		b.pool.Release(b)
	}
	b.sendReply(msg, userID, "Session ended.")
	b.logger().Infof("secondary bot detached from %s", sk)
	return true
}

// commandContext creates a context with metadata for command dispatch.
func (b *Bot) commandContext(ctx context.Context, userID string, chatID int64) context.Context {
	ctx = context.WithValue(ctx, command.LastMessageUserKey{}, userID)
	ctx = context.WithValue(ctx, command.ChatIDKey{}, chatID)
	return ctx
}

// agentWorker processes queued messages one at a time.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.queue:
			b.processAgentMessage(ctx, qm)
			// Drain steer messages that arrived after the turn completed.
			// Process them as normal follow-up turns so the user's redirection
			// isn't silently dropped.
			if orphan := b.drainSteer(); orphan != "" {
				b.logger().Infof("steer: processing orphaned steer message as follow-up turn")
				b.processAgentMessage(ctx, queuedMessage{msg: qm.msg, userID: qm.userID, text: orphan})
			}
		}
	}
}

// processAgentMessage handles a single agent turn with a cancellable context.
func (b *Bot) processAgentMessage(ctx context.Context, qm queuedMessage) {
	var sk string
	if b.isSecondary {
		// Secondary bots use their override session key
		sk = b.SessionKey()
	} else if b.agentID != "" {
		// Primary bots derive session key from the message's chat ID
		sk = b.sessionKeyForMsg(qm.msg.Chat.Id)
	} else {
		sk = b.SessionKey()
	}
	if sk == "" {
		return // no session assigned (idle secondary bot)
	}

	// Create a cancellable context for this turn
	turnCtx, cancel := context.WithCancel(ctx)

	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	defer func() {
		b.turnMu.Lock()
		b.turnCancel = nil
		b.turnMu.Unlock()
		cancel()
	}()

	// Send typing indicator and keep it alive throughout the agent turn.
	// Telegram typing expires after ~5s, so we re-send every 4s.
	_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
	typingTicker := time.NewTicker(4 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
			case <-turnCtx.Done():
				return
			}
		}
	}()
	defer typingTicker.Stop()

	// Track tool calls for live visibility via send+edit pattern.
	tracker := &toolCallTracker{bot: b, chatID: qm.msg.Chat.Id}

	// Stream writer: real-time message updates during streaming.
	var sw *streamWriter
	if b.streamOutput {
		interval := b.streamUpdateInterval
		if interval == 0 {
			interval = 250 * time.Millisecond
		}
		sw = newStreamWriter(b.client, qm.msg.Chat.Id, interval)
	}

	// Accumulate thinking blocks for the turn
	var thinkingBuf strings.Builder

	// Per-turn callbacks scoped to context -- no cross-turn races.
	cb := &agent.TurnCallbacks{
		// Intermediate reply delivery (deferred replies).
		// When intermediate text fires, reset the tracker so the next tool call
		// creates a fresh message below the text instead of editing the stale
		// earlier message (which would appear above the text in chat).
		ReplyFunc: func(text string) {
			b.sendReply(qm.msg, qm.userID, text)
			tracker.resetMsgID()
		},
		// Refresh typing indicator when tools complete
		ActivityFunc: func() {
			_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
		},
		ToolCallObserver:   tracker.observeToolCall,
		ToolResultObserver: tracker.observeToolResult,
		// Thinking block accumulator (gated by showThinking)
		ThinkingObserver: func(thinking string) {
			if b.showThinking == "off" || b.showThinking == "" {
				return
			}
			if thinkingBuf.Len() > 0 {
				thinkingBuf.WriteString("\n")
			}
			thinkingBuf.WriteString(thinking)
		},
		// Streaming delta callbacks: update stream writer + refresh typing indicator.
		TextDeltaObserver: func(delta string) {
			if sw != nil {
				sw.OnDelta(delta)
			}
			_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
		},
		// Steer: drain buffered user messages for injection between tool calls.
		SteerCheckFunc: b.drainSteer,
		// Retry notifications: inform user when API is retrying.
		RetryNotifyFunc:  tracker.notifyRetry,
		RetrySuccessFunc: tracker.clearRetryNotification,
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "telegram")

	var response string
	var err error
	if len(qm.images) > 0 {
		// Convert telegram images to platform image data
		platformImages := make([]platform.Attachment, len(qm.images))
		for i, img := range qm.images {
			platformImages[i] = platform.Attachment{MimeType: img.mediaType, Data: img.data, SavedPath: img.savedPath}
		}
		response, err = b.handler.HandleMessageWithAttachments(turnCtx, sk, qm.text, platformImages)
	} else {
		response, err = b.handler.HandleMessage(turnCtx, sk, qm.text)
	}
	if err != nil {
		if turnCtx.Err() != nil {
			b.logger().Infof("agent turn cancelled")
			return // /stop was called, "Stopped." already sent
		}
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
		response = fmt.Sprintf("Error: %s", b.sanitizeError(err))
	}
	if b.OnTurnComplete != nil {
		b.OnTurnComplete()
	}

	// Finish the stream writer and get the message ID it created (if any).
	var streamMsgID int64
	if sw != nil {
		streamMsgID = sw.Finish()
	}

	// Guard against empty responses (e.g. end_turn after tool use with no text).
	// Sending an empty string to Telegram would fail with "message text is empty".
	if strings.TrimSpace(response) == "" {
		b.logger().Debugf("agent returned empty response for %s, not sending", sk)
		return
	}

	// Show thinking: prepend thinking to response ("true" mode) or attach
	// a toggle button ("compact" mode).
	thinkingText := thinkingBuf.String()

	// Determine which message to edit with the final response.
	// The stream message takes priority (it's what the user sees as "in-progress"),
	// then the tool call preview message.
	hasThinking := thinkingText != "" && b.showThinking != "off" && b.showThinking != ""

	editID := tracker.lastMsgID()
	if streamMsgID != 0 {
		editID = streamMsgID
	}

	// Try to edit an existing message with the final HTML-formatted response.
	// Works for both stream messages and tool call preview messages.
	// Skip when thinking will be shown — thinking-aware send functions handle the full reply.
	var canEditInPlace bool
	if streamMsgID != 0 {
		// Stream messages can always be edited (no show_tool_calls mode gate).
		canEditInPlace = editID != 0 && !hasThinking && len(response) <= 4096
	} else {
		// Tool call preview: only edit in preview mode.
		canEditInPlace = editID != 0 && b.showToolCalls == "preview" && !hasThinking && len(response) <= 4096
	}

	if canEditInPlace {
		htmlResp := ConvertToTelegramHTML(response, b.tableOpts())
		_, _, editErr := b.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
			ChatId:    qm.msg.Chat.Id,
			MessageId: editID,
			ParseMode: "HTML",
		})
		if editErr == nil {
			log.Conversation(log.ConversationEntry{
				Direction: "sent",
				UserID:    qm.userID,
				Username:  qm.msg.From.Username,
				ChatID:    qm.msg.Chat.Id,
				Text:      htmlResp,
				ParseMode: "HTML",
				Session:   b.SessionKey(),
			})
			return
		}
		b.logger().Debugf("edit final response failed, falling back: %v", editErr)
	}

	// Full thinking: prepend italic thinking + divider to response
	if b.showThinking == "true" && thinkingText != "" {
		b.sendReplyWithFullThinking(qm.msg, qm.userID, response, thinkingText)
		b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
		return
	}

	// Compact thinking: send response with "Show thinking" toggle button
	if b.showThinking == "compact" && thinkingText != "" {
		b.sendReplyWithThinking(qm.msg, qm.userID, response, thinkingText)
		b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
		return
	}

	b.sendReply(qm.msg, qm.userID, response)
	b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
}

// editStreamPreview edits the stream message to a truncated preview when the
// final response was sent as a separate message (too long, has thinking, etc.).
func (b *Bot) editStreamPreview(streamMsgID, chatID int64, response string) {
	if streamMsgID == 0 {
		return
	}
	preview := truncate(response, 200)
	_, _, _ = b.client.EditMessageText(
		htmlEscapeBot(preview)+"\n\n<i>(full response below)</i>",
		&gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: streamMsgID,
			ParseMode: "HTML",
		})
}

// cancelTurn cancels the in-flight agent turn, if any.
func (b *Bot) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnCancel != nil {
		b.logger().Infof("cancelling agent turn via /stop")
		b.turnCancel()
	}
}

// sendHTMLChunks sends pre-converted HTML to a chat, splitting into chunks,
// falling back to plain text if HTML parsing fails, and logging each chunk.
func (b *Bot) sendHTMLChunks(chatID int64, html, userID, username string) {
	for _, chunk := range splitMessage(html, 4096) {
		parseMode := "HTML"
		sendErr := ""
		if _, err := b.client.SendMessage(chatID, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			parseMode = ""
			if _, err := b.client.SendMessage(chatID, chunk, nil); err != nil {
				b.logger().Errorf("send error: %s", b.sanitizeError(err))
				sendErr = err.Error()
			}
		}

		log.Conversation(log.ConversationEntry{
			Direction: "sent",
			UserID:    userID,
			Username:  username,
			ChatID:    chatID,
			Text:      chunk,
			ParseMode: parseMode,
			Session:   b.SessionKey(),
			Error:     sendErr,
		})
	}
}

// sendReply sends a response back to the user, splitting long messages and
// falling back to plain text if HTML formatting fails.
// Logs each chunk to the conversation log.
func (b *Bot) sendReply(msg *gotgbot.Message, userID string, response string) {
	parts := []string{response}
	if strings.Contains(response, "\x00") {
		parts = strings.Split(response, "\x00")
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b.sendHTMLChunks(msg.Chat.Id, ConvertToTelegramHTML(part, b.tableOpts()), userID, msg.From.Username)
	}
}

// sendReplyWithFullThinking sends thinking (italic) + divider + response as a single message.
// Thinking and response are converted to HTML separately to avoid markdown interference.
func (b *Bot) sendReplyWithFullThinking(msg *gotgbot.Message, userID string, response, thinkingText string) {
	thinkingHTML := "<i>" + htmlEscapeBot(thinkingText) + "</i>"
	responseHTML := ConvertToTelegramHTML(response, b.tableOpts())
	divider := "\n" + strings.Repeat("—", b.displayWidth) + "\n\n"
	b.sendHTMLChunks(msg.Chat.Id, thinkingHTML+divider+responseHTML, userID, msg.From.Username)
}

// sendReplyWithThinking sends a response with a "Show thinking" inline keyboard button.
// The thinking content is stored for later toggle via callback query.
func (b *Bot) sendReplyWithThinking(msg *gotgbot.Message, userID string, response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, b.tableOpts())

	// Send with placeholder button (msgID unknown until sent)
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: singleButtonKeyboard("Show thinking", "th:show:0"),
	}

	// If response is too long to fit with a button, chunk it — send all but last
	// chunk as plain, then send last chunk with button.
	chunks := splitMessage(responseHTML, 4096)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			// Non-last chunks: use sendHTMLChunks for fallback + logging
			b.sendHTMLChunks(msg.Chat.Id, chunk, userID, msg.From.Username)
			continue
		}
		// Last chunk — send with button
		sent, err := b.client.SendMessage(msg.Chat.Id, chunk, sendOpts)
		if err != nil {
			b.logger().Errorf("send reply with thinking button: %v", err)
			return
		}
		// Update button with real message ID and store thinking data
		kb := singleButtonKeyboard("Show thinking", fmt.Sprintf("th:show:%d", sent.MessageId))
		_, _, _ = b.client.EditMessageText(chunk, &gotgbot.EditMessageTextOpts{
			ChatId:      msg.Chat.Id,
			MessageId:   sent.MessageId,
			ParseMode:   "HTML",
			ReplyMarkup: kb,
		})
		b.thinkingStore.Store(sent.MessageId, thinkingEntry{
			responseHTML: chunk,
			thinkingText: thinkingText,
		})
		log.Conversation(log.ConversationEntry{
			Direction: "sent",
			UserID:    userID,
			Username:  msg.From.Username,
			ChatID:    msg.Chat.Id,
			Text:      chunk,
			ParseMode: "HTML",
			Session:   b.SessionKey(),
		})
	}
}

// SendNotification sends a plain text notification to the default chat.
// Used for system alerts (cache bust, etc.) — not an agent turn, no tokens spent.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendNotification(text string) {
	if strings.TrimSpace(text) == "" {
		b.logger().Debugf("skipping empty notification")
		return
	}

	chatID := b.defaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		b.logger().Warnf("no chat ID for notification: %s", text)
		return
	}

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		b.logger().Errorf("send notification: %s", b.sanitizeError(err))
	}
}

// SendStartupNotification sends a startup notification to the last known chat.
// Skips silently if no chat ID is available (expected on first run or fresh state).
func (b *Bot) SendStartupNotification(agentID string) {
	b.SendStartupNotificationWithDiagnosis(agentID, nil)
}

// StartupDiagnosis is an interface for the diagnosis result from the startup package.
// Using an interface avoids importing the startup package (would create a cycle).
type StartupDiagnosis interface {
	FormatNotification() string
}

// SendStartupNotificationWithDiagnosis sends a startup notification with optional diagnosis info.
// If diagnosis is nil or produces no additional text, sends a simple restart message.
func (b *Bot) SendStartupNotificationWithDiagnosis(agentID string, diagnosis StartupDiagnosis) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		b.logger().Debugf("no chat ID for startup notification (no prior messages)")
		return
	}

	botName := b.Username()
	if botName == "" {
		botName = "foci"
	}
	text := fmt.Sprintf("%s restarted at %s", botName, time.Now().Format("15:04:05"))

	if diagnosis != nil {
		if extra := diagnosis.FormatNotification(); extra != "" {
			text = fmt.Sprintf("%s\n\n%s", text, extra)
		}
	}

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		b.logger().Errorf("send startup notification: %s", b.sanitizeError(err))
	}
}

// SendText sends a text message to the default chat without any header.
// Returns an error if no chat ID is available.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendText(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatID := b.defaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID — no default chat configured")
	}

	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()), "", "")
	return nil
}

// SendInjected sends a system/injected text message to the default chat.
// Prepends the configured InjectedMessageHeader (if non-empty) so users can
// distinguish system messages from agent replies.
//
// Prefer SendToSession when a session key is available — it routes to the
// correct chat for chat-based sessions.
func (b *Bot) SendInjected(text string) error {
	if b.injectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.injectedMessageHeader + "\n" + text
	}
	return b.SendText(text)
}

// SendToSession sends a system/injected text message to the chat associated
// with the given session key. Falls back to the bot's default chat if the
// session key doesn't contain a chat ID (e.g. independent sessions).
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendToSession(sessionKey, text string) error {
	if b.injectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.injectedMessageHeader + "\n" + text
	}
	return b.SendTextToSession(sessionKey, text)
}

// SendTextToSession sends a text message (without header) to the chat
// associated with the given session key. Falls back to the bot's default chat
// if the session key doesn't contain a chat ID.
func (b *Bot) SendTextToSession(sessionKey, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.defaultChatID()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID for session %q and no default chat", sessionKey)
	}

	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()), "", "")
	return nil
}

// SendDocument sends a file as a Telegram document to the last known chat.
