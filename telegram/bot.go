package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/agent"
	"foci/command"
	"foci/log"
	"foci/state"
	"foci/voice"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

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

// imageAttachment is a downloaded image ready for the agent.
type imageAttachment struct {
	data      []byte
	mediaType string
	savedPath string // non-empty if saved to disk
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg    *gotgbot.Message
	userID string
	text   string
	images []imageAttachment
}

// Bot wraps the Telegram bot API with agent integration.
// Messages are received on one goroutine and processed on another.
// Slash commands execute immediately on the receiver goroutine.
type Bot struct {
	log                *log.ComponentLogger
	api                *gotgbot.Bot // for receiving updates (Run)
	client             botClient    // for sending messages and files (mockable in tests)
	agent              *agent.Agent
	commands           *command.Registry
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

	stateStore           *state.Store // nil = no persistence
	stateKey             string       // state key prefix (e.g. "bot:mybot")
	toolCallPreviewChars int          // max chars for tool call preview (default 450)
	showToolCalls        string       // tool call display mode: "off", "preview", "full"
	showThinking         string       // thinking display mode: "off", "compact", "true"
	displayWidth         int          // character width for dividers (default 44)
	messagesInLog        bool         // log user message content to event log (default false for privacy)
	receivedFilesDir     string       // if non-empty, save received files to this directory
	toolResults          sync.Map            // message ID (int64) → toolResultEntry; for inline keyboard expansion
	thinkingStore        sync.Map            // message ID (int64) → thinkingEntry; ephemeral, for inline keyboard expansion
	toolDetailStore      *ToolDetailStore    // nil = no persistence; write-through to SQLite
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
func NewBot(token string, allowedUsers []string, ag *agent.Agent, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) (*Bot, error) {
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
		log:          log.NewComponentLogger("telegram:" + agentID),
		api:          api,
		client:       api,
		agent:        ag,
		commands:     cmds,
		lastMsgStore: lastMsgStore,
		allowedUsers: allowed,
		agentID:      agentID,
		botToken:     token,
		queue:        make(chan queuedMessage, 64),
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

// SetMessagesInLog controls whether user message content is logged to the event log.
func (b *Bot) SetMessagesInLog(v bool) {
	b.messagesInLog = v
}

// SetReceivedFilesDir configures auto-saving of received files to disk.
// Empty string disables saving.
func (b *Bot) SetReceivedFilesDir(dir string) {
	b.receivedFilesDir = dir
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

// DisplaySettings returns the current display settings for inspection/testing.
func (b *Bot) DisplaySettings() (showToolCalls, showThinking string, displayWidth int, messagesInLog bool, receivedFilesDir string) {
	return b.showToolCalls, b.showThinking, b.displayWidth, b.messagesInLog, b.receivedFilesDir
}

// NewBotForTest creates a Bot without connecting to the Telegram API.
// For use in tests outside the telegram package.
func NewBotForTest() *Bot {
	return &Bot{
		log:   log.NewComponentLogger("telegram:test"),
		queue: make(chan queuedMessage, 64),
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

// SetSecondary marks this bot as a secondary bot in the given pool.
func (b *Bot) SetSecondary(pool *Pool) {
	b.isSecondary = true
	b.pool = pool
}

// SetAgentAndCommands re-wires the bot to a different agent and command registry.
// Only safe to call on idle secondary bots (no active session key) between
// pool acquisition and setting the session key.
func (b *Bot) SetAgentAndCommands(ag *agent.Agent, cmds *command.Registry) {
	b.agent = ag
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
			return SessionKeyForChat(b.agentID, chatID)
		}
	}
	return ""
}

// SessionKeyForChat returns the session key for a specific chat ID.
// Format: agent:<agentID>:chat:<chatID>
func SessionKeyForChat(agentID string, chatID int64) string {
	return fmt.Sprintf("agent:%s:chat:%d", agentID, chatID)
}

// sessionKeyForMsg returns the session key for the message's chat.
func (b *Bot) sessionKeyForMsg(chatID int64) string {
	return SessionKeyForChat(b.agentID, chatID)
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
	return SessionKeyForChat(b.agentID, chatID)
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

	// Download images from photos or image documents
	var images []imageAttachment
	if len(msg.Photo) > 0 {
		// Take the largest photo (last in the array)
		photo := msg.Photo[len(msg.Photo)-1]
		if att, ok := b.downloadImage(photo.FileId, "image/jpeg", msg.Chat.Id); ok {
			images = append(images, att)
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if att, ok := b.downloadImage(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id); ok {
			images = append(images, att)
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

	// Handle non-image document attachments
	if msg.Document != nil && !isImageMIME(msg.Document.MimeType) {
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

	// Period prefix (e.g. ".mana") is an alias for slash commands — easier to
	// type on phone keyboards. Only treated as a command if it matches a
	// registered command; otherwise falls through to the agent as normal text.
	if text != "" && strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= 'a' && text[1] <= 'z' {
		dotText := strings.TrimSpace(text)[1:] // strip leading dot, preserve case
		cmdName, _, _ := strings.Cut(strings.ToLower(dotText), " ")
		if b.commands.Get(cmdName) != nil || b.isStopCommand("/"+cmdName) {
			dotCmd := "/" + dotText
			cmdCtx := context.WithValue(ctx, command.LastMessageUserKey{}, userID)
			cmdCtx = context.WithValue(cmdCtx, command.ChatIDKey{}, msg.Chat.Id)
			cmdCtx = context.WithValue(cmdCtx, command.DisplayWidthKey{}, b.displayWidth)
			if _, opts, ok := b.commands.LookupKeyboard(cmdCtx, dotCmd); ok {
				b.sendCommandKeyboard(msg.Chat.Id, cmdName, opts)
				return
			}
			if result, ok := b.commands.Dispatch(cmdCtx, dotCmd); ok {
				b.logger().Debugf("dot-command %s → %s dispatched", text, dotCmd)
				b.sendReply(msg, userID, result)
				return
			}
		}
		// Not a valid command — fall through to agent
	}

	// Slash commands bypass the agent pipeline entirely
	if text != "" && strings.HasPrefix(text, "/") {
		cmd := strings.ToLower(strings.TrimSpace(text))

		// Skip stale slash commands (e.g. /restart replayed from the update
		// queue after a crash). Agent messages are still delivered since the
		// agent can reason about timeliness, but slash commands execute
		// unconditionally so stale ones must be dropped.
		if age := time.Since(time.Unix(int64(msg.Date), 0)); age > 30*time.Second {
			b.logger().Warnf("dropping stale command %q (age=%s)", cmd, age.Truncate(time.Second))
			return
		}

		// /stop cancels the current agent turn (including configured aliases)
		if b.isStopCommand(cmd) {
			b.cancelTurn()
			b.sendReply(msg, userID, "Stopped.")
			return
		}

		// /done detaches a secondary bot from its forked session
		if cmd == "/done" {
			if !b.isSecondary {
				b.sendReply(msg, userID, "Nothing to detach — this is the main session.")
				return
			}
			sk := b.SessionKey()
			if sk == "" {
				b.sendReply(msg, userID, "Already idle.")
				return
			}
			b.cancelTurn()
			if b.pool != nil {
				b.pool.Release(b)
			}
			b.sendReply(msg, userID, "Session ended.")
			b.logger().Infof("secondary bot detached from %s", sk)
			return
		}

		// Pass userID, chatID, and display width to command via context
		cmdCtx := context.WithValue(ctx, command.LastMessageUserKey{}, userID)
		cmdCtx = context.WithValue(cmdCtx, command.ChatIDKey{}, msg.Chat.Id)
		cmdCtx = context.WithValue(cmdCtx, command.DisplayWidthKey{}, b.displayWidth)

		// Check for inline keyboard (bare command, no args)
		if name, opts, ok := b.commands.LookupKeyboard(cmdCtx, text); ok {
			b.logger().Debugf("command /%s showing keyboard (%d options)", name, len(opts))
			b.sendCommandKeyboard(msg.Chat.Id, name, opts)
			return
		}

		if result, ok := b.commands.Dispatch(cmdCtx, text); ok {
			b.logger().Debugf("command %s dispatched", text)
			b.sendReply(msg, userID, result)
			return
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	// Replying would cause spurious "idle" messages on restart when stale
	// Telegram updates are replayed.
	if b.isSecondary && b.SessionKey() == "" {
		b.logger().Debugf("dropping message to idle secondary bot from %s", formatUserInfo(msg.From))
		return
	}

	select {
	case b.queue <- queuedMessage{msg: msg, userID: userID, text: text, images: images}:
	default:
		b.logger().Warnf("message queue full, dropping message from %s", formatUserInfo(msg.From))
		b.sendReply(msg, userID, "Busy — message queue is full. Try again shortly.")
	}
}

// agentWorker processes queued messages one at a time.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.queue:
			b.processAgentMessage(ctx, qm)
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
		// Voice reply delivery (for voice mode)
		VoiceReplyFunc: func(oggData []byte) {
			b.sendVoiceNote(qm.msg.Chat.Id, qm.userID, qm.msg.From.Username, oggData)
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
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "telegram")

	var response string
	var err error
	if len(qm.images) > 0 {
		// Convert telegram images to agent image data
		agentImages := make([]agent.ImageData, len(qm.images))
		for i, img := range qm.images {
			agentImages[i] = agent.ImageData{MediaType: img.mediaType, Data: img.data, SavedPath: img.savedPath}
		}
		response, err = b.agent.HandleMessageWithImages(turnCtx, sk, qm.text, agentImages)
	} else {
		response, err = b.agent.HandleMessage(turnCtx, sk, qm.text)
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

	// Guard against empty responses (e.g. end_turn after tool use with no text).
	// Sending an empty string to Telegram would fail with "message text is empty".
	if strings.TrimSpace(response) == "" {
		b.logger().Debugf("agent returned empty response for %s, not sending", sk)
		return
	}

	// Voice mode: convert final reply to voice note
	if b.agent.VoiceMode(sk) && b.tts != nil && response != "" {
		if audioData, err := b.tts.Synthesize(turnCtx, response); err != nil {
			b.logger().Errorf("tts for voice mode: %v", err)
			b.sendReply(qm.msg, qm.userID, response) // fall back to text
		} else {
			b.sendVoiceNote(qm.msg.Chat.Id, qm.userID, qm.msg.From.Username, audioData)
		}
		return
	}

	// Show thinking: prepend thinking to response ("true" mode) or attach
	// a toggle button ("compact" mode).
	thinkingText := thinkingBuf.String()

	// If we sent a tool-call message, try to edit it with the final response.
	// Fall back to sendReply if the response is too long for a single edit
	// or if the edit fails. Skip when thinking will be shown — thinking-aware
	// send functions handle the full reply (preview edit can't include thinking).
	hasThinking := thinkingText != "" && b.showThinking != "off" && b.showThinking != ""
	editID := tracker.lastMsgID()

	if editID != 0 && b.showToolCalls == "preview" && !hasThinking && len(response) <= 4096 {
		htmlResp := ConvertToTelegramHTML(response)
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
		return
	}

	// Compact thinking: send response with "Show thinking" toggle button
	if b.showThinking == "compact" && thinkingText != "" {
		b.sendReplyWithThinking(qm.msg, qm.userID, response, thinkingText)
		return
	}

	b.sendReply(qm.msg, qm.userID, response)
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

// sendReply sends a response back to the user, splitting long messages and
// falling back to plain text if HTML formatting fails.
// Logs each chunk to the conversation log.
func (b *Bot) sendReply(msg *gotgbot.Message, userID string, response string) {
	// Split multi-message responses (separated by \x00) into individual messages,
	// each converted and chunked independently so code blocks stay intact.
	if !strings.Contains(response, "\x00") {
		b.sendReplyPart(msg, userID, response)
		return
	}
	for _, part := range strings.Split(response, "\x00") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b.sendReplyPart(msg, userID, part)
	}
}

// sendReplyWithFullThinking sends thinking (italic) + divider + response as a single message.
// Thinking and response are converted to HTML separately to avoid markdown interference.
func (b *Bot) sendReplyWithFullThinking(msg *gotgbot.Message, userID string, response, thinkingText string) {
	thinkingHTML := "<i>" + htmlEscapeBot(thinkingText) + "</i>"
	responseHTML := ConvertToTelegramHTML(response)
	divider := "\n" + strings.Repeat("—", b.displayWidth) + "\n\n"
	fullHTML := thinkingHTML + divider + responseHTML

	chunks := splitMessage(fullHTML, 4096)
	for _, chunk := range chunks {
		_, _ = b.client.SendMessage(msg.Chat.Id, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
	}
	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    userID,
		Username:  msg.From.Username,
		ChatID:    msg.Chat.Id,
		Text:      fullHTML,
		ParseMode: "HTML",
		Session:   b.SessionKey(),
	})
}

// sendReplyWithThinking sends a response with a "Show thinking" inline keyboard button.
// The thinking content is stored for later toggle via callback query.
func (b *Bot) sendReplyWithThinking(msg *gotgbot.Message, userID string, response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response)

	// Send with placeholder button (msgID unknown until sent)
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
				{Text: "Show thinking", CallbackData: "th:show:0"},
			}},
		},
	}

	// If response is too long to fit with a button, chunk it — send all but last
	// chunk as plain, then send last chunk with button.
	chunks := splitMessage(responseHTML, 4096)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			// Plain chunk without button
			if _, err := b.client.SendMessage(msg.Chat.Id, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
				b.logger().Debugf("send thinking chunk: %v", err)
			}
			continue
		}
		// Last chunk — send with button
		sent, err := b.client.SendMessage(msg.Chat.Id, chunk, sendOpts)
		if err != nil {
			b.logger().Errorf("send reply with thinking button: %v", err)
			return
		}
		// Update button with real message ID and store thinking data
		_, _, _ = b.client.EditMessageText(chunk, &gotgbot.EditMessageTextOpts{
			ChatId:    msg.Chat.Id,
			MessageId: sent.MessageId,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show thinking", CallbackData: fmt.Sprintf("th:show:%d", sent.MessageId)},
				}},
			},
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

func (b *Bot) sendReplyPart(msg *gotgbot.Message, userID string, response string) {
	// Convert standard markdown to Telegram HTML
	response = ConvertToTelegramHTML(response)

	for _, chunk := range splitMessage(response, 4096) {
		parseMode := "HTML"
		sendErr := ""
		if _, err := b.client.SendMessage(msg.Chat.Id, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			// Retry without markdown if parsing fails
			parseMode = ""
			if _, err := b.client.SendMessage(msg.Chat.Id, chunk, nil); err != nil {
				b.logger().Errorf("send error: %s", b.sanitizeError(err))
				sendErr = err.Error()
			}
		}

		log.Conversation(log.ConversationEntry{
			Direction: "sent",
			UserID:    userID,
			Username:  msg.From.Username,
			ChatID:    msg.Chat.Id,
			Text:      chunk,
			ParseMode: parseMode,
			Session:   b.SessionKey(),
			Error:     sendErr,
		})
	}
}

// SendNotification sends a plain text notification to the last known chat.
// Used for system alerts (cache bust, etc.) — not an agent turn, no tokens spent.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendNotification(text string) {
	if strings.TrimSpace(text) == "" {
		b.logger().Debugf("skipping empty notification")
		return
	}

	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

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

// SendText sends a text message to the last known chat with HTML support.
// Returns an error if no chat ID is available.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendText(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	// Convert standard markdown to Telegram HTML
	text = ConvertToTelegramHTML(text)

	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		return fmt.Errorf("no chat ID — no messages received yet")
	}

	for _, chunk := range splitMessage(text, 4096) {
		if _, err := b.client.SendMessage(chatID, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			if _, err := b.client.SendMessage(chatID, chunk, nil); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      text,
		Session:   b.SessionKey(),
	})
	return nil
}

