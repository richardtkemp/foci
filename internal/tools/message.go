package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/voice"
)

type MessageSender = platform.Sender

func NewSendMessageToUserTool(getSender func(sessionKey string) MessageSender, tts voice.TTS) *Tool {
	return &Tool{
		Name:        "send_message_to_user",
		ExecExport:  true,
		Description: "Send a proactive message to the user. Can send text, files, voice notes, videos, photos, audio, or animations. Use send_as=\"voice\" with text (no file_path) to synthesize speech and send as a voice note. Use for alerts, sharing files, or sending media.",
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text     string `json:"text"`
				FilePath string `json:"file_path"`
				SendAs   string `json:"send_as"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" && p.FilePath == "" {
				return ToolResult{}, fmt.Errorf("at least one of text or file_path is required")
			}
			if p.SendAs == "" {
				p.SendAs = "document"
			}

			sessionKey := SessionKeyFromContext(ctx)
			bot := getSender(sessionKey)
			if bot == nil {
				return ToolResult{}, fmt.Errorf("messaging not configured")
			}

			// If the message originates from a different session than the bot's
			// own session, prepend a header so the user knows which session sent it.
			if p.Text != "" && sessionKey != "" {
				botSession := bot.SessionKey()
				if botSession != "" && sessionKey != botSession {
					p.Text = "[[ message from " + sessionKey + " ]]\n" + p.Text
				}
			}

			// Extract chat ID from session key for targeted delivery.
			chatID := ChatIDFromSessionKey(sessionKey)

			var sent []string

			// TTS synthesis: send_as="voice" + text + no file_path
			if p.SendAs == "voice" && p.Text != "" && p.FilePath == "" {
				if tts == nil {
					return ToolResult{}, fmt.Errorf("tts not configured")
				}
				audioData, err := tts.Synthesize(ctx, p.Text)
				if err != nil {
					log.Errorf("voice", "tts synthesis failed: %v", err)
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
				return TextResult("Sent: voice note"), nil
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
				sent = append(sent, label)
			}

			return TextResult(fmt.Sprintf("Sent: %s", joinWords(sent))), nil
		},
	}
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
		return words[0] + " + " + words[1]
	}
}
