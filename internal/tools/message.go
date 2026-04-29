package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/voice"
)

func NewSendToChatTool(getSender func(sessionKey string) platform.Sender, tts voice.TTS) *Tool {
	return &Tool{
		Name:       "send_to_chat",
		ExecExport: true,
		Positional: []string{"text"},
		StdinParam: "text",
		// description is a more natural flag when paired with a file —
		// "this file with this caption". Maps to the canonical text field
		// so `--description X --file Y` and `--text X --file Y` are
		// equivalent.
		Aliases:     map[string][]string{"text": {"description"}},
		Description: "Send a rich message to the user. Can send text, files, or TTS.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "Text message to send. Supports Markdown formatting."
				},
				"file": {
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text     string `json:"text"`
				FilePath string `json:"file"`
				SendAs   string `json:"send_as"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" && p.FilePath == "" {
				return ToolResult{}, fmt.Errorf("at least one of text or file is required")
			}
			if p.SendAs == "" {
				p.SendAs = "document"
			}

			sessionKey := SessionKeyFromContext(ctx)
			bot := getSender(sessionKey)
			if bot == nil {
				return ToolResult{}, fmt.Errorf("messaging not configured")
			}

			// Extract chat ID from session key for targeted delivery.
			chatID := ChatIDFromSessionKey(sessionKey)

			var sent []string

			// TTS synthesis: send_as="voice" + text + no file
			if p.SendAs == "voice" && p.Text != "" && p.FilePath == "" {
				if tts == nil {
					return ToolResult{}, fmt.Errorf("tts not configured")
				}
				audioData, err := tts.Synthesize(ctx, p.Text)
				if err != nil {
					log.Errorf("voice", "session=%s tts synthesis failed: %v", sessionKey, err)
					return ToolResult{}, fmt.Errorf("tts: %w", err)
				}
				if chatID != 0 {
					err = bot.SendVoiceDataToChat(chatID, audioData)
				} else {
					err = bot.SendVoiceData(audioData)
				}
				if err != nil {
					return ToolResult{}, fmt.Errorf("send voice note: %w", err)
				}
				logToolSend(sessionKey, chatID, "[voice note]")
				return TextResult("Sent: voice note"), nil
			}

			// If the message originates from a different session than the bot's
			// own session, prepend a header so the user knows which session sent it.
			// Placed after TTS early-return so the header isn't synthesized as speech.
			if p.Text != "" && sessionKey != "" {
				botSession := bot.SessionKey()
				if botSession != "" && sessionKey != botSession {
					p.Text = "[[ message from " + sessionKey + " ]]\n" + p.Text
				}
			}

			if p.Text != "" {
				var err error
				if chatID != 0 {
					err = bot.SendTextToChat(chatID, p.Text)
				} else {
					err = bot.SendText(p.Text)
				}
				if err != nil {
					return ToolResult{}, fmt.Errorf("send text: %w", err)
				}
				logToolSend(sessionKey, chatID, p.Text)
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
					return ToolResult{}, fmt.Errorf("send %s: %w", label, err)
				}
				logToolSend(sessionKey, chatID, fmt.Sprintf("[%s %s]", label, p.FilePath))
				sent = append(sent, label)
			}

			return TextResult(fmt.Sprintf("Sent: %s", joinWords(sent))), nil
		},
	}
}

// logToolSend logs a conversation entry for a tool-initiated send.
func logToolSend(sessionKey string, chatID int64, text string) {
	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      text,
		Session:   sessionKey,
	})
}

// ChatIDFromSessionKey extracts the chat ID from a session key.
// Delegates to session.ChatIDFromKey for the actual parsing.
func ChatIDFromSessionKey(key string) int64 {
	return session.ChatIDFromKey(key)
}

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return "nothing"
	case 1:
		return words[0]
	default:
		return strings.Join(words, " + ")
	}
}