// SendDocument sends a file as a Telegram document to the last known chat.
func (b *Bot) SendDocument(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendDocumentToChat(chatID, filePath)
}

// SendVoice sends a voice note from a file to the last known chat.
func (b *Bot) SendVoice(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVoiceToChat(chatID, filePath)
}

// SendVideo sends a video file to the last known chat.
func (b *Bot) SendVideo(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVideoToChat(chatID, filePath)
}

// SendPhoto sends a photo to the last known chat.
func (b *Bot) SendPhoto(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendPhotoToChat(chatID, filePath)
}

// SendAudio sends an audio file to the last known chat.
func (b *Bot) SendAudio(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendAudioToChat(chatID, filePath)
}

// SendAnimation sends an animation (GIF) to the last known chat.
func (b *Bot) SendAnimation(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendAnimationToChat(chatID, filePath)
}

// SendVoiceData sends audio bytes as a Telegram voice note to the last known chat.
func (b *Bot) SendVoiceData(audioData []byte) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVoiceDataToChat(chatID, audioData)
}

// SendVoiceDataToChat sends audio bytes as a Telegram voice note to a specific chat.
func (b *Bot) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	_, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader("voice.mp3", bytes.NewReader(audioData)), nil)
	return err
}

// lastChatID returns the last known chat ID, or an error if none has been set.
func (b *Bot) lastChatID() (int64, error) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()
	if chatID == 0 {
		return 0, fmt.Errorf("no chat ID — no messages received yet")
	}
	return chatID, nil
}

