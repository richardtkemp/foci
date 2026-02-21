package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"clod/agent"
	"clod/command"
	"clod/log"
	"clod/voice"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// sender abstracts the Telegram send API for testability.
type sender interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
}

// fileGetter abstracts getting file info from Telegram for testability.
type fileGetter interface {
	GetFile(config tgbotapi.FileConfig) (tgbotapi.File, error)
}

// imageAttachment is a downloaded image ready for the agent.
type imageAttachment struct {
	data      []byte
	mediaType string
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg    *tgbotapi.Message
	userID string
	text   string
	images []imageAttachment
}

// Bot wraps the Telegram bot API with agent integration.
// Messages are received on one goroutine and processed on another.
// Slash commands execute immediately on the receiver goroutine.
type Bot struct {
	api          *tgbotapi.BotAPI // for receiving updates (Run)
	sender       sender           // for sending messages (mockable in tests)
	fileGetter   fileGetter       // for getting file info (mockable in tests)
	agent        *agent.Agent
	commands     *command.Registry
	allowedUsers map[string]bool
	sessionKey   string
	botToken     string           // for building file download URLs

	transcriber  *voice.Transcriber // nil = voice notes not supported
	tts          *voice.TTS         // nil = TTS not available

	queue      chan queuedMessage     // receiver → agent worker
	turnCancel context.CancelFunc     // cancel the current agent turn
	turnMu     sync.Mutex             // protects turnCancel
	chatID     int64                   // last known chat ID (for notifications)
	chatMu     sync.Mutex
}

// NewBot creates a new Telegram bot.
func NewBot(token string, allowedUsers []string, ag *agent.Agent, cmds *command.Registry, sessionKey string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}

	return &Bot{
		api:          api,
		sender:       api,
		fileGetter:   api,
		agent:        ag,
		commands:     cmds,
		allowedUsers: allowed,
		sessionKey:   sessionKey,
		botToken:     token,
		queue:        make(chan queuedMessage, 64),
	}, nil
}

// SetTranscriber sets the Whisper transcriber for inbound voice notes.
func (b *Bot) SetTranscriber(t *voice.Transcriber) {
	b.transcriber = t
}

// SetTTS sets the TTS engine for outbound voice notes.
func (b *Bot) SetTTS(t *voice.TTS) {
	b.tts = t
}

// Run starts the receiver and agent worker goroutines. Blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	log.Infof("telegram", "bot started as @%s", b.api.Self.UserName)

	// Agent worker — processes queued messages sequentially
	go b.agentWorker(ctx)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return
		case update := <-updates:
			if update.Message == nil {
				continue
			}
			b.receiveMessage(ctx, update.Message)
		}
	}
}

