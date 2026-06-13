package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/convo"
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
					"description": "Text message to send. Supports Markdown formatting. When piped on stdin and --text/--description is omitted, stdin populates this field."
				},
				"file": {
					"type": "string",
					"format": "filepath",
					"description": "Path to a file to send as a document attachment. Relative paths are resolved against the caller's working directory; absolute paths are passed through unchanged."
				},
				"filename": {
					"type": "string",
					"description": "Optional display name for the attachment (overrides the file's basename). Useful when the source path has a temp/internal name. Path components are stripped — basename only."
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
				Filename string `json:"filename"`
				SendAs   string `json:"send_as"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.Text == "" && p.FilePath == "" {
				return ToolResult{}, fmt.Errorf("at least one of text or file is required")
			}
			if p.Filename != "" && p.FilePath == "" {
				return ToolResult{}, fmt.Errorf("filename requires file")
			}
			if p.SendAs == "" {
				p.SendAs = "document"
			}

			// Custom display filename: symlink the source into a per-call temp
			// dir under the desired basename, then send the symlink. Platform
			// implementations use filepath.Base(path) to label the attachment,
			// so the symlink name becomes the displayed filename. Symlinks are
			// followed by os.Open, so size/mime detection still sees the real
			// file. Per-call temp dir avoids collisions on concurrent sends.
			if p.Filename != "" {
				cleanName := filepath.Base(p.Filename) // strip any path components defensively
				if cleanName == "." || cleanName == "/" || cleanName == "" {
					return ToolResult{}, fmt.Errorf("invalid filename %q", p.Filename)
				}
				absSrc, err := filepath.Abs(p.FilePath)
				if err != nil {
					return ToolResult{}, fmt.Errorf("resolve source path: %w", err)
				}
				tmpDir, err := os.MkdirTemp("", "foci-send-")
				if err != nil {
					return ToolResult{}, fmt.Errorf("create temp dir: %w", err)
				}
				defer func() { _ = os.RemoveAll(tmpDir) }()
				linkPath := filepath.Join(tmpDir, cleanName)
				if err := os.Symlink(absSrc, linkPath); err != nil {
					return ToolResult{}, fmt.Errorf("create filename symlink: %w", err)
				}
				p.FilePath = linkPath
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

			// Decide whether text rides along as a caption on the file
			// or goes as a separate text message. send_as=voice doesn't
			// caption (foci's voice path is for raw voice notes, no
			// caption param plumbed); long text falls back to a
			// separate message because Telegram caps captions at 1024
			// chars and Discord at 2000.
			canCaption := p.FilePath != "" &&
				p.SendAs != "voice" &&
				p.Text != "" &&
				len(p.Text) <= platform.MaxCaptionLen
			caption := ""
			if canCaption {
				caption = p.Text
			}

			// Standalone text — only when not riding as caption.
			if p.Text != "" && !canCaption {
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
						err = bot.SendVideoToChat(chatID, p.FilePath, caption)
					} else {
						err = bot.SendVideo(p.FilePath, caption)
					}
					label = "video"
				case "photo":
					if chatID != 0 {
						err = bot.SendPhotoToChat(chatID, p.FilePath, caption)
					} else {
						err = bot.SendPhoto(p.FilePath, caption)
					}
					label = "photo"
				case "audio":
					if chatID != 0 {
						err = bot.SendAudioToChat(chatID, p.FilePath, caption)
					} else {
						err = bot.SendAudio(p.FilePath, caption)
					}
					label = "audio"
				case "animation":
					if chatID != 0 {
						err = bot.SendAnimationToChat(chatID, p.FilePath, caption)
					} else {
						err = bot.SendAnimation(p.FilePath, caption)
					}
					label = "animation"
				default: // "document"
					if chatID != 0 {
						err = bot.SendDocumentToChat(chatID, p.FilePath, caption)
					} else {
						err = bot.SendDocument(p.FilePath, caption)
					}
					label = "document"
				}
				if err != nil {
					return ToolResult{}, fmt.Errorf("send %s: %w", label, err)
				}
				if caption != "" {
					logToolSend(sessionKey, chatID, fmt.Sprintf("[%s %s + caption: %s]", label, p.FilePath, caption))
					sent = append(sent, label+"+caption")
				} else {
					logToolSend(sessionKey, chatID, fmt.Sprintf("[%s %s]", label, p.FilePath))
					sent = append(sent, label)
				}
			}

			return TextResult(fmt.Sprintf("Sent: %s", joinWords(sent))), nil
		},
	}
}

// logToolSend logs a conversation entry for a tool-initiated send.
func logToolSend(sessionKey string, chatID int64, text string) {
	convo.Record(convo.Entry{
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
