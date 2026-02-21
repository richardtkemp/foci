package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// TelegramSender abstracts the telegram bot methods needed by the send_telegram tool.
type TelegramSender interface {
	SendText(text string) error
	SendDocument(filePath string) error
	SendVoice(filePath string) error
}

// NewSendTelegramTool creates a tool that sends proactive messages, documents,
// or voice notes via Telegram. The getSender callback returns the current bot
// (nil if telegram is not configured).
func NewSendTelegramTool(getSender func() TelegramSender) *Tool {
	return &Tool{
		Name:        "send_telegram",
		Description: "Send a proactive Telegram message to the user. Can send text, files, or voice notes. Use for alerts, sharing files, or sending voice replies.",
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
				"as_voice": {
					"type": "boolean",
					"description": "If true and file_path is set, send the file as a voice note instead of a document."
				}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Text     string `json:"text"`
				FilePath string `json:"file_path"`
				AsVoice  bool   `json:"as_voice"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" && p.FilePath == "" {
				return "", fmt.Errorf("at least one of text or file_path is required")
			}

			bot := getSender()
			if bot == nil {
				return "", fmt.Errorf("telegram not configured")
			}

			var sent []string

			if p.Text != "" {
				if err := bot.SendText(p.Text); err != nil {
					return "", fmt.Errorf("send text: %w", err)
				}
				sent = append(sent, "text")
			}

			if p.FilePath != "" {
				if p.AsVoice {
					if err := bot.SendVoice(p.FilePath); err != nil {
						return "", fmt.Errorf("send voice: %w", err)
					}
					sent = append(sent, "voice note")
				} else {
					if err := bot.SendDocument(p.FilePath); err != nil {
						return "", fmt.Errorf("send document: %w", err)
					}
					sent = append(sent, "document")
				}
			}

			return fmt.Sprintf("Sent: %s", joinWords(sent)), nil
		},
	}
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