// SendTextToChat sends a text message to a specific chat ID with HTML support.
func (b *Bot) SendTextToChat(chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	text = ConvertToTelegramHTML(text)

	for _, chunk := range splitMessage(text, 4096) {
		if _, err := b.client.SendMessage(chatID, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			if _, err := b.client.SendMessage(chatID, chunk, nil); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      text,
		Session:   b.SessionKey(),
	})
	return nil
}

// SendDocumentToChat sends a file as a Telegram document to a specific chat ID.
func (b *Bot) SendDocumentToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendDocument(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send document: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[document %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendVoiceToChat sends a voice note from a file to a specific chat ID.
func (b *Bot) SendVoiceToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open voice file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send voice: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[voice %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendVideoToChat sends a video file to a specific chat ID.
func (b *Bot) SendVideoToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open video file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendVideo(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send video: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[video %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendPhotoToChat sends a photo to a specific chat ID.
func (b *Bot) SendPhotoToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open photo file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendPhoto(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send photo: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[photo %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendAudioToChat sends an audio file to a specific chat ID.
func (b *Bot) SendAudioToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open audio file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendAudio(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send audio: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[audio %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendAnimationToChat sends an animation (GIF) to a specific chat ID.
func (b *Bot) SendAnimationToChat(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open animation file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := b.client.SendAnimation(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send animation: %w", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[animation %s]", filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// sendVoiceNote sends audio data as a Telegram voice note.
func (b *Bot) sendVoiceNote(chatID int64, userID string, username string, audioData []byte) {
	if _, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader("voice.mp3", bytes.NewReader(audioData)), nil); err != nil {
		b.logger().Errorf("send voice note: %s", b.sanitizeError(err))
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    userID,
		Username:  username,
		ChatID:    chatID,
		Text:      fmt.Sprintf("[voice note %d bytes]", len(audioData)),
		Session:   b.SessionKey(),
	})
}

// downloadFile downloads a file from Telegram by file ID.
func (b *Bot) downloadFile(fileID string) ([]byte, error) {
	file, err := b.client.GetFile(fileID, nil)
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.botToken, file.FilePath)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file: status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}

	return data, nil
}

// extForMediaType returns a file extension for the given media type.
func extForMediaType(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// extForVideo returns a file extension for video MIME types.
func extForVideo(mt string) string {
	switch mt {
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "video/x-matroska":
		return ".mkv"
	case "video/avi", "video/x-msvideo":
		return ".avi"
	default:
		return ".mp4"
	}
}

// extForMIME returns a file extension for common MIME types.
func extForMIME(mt string) string {
	switch {
	case strings.HasPrefix(mt, "video/"):
		return extForVideo(mt)
	case mt == "application/pdf":
		return ".pdf"
	case mt == "application/json":
		return ".json"
	case mt == "text/plain":
		return ".txt"
	case mt == "text/csv":
		return ".csv"
	case mt == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case mt == "application/vnd.ms-excel":
		return ".xls"
	case mt == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case mt == "application/msword":
		return ".doc"
	case strings.HasPrefix(mt, "audio/"):
		return ".mp3"
	default:
		return ".bin"
	}
}

// fileTooLargeError indicates a file exceeds the download size limit.
type fileTooLargeError struct {
	size int64
}

func (e *fileTooLargeError) Error() string {
	return fmt.Sprintf("file too large: %d bytes (limit 20MB)", e.size)
}

// isFileTooLarge returns true if the error is a file size limit error.
func isFileTooLarge(err error) bool {
	_, ok := err.(*fileTooLargeError)
	return ok
}

// downloadAndSaveMedia downloads a file from Telegram and saves it to disk.
// Returns the saved file path or an error (including fileTooLargeError if over 20MB).
func (b *Bot) downloadAndSaveMedia(fileID string, fileSize int64, mediaType string, chatID int64, ext string) (string, error) {
	const maxFileSize = 20 * 1024 * 1024 // 20MB Telegram Bot API limit

	if fileSize > maxFileSize {
		return "", &fileTooLargeError{size: fileSize}
	}

	if b.receivedFilesDir == "" {
		return "", fmt.Errorf("media save directory not configured")
	}

	data, err := b.downloadFile(fileID)
	if err != nil {
		return "", err
	}

	return b.saveMedia(data, mediaType, chatID, ext)
}

// saveMedia writes media data to disk and returns the saved file path.
func (b *Bot) saveMedia(data []byte, mediaType string, chatID int64, ext string) (string, error) {
	if err := os.MkdirAll(b.receivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	filename := fmt.Sprintf("%s_%s_chat-%d%s", time.Now().UTC().Format("2006-01-02T15-04-05Z"), mediaType, chatID, ext)
	path := filepath.Join(b.receivedFilesDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return path, nil
}

// saveImage writes image data to disk and returns the saved file path.
func (b *Bot) saveImage(data []byte, mediaType string, chatID int64) (string, error) {
	if err := os.MkdirAll(b.receivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create image dir: %w", err)
	}
	ext := extForMediaType(mediaType)
	filename := fmt.Sprintf("%s_chat-%d%s", time.Now().UTC().Format("2006-01-02T15-04-05Z"), chatID, ext)
	path := filepath.Join(b.receivedFilesDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write image: %w", err)
	}
	return path, nil
}

// downloadImage downloads a file and returns it as an imageAttachment,
// optionally saving to disk. Returns (attachment, true) on success.
func (b *Bot) downloadImage(fileID, mimeType string, chatID int64) (imageAttachment, bool) {
	data, err := b.downloadFile(fileID)
	if err != nil {
		b.logger().Errorf("download image: %s", b.sanitizeError(err))
		return imageAttachment{}, false
	}
	att := imageAttachment{data: data, mediaType: mimeType}
	if b.receivedFilesDir != "" {
		if path, err := b.saveImage(data, mimeType, chatID); err != nil {
			b.logger().Warnf("save image: %v", err)
		} else {
			att.savedPath = path
			b.logger().Infof("saved image to %s", path)
		}
	}
	return att, true
}

// handleMediaMessage downloads and saves a media file (video, video note,
// document), prepending a status annotation to text. On success it prepends
// "[Label saved to: path]"; on file-too-large it prepends a size warning.
func (b *Bot) handleMediaMessage(text, fileID string, fileSize int64, mediaType, label string, chatID int64, ext string) string {
	path, err := b.downloadAndSaveMedia(fileID, fileSize, mediaType, chatID, ext)
	if err != nil {
		if isFileTooLarge(err) {
			return fmt.Sprintf("[%s too large to download (%d MB)]\n\n%s", label, fileSize/(1024*1024), text)
		}
		b.logger().Errorf("download %s: %s", mediaType, b.sanitizeError(err))
		return text
	}
	b.logger().Infof("saved %s to %s", mediaType, path)
	return fmt.Sprintf("[%s saved to: %s]\n\n%s", label, path, text)
}

// isImageMIME returns true if the MIME type is a supported image format.
func isImageMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// splitMessage splits text into chunks of at most maxLen bytes.
// It prefers splitting at newline boundaries and preserves HTML formatting
// by closing open tags at split points and reopening them in the next chunk.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		chunk, rest := splitChunk(text, maxLen)
		chunks = append(chunks, chunk)
		text = rest
	}
	return chunks
}

// splitChunk splits text at a good boundary, returning the chunk and remaining text.
// It closes any open HTML tags at the end of the chunk and reopens them in the rest.
func splitChunk(text string, maxLen int) (chunk, rest string) {
	end := findSplitPoint(text, maxLen)
	open := openHTMLTags(text[:end])

	if len(open) == 0 {
		return text[:end], text[end:]
	}

	var suffix, prefix string
	for i := len(open) - 1; i >= 0; i-- {
		suffix += closingHTMLTag(open[i])
	}
	for _, tag := range open {
		prefix += tag
	}

	// Reduce split point if closing tags would exceed maxLen.
	if end+len(suffix) > maxLen {
		end = findSplitPoint(text, maxLen-len(suffix))
		// Recompute with new split point.
		open = openHTMLTags(text[:end])
		suffix = ""
		prefix = ""
		for i := len(open) - 1; i >= 0; i-- {
			suffix += closingHTMLTag(open[i])
		}
		for _, tag := range open {
			prefix += tag
		}
	}

	return text[:end] + suffix, prefix + text[end:]
}

// findSplitPoint finds the best position to split text, up to maxLen bytes.
// Prefers newline boundaries and avoids splitting inside HTML tags.
func findSplitPoint(text string, maxLen int) int {
	end := maxLen
	if end > len(text) {
		end = len(text)
	}
	if end >= len(text) {
		return end
	}

	// Prefer splitting at a newline.
	if idx := strings.LastIndex(text[:end], "\n"); idx > 0 {
		return idx + 1
	}

	// No newline — avoid splitting inside an HTML tag.
	lastOpen := strings.LastIndexByte(text[:end], '<')
	lastClose := strings.LastIndexByte(text[:end], '>')
	if lastOpen >= 0 && lastOpen > lastClose && lastOpen > 0 {
		return lastOpen
	}

	return end
}

// openHTMLTags scans HTML text and returns the stack of unclosed tags.
// Each entry is the full opening tag (e.g. "<pre>", "<a href=\"url\">").
func openHTMLTags(html string) []string {
	var stack []string
	for i := 0; i < len(html); {
		idx := strings.IndexByte(html[i:], '<')
		if idx < 0 {
			break
		}
		i += idx
		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			break // incomplete tag at end of string
		}
		tag := html[i : i+end+1]
		i += end + 1

		if strings.HasPrefix(tag, "</") {
			// Closing tag — pop matching from stack.
			name := htmlTagName(tag[2:])
			for j := len(stack) - 1; j >= 0; j-- {
				if htmlTagName(stack[j][1:]) == name {
					stack = append(stack[:j], stack[j+1:]...)
					break
				}
			}
		} else if !strings.HasSuffix(tag, "/>") {
			// Opening tag (skip self-closing).
			stack = append(stack, tag)
		}
	}
	return stack
}

// htmlTagName extracts the tag name from a string starting after '<' or '</'.
// E.g. "pre>", "a href=\"url\">" → "pre", "a".
func htmlTagName(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '>' || s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

// closingHTMLTag returns the closing tag for a full opening tag.
// E.g. "<pre>" → "</pre>", "<a href=\"url\">" → "</a>".
func closingHTMLTag(openTag string) string {
	return "</" + htmlTagName(openTag[1:]) + ">"
}

// toolEmoji maps tool names to per-tool display emoji.
var toolEmoji = map[string]string{
	"exec":            "▶️",
	"web_fetch":       "🔗",
	"web_search":      "🔍",
	"http_request":    "🌍",
	"read":            "📖",
	"write":           "✏️",
	"edit":            "✂️",
	"tmux":            "🪟",
	"todo":            "☑️",
	"send_telegram":   "📨",
	"memory_search":   "🧠",
	"spawn":           "🐣",
	"scratchpad":      "📋",
	"send_to_session": "💬",
	"remind":          "💭",
}

// emojiForTool returns the per-tool emoji, falling back to 🔧 for unknown tools.
func emojiForTool(name string) string {
	if e, ok := toolEmoji[name]; ok {
		return e
	}
	return "🔧"
}

// formatToolCall formats a tool call for display in Telegram.
func (b *Bot) formatToolCall(toolName string, params json.RawMessage) string {
	maxChars := b.toolCallPreviewChars
	if maxChars == 0 {
		maxChars = 450
	}
	// Pretty-print params; truncate only in preview mode
	paramStr := string(params)
	var pretty bytes.Buffer
	if json.Indent(&pretty, params, "", "  ") == nil {
		paramStr = pretty.String()
	}
	if b.showToolCalls != "full" && len(paramStr) > maxChars {
		paramStr = paramStr[:maxChars] + "..."
	}
	// Unescape literal \n and \t within JSON string values so they render
	// as actual newlines/tabs in the Telegram <pre> block.
	paramStr = unescapeJSONStringLiterals(paramStr)
	paramStr = htmlEscapeBot(paramStr)
	emoji := emojiForTool(toolName)
	return fmt.Sprintf("%s <b>%s</b>\n<pre>%s</pre>", emoji, htmlEscapeBot(toolName), paramStr)
}

// formatToolCallCompact returns a compact one-line summary for "full" mode.
// e.g. "⚡ exec: ls -la /tmp" or "📡 http_request: GET https://example.com"
func formatToolCallCompact(toolName string, params json.RawMessage) string {
	emoji := emojiForTool(toolName)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return fmt.Sprintf("%s <b>%s</b>", emoji, htmlEscapeBot(toolName))
	}

	summary := compactSummary(toolName, m)
	if summary == "" {
		return fmt.Sprintf("%s <b>%s</b>", emoji, htmlEscapeBot(toolName))
	}
	return fmt.Sprintf("%s <b>%s</b>: %s", emoji, htmlEscapeBot(toolName), htmlEscapeBot(summary))
}

// compactSummary extracts the most meaningful param values for a compact display.
func compactSummary(toolName string, m map[string]json.RawMessage) string {
	str := func(key string) string {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		// Not a string — use raw JSON (e.g. numbers, booleans)
		return strings.TrimSpace(string(raw))
	}

	switch toolName {
	case "exec":
		return truncate(str("command"), 60)
	case "web_fetch":
		return truncate(str("url"), 80)
	case "web_search", "memory_search":
		return truncate(str("query"), 60)
	case "http_request":
		method := str("method")
		if method == "" {
			method = "GET"
		}
		return truncate(method+" "+str("url"), 80)
	case "read", "write", "edit":
		return truncate(str("path"), 80)
	case "tmux":
		op := str("operation")
		name := str("name")
		if op != "" && name != "" {
			return op + " " + name
		}
		if op != "" {
			return op
		}
		return truncate(str("name"), 60)
	case "todo", "scratchpad":
		return str("action")
	case "remind":
		return truncate(str("text"), 40)
	case "send_telegram":
		return truncate(str("text"), 40)
	case "spawn":
		return truncate(str("prompt"), 40)
	}

	// Fallback: use the first string-valued param
	for _, key := range sortedKeys(m) {
		if v := str(key); v != "" {
			return truncate(v, 60)
		}
	}
	return ""
}

// sortedKeys returns map keys in sorted order for deterministic fallback.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// sort inline to avoid import — simple insertion sort for small maps
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// unescapeJSONStringLiterals replaces literal \n and \t sequences (as they
// appear in pretty-printed JSON string values) with actual newline and tab
// characters so they render properly inside Telegram <pre> blocks.
func unescapeJSONStringLiterals(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	return s
}

// htmlEscapeBot escapes HTML special characters for Telegram messages.
func htmlEscapeBot(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// sendCommandKeyboard sends an inline keyboard message for a bare slash command.
func (b *Bot) sendCommandKeyboard(chatID int64, cmdName string, opts []command.KeyboardOption) {
	// Group options by row
	rowMap := make(map[int][]gotgbot.InlineKeyboardButton)
	maxRow := 0
	for _, opt := range opts {
		data := fmt.Sprintf("cmd:/%s %s", cmdName, opt.Data)
		rowMap[opt.Row] = append(rowMap[opt.Row], gotgbot.InlineKeyboardButton{
			Text:         opt.Label,
			CallbackData: data,
		})
		if opt.Row > maxRow {
			maxRow = opt.Row
		}
	}

	rows := make([][]gotgbot.InlineKeyboardButton, 0, maxRow+1)
	for i := 0; i <= maxRow; i++ {
		if buttons, ok := rowMap[i]; ok {
			rows = append(rows, buttons)
		}
	}

	label := fmt.Sprintf("/%s:", cmdName)
	_, _ = b.client.SendMessage(chatID, label, &gotgbot.SendMessageOpts{
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
}

// handleCallbackQuery processes inline keyboard button presses for tool result
// and thinking block expansion, and command keyboard selections.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *gotgbot.CallbackQuery) {
	if cq.Data == "" || cq.Message.GetChat().Id == 0 {
		return
	}
	chatID := cq.Message.GetChat().Id

	// Always answer the callback query to dismiss the loading indicator.
	defer func() {
		_, _ = b.client.AnswerCallbackQuery(cq.Id, nil)
	}()

	// Command keyboard callbacks: "cmd:/name args"
	if strings.HasPrefix(cq.Data, "cmd:") {
		cmdText := cq.Data[4:] // strip "cmd:" prefix
		b.handleCommandCallback(ctx, chatID, cq.Message.GetMessageId(), cmdText)
		return
	}

	parts := strings.SplitN(cq.Data, ":", 3)
	if len(parts) != 3 {
		return
	}
	action := parts[1] // "show" or "hide"
	msgID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}

	switch parts[0] {
	case "tc":
		b.handleToolCallCallback(chatID, action, msgID)
	case "th":
		b.handleThinkingCallback(chatID, action, msgID)
	}
}

// handleCommandCallback executes a command from an inline keyboard press
// and edits the original message to show the result.
func (b *Bot) handleCommandCallback(ctx context.Context, chatID, msgID int64, cmdText string) {
	cmdCtx := context.WithValue(ctx, command.LastMessageUserKey{}, "")
	cmdCtx = context.WithValue(cmdCtx, command.ChatIDKey{}, chatID)
	cmdCtx = context.WithValue(cmdCtx, command.DisplayWidthKey{}, b.displayWidth)

	// Check if this bare subcommand needs a chained keyboard (e.g. /tmux kill → pick session)
	if parentName, opts, ok := b.commands.LookupChainKeyboard(cmdCtx, cmdText); ok {
		b.editMessageWithKeyboard(chatID, msgID, parentName, cmdText, opts)
		return
	}

	result, ok := b.commands.Dispatch(cmdCtx, cmdText)
	if !ok {
		result = "Unknown command: " + cmdText
	}

	// Strip multi-message separators for edit (edit replaces single message)
	result = strings.ReplaceAll(result, "\x00", "\n\n")

	display := ConvertToTelegramHTML(result)
	if len(display) > 4096 {
		display = display[:4090] + "\n..."
	}

	b.logger().Debugf("command callback %q dispatched", cmdText)

	_, _, err := b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: msgID,
		ParseMode: "HTML",
	})
	if err != nil {
		b.logger().Debugf("command callback HTML edit failed: %v, retrying as plain text", err)
		_, _, _ = b.client.EditMessageText(result, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
		})
	}
}

// editMessageWithKeyboard replaces the message with a chained inline keyboard.
func (b *Bot) editMessageWithKeyboard(chatID, msgID int64, parentName, cmdText string, opts []command.KeyboardOption) {
	// Group options by row
	rowMap := make(map[int][]command.KeyboardOption)
	for _, o := range opts {
		rowMap[o.Row] = append(rowMap[o.Row], o)
	}
	maxRow := 0
	for r := range rowMap {
		if r > maxRow {
			maxRow = r
		}
	}
	var rows [][]gotgbot.InlineKeyboardButton
	for r := 0; r <= maxRow; r++ {
		ropts := rowMap[r]
		if len(ropts) == 0 {
			continue
		}
		var buttons []gotgbot.InlineKeyboardButton
		for _, o := range ropts {
			buttons = append(buttons, gotgbot.InlineKeyboardButton{
				Text:         o.Label,
				CallbackData: fmt.Sprintf("cmd:/%s %s", parentName, o.Data),
			})
		}
		rows = append(rows, buttons)
	}

	display := "/" + parentName + " " + strings.TrimPrefix(cmdText, "/"+parentName+" ") + ":"
	_, _, _ = b.client.EditMessageText(display, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: msgID,
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
}

// handleToolCallCallback handles tool call expand/collapse button presses.
func (b *Bot) handleToolCallCallback(chatID int64, action string, msgID int64) {
	toolTextVal, ok := b.toolResults.Load(msgID)
	if !ok {
		return
	}
	stored := toolTextVal.(toolResultEntry)

	switch action {
	case "show":
		var expanded string
		if stored.result == "" {
			// Tool still running — show params with placeholder.
			expanded = formatToolCallWithResult(stored.fullInput, "⏳ Running...")
		} else {
			expanded = formatToolCallWithResult(stored.fullInput, stored.result)
		}
		// Mark as expanded so ToolResultObserver can update when result arrives.
		stored.expanded = true
		stored.chatID = chatID
		b.toolResults.Store(msgID, stored)
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide", CallbackData: fmt.Sprintf("tc:hide:%d", msgID)},
				}},
			},
		})
	case "hide":
		stored.expanded = false
		b.toolResults.Store(msgID, stored)
		_, _, _ = b.client.EditMessageText(stored.compactText, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", msgID)},
				}},
			},
		})
	}
}

