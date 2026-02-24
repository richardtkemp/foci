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
	"strings"
	"sync"
	"time"

	"clod/agent"
	"clod/command"
	"clod/log"
	"clod/state"
	"clod/voice"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// botClient abstracts Telegram API methods for testability.
type botClient interface {
	SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error)
	EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error)
	SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error)
	SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error)
	SendChatAction(chatId int64, action string, opts *gotgbot.SendChatActionOpts) (bool, error)
	GetFile(fileId string, opts *gotgbot.GetFileOpts) (*gotgbot.File, error)
	SetMyCommands(commands []gotgbot.BotCommand, opts *gotgbot.SetMyCommandsOpts) (bool, error)
}

// imageAttachment is a downloaded image ready for the agent.
type imageAttachment struct {
	data      []byte
	mediaType string
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
	api          *gotgbot.Bot // for receiving updates (Run)
	client       botClient    // for sending messages and files (mockable in tests)
	agent        *agent.Agent
	commands     *command.Registry
	lastMsgStore *command.LastMessageStore // for // repeat command
	allowedUsers map[string]bool
	agentID      string       // agent ID for session key derivation
	sessionKey   string       // override session key (multiball secondary bots only)
	sessionMu    sync.RWMutex // protects sessionKey (mutable for secondary bots)
	isSecondary  bool         // true for secondary bots (multiball)
	pool                *Pool        // back-reference to pool (secondary bots only)
	OnSessionKeyChange  func(username, sessionKey string) // fires after SetSessionKey (fork/release)
	botToken            string       // for building file download URLs

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
}

// NewBot creates a new Telegram bot.
// agentID is used for per-chat session key derivation (agent:ID:chat:CHATID).
// For secondary (multiball) bots, pass agentID="" — their session key is set dynamically via SetSessionKey.
func NewBot(token string, allowedUsers []string, ag *agent.Agent, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) (*Bot, error) {
	api, err := gotgbot.NewBot(token, nil)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}

	return &Bot{
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

// SetStateStore configures persistent state for this bot.
// key is used as the prefix for state keys (e.g. "bot:mybot").
func (b *Bot) SetStateStore(store *state.Store, key string) {
	b.stateStore = store
	b.stateKey = key

	// Restore chatID from persisted state
	var chatID int64
	if store.Get(key+":chatid", &chatID) && chatID != 0 {
		b.SetChatID(chatID)
		log.Infof("telegram", "restored chat ID %d from state", chatID)
	}

	// Restore default chat from persisted state (for per-chat session routing)
	if b.agentID != "" {
		var defaultChat int64
		if store.Get("agent:"+b.agentID+":default_chat", &defaultChat) && defaultChat != 0 {
			log.Infof("telegram", "restored default chat %d for agent %s", defaultChat, b.agentID)
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
		log.Errorf("telegram", "persist default chat: %v", err)
	}
}

// recordChatUsername persists the username for a chat ID (for /sessions display).
func (b *Bot) recordChatUsername(chatID int64, username string) {
	if b.stateStore == nil || b.agentID == "" || username == "" {
		return
	}
	key := fmt.Sprintf("agent:%s:chat:%d:username", b.agentID, chatID)
	if err := b.stateStore.Set(key, username); err != nil {
		log.Errorf("telegram", "persist chat username: %v", err)
	}
}

// DefaultSessionKey returns the session key for the default chat.
// Used by heartbeats, cron, and other proactive features.
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
		log.Warnf("telegram", "setMyCommands: %s", b.sanitizeError(err))
		return
	}
	log.Infof("telegram", "registered %d commands with BotFather", len(cmds))
}

