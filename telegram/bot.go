package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"clod/agent"
	"clod/command"
	"clod/log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg    *tgbotapi.Message
	userID string
	text   string
}

// Bot wraps the Telegram bot API with agent integration.
// Messages are received on one goroutine and processed on another.
// Slash commands execute immediately on the receiver goroutine.
type Bot struct {
	api          *tgbotapi.BotAPI
	agent        *agent.Agent
	commands     *command.Registry
	allowedUsers map[string]bool
	sessionKey   string

	queue      chan queuedMessage     // receiver → agent worker
	turnCancel context.CancelFunc     // cancel the current agent turn
	turnMu     sync.Mutex             // protects turnCancel
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
		agent:        ag,
		commands:     cmds,
		allowedUsers: allowed,
		sessionKey:   sessionKey,
		queue:        make(chan queuedMessage, 64),
	}, nil
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

	text := msg.Text
	if text == "" {
		return
	}

	log.Infof("telegram", "message from %s: %s", msg.From.UserName, truncate(text, 100))

	// Log received message
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    userID,
		Username:  msg.From.UserName,
		ChatID:    msg.Chat.ID,
		Text:      text,
		Session:   b.sessionKey,
	})

	// Slash commands bypass the agent pipeline entirely
	if strings.HasPrefix(text, "/") {
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
	case b.queue <- queuedMessage{msg: msg, userID: userID, text: text}:
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
	b.api.Send(typing)

	response, err := b.agent.HandleMessage(turnCtx, b.sessionKey, qm.text)
	if err != nil {
		if turnCtx.Err() != nil {
			log.Infof("telegram", "agent turn cancelled")
			return // /stop was called, "Stopped." already sent
		}
		log.Errorf("telegram", "agent error: %v", err)
		response = fmt.Sprintf("Error: %v", err)
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
		if _, err := b.api.Send(reply); err != nil {
			// Retry without markdown if parsing fails
			reply.ParseMode = ""
			if _, err := b.api.Send(reply); err != nil {
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