// handleThinkingCallback handles thinking block expand/collapse button presses.
func (b *Bot) handleThinkingCallback(chatID int64, action string, msgID int64) {
	val, ok := b.thinkingStore.Load(msgID)
	if !ok {
		return
	}
	entry := val.(thinkingEntry)

	switch action {
	case "show":
		expanded := formatThinkingExpanded(entry.thinkingText, entry.responseHTML, b.displayWidth)
		_, _, _ = b.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide thinking", CallbackData: fmt.Sprintf("th:hide:%d", msgID)},
				}},
			},
		})
	case "hide":
		_, _, _ = b.client.EditMessageText(entry.responseHTML, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show thinking", CallbackData: fmt.Sprintf("th:show:%d", msgID)},
				}},
			},
		})
	}
}

// formatThinkingExpanded prepends thinking text above a separator, with the response below.
func formatThinkingExpanded(thinkingText, responseHTML string, displayWidth int) string {
	escaped := htmlEscapeBot(thinkingText)
	divider := "\n" + strings.Repeat("—", displayWidth) + "\n"
	result := "<i>" + escaped + "</i>" + divider + responseHTML
	// Telegram messages are limited to 4096 characters; truncate thinking if needed.
	if len(result) > 4096 {
		budget := 4096 - len(responseHTML) - len(divider) - len("<i>") - len("</i>") - len("\n... (truncated)")
		if budget < 100 {
			budget = 100
		}
		escaped = truncateHTMLSafe(escaped, budget) + "\n... (truncated)"
		result = "<i>" + escaped + "</i>" + divider + responseHTML
	}
	return result
}