// Run starts the receiver and agent worker goroutines. Blocks until ctx is cancelled.
// If polling fails, it recovers and retries with backoff.
func (b *Bot) Run(ctx context.Context) {
	log.Infof("telegram", "bot started as @%s", b.api.Username)

	b.RegisterCommands()

	// Agent worker — processes queued messages sequentially
	go b.agentWorker(ctx)

	for ctx.Err() == nil {
		b.pollUpdates(ctx)

		if ctx.Err() != nil {
			return
		}

		log.Warnf("telegram", "polling interrupted, restarting in 5s...")
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
			log.Errorf("telegram", "panic in polling: %v", r)
		}
	}()

	type updateResult struct {
		updates []gotgbot.Update
		err     error
	}

	var offset int64
	// On exit, acknowledge processed updates so they aren't replayed on restart.
	// Telegram acknowledges updates implicitly when the next getUpdates has a
	// higher offset, so we must fire one final short-poll before returning.
	defer func() {
		if offset > 0 {
			_, err := b.api.GetUpdates(&gotgbot.GetUpdatesOpts{
				Offset:  offset,
				Timeout: 0,
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 5 * time.Second,
				},
			})
			if err != nil {
				log.Errorf("telegram", "failed to ack updates on shutdown: %s", b.sanitizeError(err))
			} else {
				log.Infof("telegram", "acknowledged updates up to offset %d", offset)
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
				Offset:  offset,
				Timeout: 60,
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
				log.Errorf("telegram", "get updates: %s", b.sanitizeError(res.err))
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
				continue
			}

			for _, update := range res.updates {
				if update.UpdateId >= offset {
					offset = update.UpdateId + 1
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

// receiveMessage handles an incoming message on the receiver goroutine.
// Slash commands execute immediately. Agent messages are queued.
func (b *Bot) receiveMessage(ctx context.Context, msg *gotgbot.Message) {
	userID := fmt.Sprintf("%d", msg.From.Id)

	if !b.allowedUsers[userID] {
		log.Warnf("telegram", "rejected message from user %s (%s)", userID, msg.From.Username)
		return
	}

	// Remember chat ID for notifications (cache bust alerts, etc.)
	b.chatMu.Lock()
	changed := b.chatID != msg.Chat.Id
	b.chatID = msg.Chat.Id
	b.chatMu.Unlock()

	if changed && b.stateStore != nil {
		if err := b.stateStore.Set(b.stateKey+":chatid", msg.Chat.Id); err != nil {
			log.Errorf("telegram", "persist chat ID: %v", err)
		}
	}

	// Per-chat session routing: set default chat on first message, record username
	if !b.isSecondary && b.agentID != "" {
		if b.defaultChatID() == 0 {
			b.setDefaultChat(msg.Chat.Id)
			log.Infof("telegram", "set default chat %d for agent %s", msg.Chat.Id, b.agentID)
		}
		if msg.From != nil {
			b.recordChatUsername(msg.Chat.Id, msg.From.Username)
		}
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
			log.Errorf("telegram", "download voice: %s", b.sanitizeError(err))
		} else {
			transcript, err := b.transcriber.Transcribe(ctx, data, "voice.ogg")
			if err != nil {
				log.Errorf("telegram", "transcribe voice: %v", err)
				b.sendReply(msg, userID, "Could not transcribe voice note.")
				return
			}
			log.Infof("telegram", "voice transcription from %s: %s", msg.From.Username, truncate(transcript, 100))
			text = "[voice] " + transcript
		}
	}

	// Download images from photos or image documents
	var images []imageAttachment
	if len(msg.Photo) > 0 {
		// Take the largest photo (last in the array)
		photo := msg.Photo[len(msg.Photo)-1]
		if data, err := b.downloadFile(photo.FileId); err != nil {
			log.Errorf("telegram", "download photo: %s", b.sanitizeError(err))
		} else {
			images = append(images, imageAttachment{data: data, mediaType: "image/jpeg"})
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if data, err := b.downloadFile(msg.Document.FileId); err != nil {
			log.Errorf("telegram", "download document: %s", b.sanitizeError(err))
		} else {
			images = append(images, imageAttachment{data: data, mediaType: msg.Document.MimeType})
		}
	}

	// Drop messages with no text and no images
	if text == "" && len(images) == 0 {
		return
	}

	logText := text
	if len(images) > 0 {
		logText = fmt.Sprintf("[%d image(s)] %s", len(images), text)
	}
	log.Infof("telegram", "message from %s: %s", msg.From.Username, truncate(logText, 100))

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

	// Slash commands bypass the agent pipeline entirely
	if text != "" && strings.HasPrefix(text, "/") {
		cmd := strings.ToLower(strings.TrimSpace(text))

		// Skip stale slash commands (e.g. /restart replayed from the update
		// queue after a crash). Agent messages are still delivered since the
		// agent can reason about timeliness, but slash commands execute
		// unconditionally so stale ones must be dropped.
		if age := time.Since(time.Unix(int64(msg.Date), 0)); age > 30*time.Second {
			log.Warnf("telegram", "dropping stale command %q (age=%s)", cmd, age.Truncate(time.Second))
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
			log.Infof("telegram", "secondary bot detached from %s", sk)
			return
		}

		// Pass userID and chatID to command via context
		cmdCtx := context.WithValue(ctx, command.LastMessageUserKey{}, userID)
		cmdCtx = context.WithValue(cmdCtx, command.ChatIDKey{}, msg.Chat.Id)
		if result, ok := b.commands.Dispatch(cmdCtx, text); ok {
			log.Debugf("telegram", "command %s dispatched", text)
			b.sendReply(msg, userID, result)
			return
		}
	}

	// Secondary bots with no session reject non-command messages
	if b.isSecondary && b.SessionKey() == "" {
		b.sendReply(msg, userID, "This bot is idle. Use /multiball in the main bot to start a session.")
		return
	}

	// Queue for the agent worker
	select {
	case b.queue <- queuedMessage{msg: msg, userID: userID, text: text, images: images}:
	default:
		log.Warnf("telegram", "message queue full, dropping message from %s", msg.From.Username)
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

	// Send typing indicator
	b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)

	// Track tool calls for live visibility via send+edit pattern.
	// Declared before ReplyFunc so the closure can reset toolMsgID
	// when intermediate text is sent (fixes message ordering).
	var toolMsgID int64
	var toolMsgMu sync.Mutex

	// Per-turn callbacks scoped to context -- no cross-turn races.
	cb := &agent.TurnCallbacks{
		// Intermediate reply delivery (deferred replies).
		// When intermediate text fires, reset toolMsgID so the next tool call
		// creates a fresh message below the text instead of editing the stale
		// earlier message (which would appear above the text in chat).
		ReplyFunc: func(text string) {
			b.sendReply(qm.msg, qm.userID, text)
			toolMsgMu.Lock()
			toolMsgID = 0
			toolMsgMu.Unlock()
		},
		// Voice reply delivery (for TTS tool)
		VoiceReplyFunc: func(oggData []byte) {
			b.sendVoiceNote(qm.msg.Chat.Id, qm.userID, qm.msg.From.Username, oggData)
		},
		// Refresh typing indicator when tools complete
		ActivityFunc: func() {
			b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
		},
		// Tool call visibility via send+edit pattern
		ToolCallObserver: func(toolName string, params json.RawMessage) {
			toolMsgMu.Lock()
			defer toolMsgMu.Unlock()

			text := b.formatToolCall(toolName, params)
			if toolMsgID == 0 {
				// First tool call: send a new message
				sent, err := b.client.SendMessage(qm.msg.Chat.Id, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
				if err != nil {
					log.Debugf("telegram", "send tool call msg: %v", err)
					return
				}
				toolMsgID = sent.MessageId
			} else {
				// Subsequent tool calls: edit the existing message
				_, _, err := b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
					ChatId:    qm.msg.Chat.Id,
					MessageId: toolMsgID,
					ParseMode: "HTML",
				})
				if err != nil {
					log.Debugf("telegram", "edit tool call msg: %v", err)
				}
			}
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
			agentImages[i] = agent.ImageData{MediaType: img.mediaType, Data: img.data}
		}
		response, err = b.agent.HandleMessageWithImages(turnCtx, sk, qm.text, agentImages)
	} else {
		response, err = b.agent.HandleMessage(turnCtx, sk, qm.text)
	}
	if err != nil {
		if turnCtx.Err() != nil {
			log.Infof("telegram", "agent turn cancelled")
			return // /stop was called, "Stopped." already sent
		}
		log.Errorf("telegram", "agent error: %s", b.sanitizeError(err))
		response = fmt.Sprintf("Error: %s", b.sanitizeError(err))
	}

	// Guard against empty responses (e.g. end_turn after tool use with no text).
	// Sending an empty string to Telegram would fail with "message text is empty".
	if strings.TrimSpace(response) == "" {
		log.Debugf("telegram", "agent returned empty response for %s, not sending", sk)
		return
	}

	// Voice mode: convert final reply to voice note
	if b.agent.VoiceMode(sk) && b.tts != nil && response != "" {
		if audioData, err := b.tts.Synthesize(turnCtx, response); err != nil {
			log.Errorf("telegram", "tts for voice mode: %v", err)
			b.sendReply(qm.msg, qm.userID, response) // fall back to text
		} else {
			b.sendVoiceNote(qm.msg.Chat.Id, qm.userID, qm.msg.From.Username, audioData)
		}
		return
	}

	// If we sent a tool-call message, try to edit it with the final response.
	// Fall back to sendReply if the response is too long for a single edit
	// or if the edit fails.
	toolMsgMu.Lock()
	editID := toolMsgID
	toolMsgMu.Unlock()

	if editID != 0 && len(response) <= 4096 {
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
		log.Debugf("telegram", "edit final response failed, falling back: %v", editErr)
	}

	b.sendReply(qm.msg, qm.userID, response)
}

// cancelTurn cancels the in-flight agent turn, if any.
func (b *Bot) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnCancel != nil {
		log.Infof("telegram", "cancelling agent turn via /stop")
		b.turnCancel()
	}
}

// sendReply sends a response back to the user, splitting long messages and
// falling back to plain text if HTML formatting fails.
// Logs each chunk to the conversation log.
func (b *Bot) sendReply(msg *gotgbot.Message, userID string, response string) {
	// Convert standard markdown to Telegram HTML
	response = ConvertToTelegramHTML(response)

	for _, chunk := range splitMessage(response, 4096) {
		parseMode := "HTML"
		sendErr := ""
		if _, err := b.client.SendMessage(msg.Chat.Id, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			// Retry without markdown if parsing fails
			parseMode = ""
			if _, err := b.client.SendMessage(msg.Chat.Id, chunk, nil); err != nil {
				log.Errorf("telegram", "send error: %s", b.sanitizeError(err))
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
		log.Debugf("telegram", "skipping empty notification")
		return
	}

	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		log.Warnf("telegram", "no chat ID for notification: %s", text)
		return
	}

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		log.Errorf("telegram", "send notification: %s", b.sanitizeError(err))
	}
}

// SendStartupNotification sends a startup notification to the last known chat.
// Skips silently if no chat ID is available (expected on first run or fresh state).
func (b *Bot) SendStartupNotification(agentID string) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		log.Debugf("telegram", "no chat ID for startup notification (no prior messages)")
		return
	}

	botName := b.Username()
	if botName == "" {
		botName = "clod"
	}
	text := fmt.Sprintf("%s restarted at %s", botName, time.Now().Format("15:04:05"))

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		log.Errorf("telegram", "send startup notification: %s", b.sanitizeError(err))
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
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		return fmt.Errorf("no chat ID — no messages received yet")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	defer f.Close()

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

