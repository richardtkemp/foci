package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// TelegramSender abstracts the telegram bot methods needed by the send_telegram tool.
type TelegramSender interface {
	// Default-chat methods (send to bot's last known chat).
	SendText(text string) error
	SendDocument(filePath string) error
	SendVoice(filePath string) error
	SendVideo(filePath string) error
	SendPhoto(filePath string) error
	SendAudio(filePath string) error
	SendAnimation(filePath string) error

	// Chat-targeted methods (send to a specific chat ID).
	SendTextToChat(chatID int64, text string) error
	SendDocumentToChat(chatID int64, filePath string) error
	SendVoiceToChat(chatID int64, filePath string) error
	SendVideoToChat(chatID int64, filePath string) error
	SendPhotoToChat(chatID int64, filePath string) error
	SendAudioToChat(chatID int64, filePath string) error
	SendAnimationToChat(chatID int64, filePath string) error
}

// NewSendTelegramTool creates a tool that sends proactive messages, documents,
// or voice notes via Telegram. The getSender callback returns the current bot
// (nil if telegram is not configured).
//
// The tool extracts the chat ID from the session key (format agent:X:chat:CHATID)
// and sends to that specific chat. Falls back to the bot's default chat when the
// session key doesn't contain a chat ID (e.g. spawn branches, cron sessions).
func NewSendTelegramTool(getSender func(sessionKey string) TelegramSender) *Tool {
	return &Tool{
		Name:        "send_telegram",
		ExecExport:  true,
		Description: "Send a proactive Telegram message to the user. Can send text, files, voice notes, videos, photos, audio, or animations. Use for alerts, sharing files, or sending media.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "Text message to send. Supports Markdown formatting."
				},
				"file_path": {
					"type": "string",
					"description": "Path to a file to send as a document attachment."
				},
				"send_as": {
					"type": "string",
					"description": "How to send the file: 'document' (default), 'voice', 'video', 'photo', 'audio', or 'animation' (GIF).",
					"enum": ["document", "voice", "video", "photo", "audio", "animation"]
				}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Text     string `json:"text"`
				FilePath string `json:"file_path"`
				SendAs   string `json:"send_as"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" && p.FilePath == "" {
				return "", fmt.Errorf("at least one of text or file_path is required")
			}
			if p.SendAs == "" {
				p.SendAs = "document"
			}

			sessionKey := SessionKeyFromContext(ctx)
			bot := getSender(sessionKey)
			if bot == nil {
				return "", fmt.Errorf("telegram not configured")
			}

			// Extract chat ID from session key for targeted delivery.
			chatID := chatIDFromSessionKey(sessionKey)

			var sent []string

			if p.Text != "" {
				var err error
				if chatID != 0 {
					err = bot.SendTextToChat(chatID, p.Text)
				} else {
					err = bot.SendText(p.Text)
				}
				if err != nil {
					return "", fmt.Errorf("send text: %w", err)
				}
				sent = append(sent, "text")
			}

			if p.FilePath != "" {
				var err error
				var label string
				switch p.SendAs {
				case "voice":
					if chatID != 0 {
						err = bot.SendVoiceToChat(chatID, p.FilePath)
					} else {
						err = bot.SendVoice(p.FilePath)
					}
					label = "voice note"
				case "video":
					if chatID != 0 {
						err = bot.SendVideoToChat(chatID, p.FilePath)
					} else {
						err = bot.SendVideo(p.FilePath)
					}
					label = "video"
				case "photo":
					if chatID != 0 {
						err = bot.SendPhotoToChat(chatID, p.FilePath)
					} else {
						err = bot.SendPhoto(p.FilePath)
					}
					label = "photo"
				case "audio":
					if chatID != 0 {
						err = bot.SendAudioToChat(chatID, p.FilePath)
					} else {
						err = bot.SendAudio(p.FilePath)
					}
					label = "audio"
				case "animation":
					if chatID != 0 {
						err = bot.SendAnimationToChat(chatID, p.FilePath)
					} else {
						err = bot.SendAnimation(p.FilePath)
					}
					label = "animation"
				default: // "document"
					if chatID != 0 {
						err = bot.SendDocumentToChat(chatID, p.FilePath)
					} else {
						err = bot.SendDocument(p.FilePath)
					}
					label = "document"
				}
				if err != nil {
					return "", fmt.Errorf("send %s: %w", label, err)
				}
				sent = append(sent, label)
			}

			return fmt.Sprintf("Sent: %s", joinWords(sent)), nil
		},
	}
}

// chatIDFromSessionKey extracts the chat ID from a session key with format
// "agent:<name>:chat:<chatID>". Returns 0 if the key doesn't match this format.
func chatIDFromSessionKey(key string) int64 {
	// Match agent:X:chat:CHATID — the chat segment must be the third part.
	parts := strings.Split(key, ":")
	if len(parts) >= 4 && parts[2] == "chat" {
		if id, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
			return id
		}
	}
	return 0
}

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return "nothing"
	case 1:
		return words[0]
	default:
		return words[0] + " + " + words[1]
	}
}