// truncateHTMLSafe truncates HTML-escaped text to maxLen bytes without splitting
// HTML entities (e.g. &amp; &lt; &gt;). If the cut falls inside an entity, it
// backs up to before the '&'.
func truncateHTMLSafe(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	s = s[:maxLen]
	// If we cut inside an HTML entity, back up to before the '&'.
	if idx := strings.LastIndex(s, "&"); idx != -1 {
		if !strings.Contains(s[idx:], ";") {
			s = s[:idx]
		}
	}
	return s
}

// toolCallTracker manages tool call visibility state during an agent turn.
// It encapsulates the mutable state shared between ToolCallObserver and
// ToolResultObserver callbacks (message ID, text snapshots, mutex).
type toolCallTracker struct {
	bot    *Bot
	chatID int64

	mu       sync.Mutex
	msgID    int64  // Telegram message ID of the current tool-call message
	text     string // last compact summary HTML (full mode) or full HTML (preview mode)
	fullText string // last full formatted tool call HTML (full mode only)
}

// lastMsgID returns the current tool-call message ID (thread-safe).
func (t *toolCallTracker) lastMsgID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgID
}

// resetMsgID clears the tool-call message ID (e.g. after intermediate text).
func (t *toolCallTracker) resetMsgID() {
	t.mu.Lock()
	t.msgID = 0
	t.mu.Unlock()
}

