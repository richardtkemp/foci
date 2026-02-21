package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"

	"clod/agent"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot wraps the Telegram bot API with agent integration.
type Bot struct {
	api          *tgbotapi.BotAPI
	agent        *agent.Agent
	allowedUsers map[string]bool
	sessionKey   string
}

// NewBot creates a new Telegram bot.
func NewBot(token string, allowedUsers []string, ag *agent.Agent, sessionKey string) (*Bot, error) {
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
		allowedUsers: allowed,
		sessionKey:   sessionKey,
	}, nil
}

// Run starts the long-polling loop. Blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	log.Printf("[telegram] bot started as @%s", b.api.Self.UserName)

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
			go b.handleMessage(ctx, update.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	userID := fmt.Sprintf("%d", msg.From.ID)

	if !b.allowedUsers[userID] {
		log.Printf("[telegram] rejected message from user %s (%s)", userID, msg.From.UserName)
		return
	}

	text := msg.Text
	if text == "" {
		return
	}

	log.Printf("[telegram] message from %s: %s", msg.From.UserName, truncate(text, 100))

	// Send typing indicator
	typing := tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	response, err := b.agent.HandleMessage(ctx, b.sessionKey, text)
	if err != nil {
		log.Printf("[telegram] error: %v", err)
		response = fmt.Sprintf("Error: %v", err)
	}

	// Split long messages (Telegram limit is 4096 chars)
	for _, chunk := range splitMessage(response, 4096) {
		reply := tgbotapi.NewMessage(msg.Chat.ID, chunk)
		reply.ParseMode = "Markdown"
		if _, err := b.api.Send(reply); err != nil {
			// Retry without markdown if parsing fails
			reply.ParseMode = ""
			if _, err := b.api.Send(reply); err != nil {
				log.Printf("[telegram] send error: %v", err)
			}
		}
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