// receiveMessage handles an incoming message on the receiver goroutine.
// Slash commands execute immediately. Agent messages are queued.
func (b *Bot) receiveMessage(ctx context.Context, msg *tgbotapi.Message) {
	userID := fmt.Sprintf("%d", msg.From.ID)

	if !b.allowedUsers[userID] {
		log.Warnf("telegram", "rejected message from user %s (%s)", userID, msg.From.UserName)
		return
	}

	// Remember chat ID for notifications (cache bust alerts, etc.)
	b.chatMu.Lock()
	b.chatID = msg.Chat.ID
	b.chatMu.Unlock()

	// Get text from message or caption (photos use caption)
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// Handle voice notes: download, transcribe, tag with [voice]
	if msg.Voice != nil && b.transcriber != nil {
		if data, err := b.downloadFile(msg.Voice.FileID); err != nil {
			log.Errorf("telegram", "download voice: %v", err)
		} else {
			transcript, err := b.transcriber.Transcribe(ctx, data, "voice.ogg")
			if err != nil {
				log.Errorf("telegram", "transcribe voice: %v", err)
				b.sendReply(msg, userID, "Could not transcribe voice note.")
				return
			}
			log.Infof("telegram", "voice transcription from %s: %s", msg.From.UserName, truncate(transcript, 100))
			text = "[voice] " + transcript
		}
	}

	// Download images from photos or image documents
	var images []imageAttachment
	if msg.Photo != nil && len(msg.Photo) > 0 {
		// Take the largest photo (last in the array)
		photo := msg.Photo[len(msg.Photo)-1]
		if data, err := b.downloadFile(photo.FileID); err != nil {
			log.Errorf("telegram", "download photo: %v", err)
		} else {
			images = append(images, imageAttachment{data: data, mediaType: "image/jpeg"})
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if data, err := b.downloadFile(msg.Document.FileID); err != nil {
			log.Errorf("telegram", "download document: %v", err)
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
	log.Infof("telegram", "message from %s: %s", msg.From.UserName, truncate(logText, 100))

	// Log received message
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    userID,
		Username:  msg.From.UserName,
		ChatID:    msg.Chat.ID,
		Text:      logText,
		Session:   b.sessionKey,
	})

	// Slash commands bypass the agent pipeline entirely
	if text != "" && strings.HasPrefix(text, "/") {
		// /stop cancels the current agent turn
		if strings.ToLower(strings.TrimSpace(text)) == "/stop" {
			b.cancelTurn()
			b.sendReply(msg, userID, "Stopped.")
			return
		}

		if result, ok := b.commands.Dispatch(ctx, text); ok {
			log.Debugf("telegram", "command %s dispatched", text)
			b.sendReply(msg, userID, result)
			return
		}
	}

	// Queue for the agent worker
	select {
	case b.queue <- queuedMessage{msg: msg, userID: userID, text: text, images: images}:
	default:
		log.Warnf("telegram", "message queue full, dropping message from %s", msg.From.UserName)
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
	typing := tgbotapi.NewChatAction(qm.msg.Chat.ID, tgbotapi.ChatTyping)
	b.sender.Send(typing)

	// Set up intermediate reply delivery (deferred replies)
	b.agent.SetReplyFunc(func(text string) {
		b.sendReply(qm.msg, qm.userID, text)
	})
	defer b.agent.SetReplyFunc(nil)

	// Set up voice reply delivery (for TTS tool)
	b.agent.SetVoiceReplyFunc(func(oggData []byte) {
		b.sendVoiceNote(qm.msg.Chat.ID, qm.userID, qm.msg.From.UserName, oggData)
	})
	defer b.agent.SetVoiceReplyFunc(nil)

	var response string
	var err error
	if len(qm.images) > 0 {
		// Convert telegram images to agent image data
		agentImages := make([]agent.ImageData, len(qm.images))
		for i, img := range qm.images {
			agentImages[i] = agent.ImageData{MediaType: img.mediaType, Data: img.data}
		}
		response, err = b.agent.HandleMessageWithImages(turnCtx, b.sessionKey, qm.text, agentImages)
	} else {
		response, err = b.agent.HandleMessage(turnCtx, b.sessionKey, qm.text)
	}
	if err != nil {
		if turnCtx.Err() != nil {
			log.Infof("telegram", "agent turn cancelled")
			return // /stop was called, "Stopped." already sent
		}
		log.Errorf("telegram", "agent error: %v", err)
		response = fmt.Sprintf("Error: %v", err)
	}

	// Voice mode: convert final reply to voice note
	if b.agent.VoiceMode(b.sessionKey) && b.tts != nil && response != "" {
		if audioData, err := b.tts.Synthesize(turnCtx, response); err != nil {
			log.Errorf("telegram", "tts for voice mode: %v", err)
			b.sendReply(qm.msg, qm.userID, response) // fall back to text
		} else {
			b.sendVoiceNote(qm.msg.Chat.ID, qm.userID, qm.msg.From.UserName, audioData)
		}
		return
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
// falling back to plain text if markdown fails. Logs each chunk to the
// conversation log.
func (b *Bot) sendReply(msg *tgbotapi.Message, userID string, response string) {
	for _, chunk := range splitMessage(response, 4096) {
		reply := tgbotapi.NewMessage(msg.Chat.ID, chunk)
		reply.ParseMode = "Markdown"
		sendErr := ""
		if _, err := b.sender.Send(reply); err != nil {
			// Retry without markdown if parsing fails
			reply.ParseMode = ""
			if _, err := b.sender.Send(reply); err != nil {
				log.Errorf("telegram", "send error: %v", err)
				sendErr = err.Error()
			}
		}

		log.Conversation(log.ConversationEntry{
			Direction: "sent",
			UserID:    userID,
			Username:  msg.From.UserName,
			ChatID:    msg.Chat.ID,
			Text:      chunk,
			ParseMode: reply.ParseMode,
			Session:   b.sessionKey,
			Error:     sendErr,
		})
	}
}

// SendNotification sends a plain text notification to the last known chat.
// Used for system alerts (cache bust, etc.) — not an agent turn, no tokens spent.
func (b *Bot) SendNotification(text string) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		log.Warnf("telegram", "no chat ID for notification: %s", text)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.sender.Send(msg); err != nil {
		log.Errorf("telegram", "send notification: %v", err)
	}
}

// sendVoiceNote sends audio data as a Telegram voice note.
func (b *Bot) sendVoiceNote(chatID int64, userID string, username string, audioData []byte) {
	voice := tgbotapi.NewVoice(chatID, tgbotapi.FileBytes{
		Name:  "voice.mp3",
		Bytes: audioData,
	})
	if _, err := b.sender.Send(voice); err != nil {
		log.Errorf("telegram", "send voice note: %v", err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    userID,
		Username:  username,
		ChatID:    chatID,
		Text:      fmt.Sprintf("[voice note %d bytes]", len(audioData)),
		Session:   b.sessionKey,
	})
}

// downloadFile downloads a file from Telegram by file ID.
func (b *Bot) downloadFile(fileID string) ([]byte, error) {
	file, err := b.fileGetter.GetFile(tgbotapi.FileConfig{FileID: fileID})
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