// observeToolCall handles tool call visibility via send+edit pattern.
func (t *toolCallTracker) observeToolCall(toolName string, params json.RawMessage) {
	if t.bot.showToolCalls == "off" || t.bot.showToolCalls == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.bot.showToolCalls == "full" {
		t.sendFullModeToolCall(toolName, params)
		return
	}
	t.sendPreviewModeToolCall(toolName, params)
}

// sendFullModeToolCall sends a compact summary with a "Show full" button.
func (t *toolCallTracker) sendFullModeToolCall(toolName string, params json.RawMessage) {
	compact := formatToolCallCompact(toolName, params)
	full := t.bot.formatToolCall(toolName, params)
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
				{Text: "Show full", CallbackData: "tc:show:0"},
			}},
		},
	}
	sent, err := t.bot.client.SendMessage(t.chatID, compact, sendOpts)
	if err != nil {
		t.bot.logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.msgID = sent.MessageId
	t.text = compact
	t.fullText = full
	t.bot.toolResults.Store(t.msgID, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		chatID:      t.chatID,
	})
	_, _, _ = t.bot.client.EditMessageText(compact, &gotgbot.EditMessageTextOpts{
		ChatId:    t.chatID,
		MessageId: t.msgID,
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
				{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", t.msgID)},
			}},
		},
	})
}