// SendVoice sends a voice note from a file to the last known chat.
func (b *Bot) SendVoice(filePath string) error {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		return fmt.Errorf("no chat ID — no messages received yet")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open voice file: %w", err)
	}
	defer f.Close()

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

// sendVoiceNote sends audio data as a Telegram voice note.
func (b *Bot) sendVoiceNote(chatID int64, userID string, username string, audioData []byte) {
	if _, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader("voice.mp3", bytes.NewReader(audioData)), nil); err != nil {
		log.Errorf("telegram", "send voice note: %s", b.sanitizeError(err))
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file: status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}

	return data, nil
}

// isImageMIME returns true if the MIME type is a supported image format.
func isImageMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		end := maxLen
		if end > len(text) {
			end = len(text)
		}
		// Try to split at a newline
		if end < len(text) {
			if idx := strings.LastIndex(text[:end], "\n"); idx > 0 {
				end = idx + 1
			}
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// formatToolCall formats a tool call for display in Telegram.
func (b *Bot) formatToolCall(toolName string, params json.RawMessage) string {
	maxChars := b.toolCallPreviewChars
	if maxChars == 0 {
		maxChars = 450
	}
	// Pretty-print params, truncated
	paramStr := string(params)
	var pretty bytes.Buffer
	if json.Indent(&pretty, params, "", "  ") == nil {
		paramStr = pretty.String()
	}
	if len(paramStr) > maxChars {
		paramStr = paramStr[:maxChars] + "..."
	}
	// Unescape literal \n and \t within JSON string values so they render
	// as actual newlines/tabs in the Telegram <pre> block.
	paramStr = unescapeJSONStringLiterals(paramStr)
	paramStr = htmlEscapeBot(paramStr)
	return fmt.Sprintf("🔧 <b>%s</b>\n<pre>%s</pre>", htmlEscapeBot(toolName), paramStr)
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