// sendPreviewModeToolCall sends or edits a tool call message (overwriting previous).
func (t *toolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := t.bot.formatToolCall(toolName, params)
	sendOpts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
	if t.msgID == 0 {
		sent, err := t.bot.client.SendMessage(t.chatID, text, sendOpts)
		if err != nil {
			t.bot.logger().Debugf("send tool call msg: %v", err)
			return
		}
		t.msgID = sent.MessageId
		t.text = text
	} else {
		_, _, err := t.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: t.msgID,
			ParseMode: "HTML",
		})
		if err != nil {
			t.bot.logger().Debugf("edit tool call msg: %v", err)
		}
		t.text = text
	}
}

// observeToolResult stores tool results for inline keyboard expansion (full mode only).
func (t *toolCallTracker) observeToolResult(toolName string, result string, isError bool) {
	if t.bot.showToolCalls != "full" {
		return
	}
	t.mu.Lock()
	msgID := t.msgID
	compact := t.text
	full := t.fullText
	t.mu.Unlock()
	if msgID == 0 {
		return
	}

	var wasExpanded bool
	if prev, ok := t.bot.toolResults.Load(msgID); ok {
		entry := prev.(toolResultEntry)
		wasExpanded = entry.expanded
	}

	t.bot.toolResults.Store(msgID, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		result:      result,
		expanded:    wasExpanded,
		chatID:      t.chatID,
	})
	if t.bot.toolDetailStore != nil {
		t.bot.toolDetailStore.Store(msgID, compact, full, result)
	}

	if wasExpanded {
		expanded := formatToolCallWithResult(full, result)
		_, _, _ = t.bot.client.EditMessageText(expanded, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: msgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Hide", CallbackData: fmt.Sprintf("tc:hide:%d", msgID)},
				}},
			},
		})
	}
}

// toolResultEntry stores the compact summary, full input text, and result
// for inline keyboard expansion in "full" mode.
type toolResultEntry struct {
	compactText string // compact one-line summary (collapsed state)
	fullInput   string // full formatted tool call HTML with JSON params
	result      string // the raw tool result text (empty while tool is running)
	expanded    bool   // true if user clicked "Show full" before result arrived
	chatID      int64  // chat where the message lives (for deferred edits)
}

// thinkingEntry stores thinking text and response HTML for compact mode toggle.
type thinkingEntry struct {
	responseHTML string // the original response HTML (collapsed state)
	thinkingText string // raw thinking text
}

// formatToolCallWithResult combines a tool call message with its result,
// truncating the result so the total message fits within Telegram's 4096 char limit.
func formatToolCallWithResult(toolText, result string) string {
	const maxLen = 4096
	separator := "\n\n📋 <b>Result:</b>\n<pre>"
	suffix := "</pre>"

	overhead := len(toolText) + len(separator) + len(suffix)
	if overhead >= maxLen {
		// Tool text alone is too long; just return it as-is.
		return toolText
	}

	escapedResult := htmlEscapeBot(result)
	available := maxLen - overhead
	if len(escapedResult) > available {
		escapedResult = escapedResult[:available-3] + "..."
	}
	return toolText + separator + escapedResult + suffix
}

// sanitizeError replaces the bot token in an error string to prevent it
// from leaking into log files.
func (b *Bot) sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	if b.botToken == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), b.botToken, "[REDACTED]")
}
