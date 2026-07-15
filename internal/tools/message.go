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
	"foci/internal/tempdir"
	"foci/internal/voice"
)

func NewSendToChatTool(getSender func(sessionKey string) platform.Sender, tts func() voice.TTS, sessionTypeFn func(sessionKey string) session.SessionType) *Tool {
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
					"description": "Path to a file to send as a document attachment. Relative paths are resolved against the caller's working directory; absolute paths are passed through unchanged. Pass '-' to read the attachment body from stdin (e.g. pipe a file in: cat plan.md | foci_send_to_chat 'caption' --file - ); pair with --filename to set the display name."
				},
				"filename": {
					"type": "string",
					"description": "Optional display name for the attachment (overrides the file's basename). Useful when the source path has a temp/internal name. Path components are stripped — basename only."
				},
				"send_as": {
					"type": "string",
					"description": "How to send the file: 'document' (default), 'voice', 'video', 'photo', 'audio', or 'animation' (GIF). This is Telegram/Discord DELIVERY semantics only — it picks the native attachment type. The foci app ignores it: the app renders inline images and the media gallery purely by the file's MIME type, so a PNG sent as 'document' and one sent as 'photo' render identically there. TTS mode (no file): 'voice' with 'text' and no 'file' synthesizes the text to speech and sends it as a voice note.",
					"enum": ["document", "voice", "video", "photo", "audio", "animation"]
				}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return sendToChatExecute(ctx, params, getSender, tts, sessionTypeFn)
		},
	}
}

// messageTarget binds a Sender to a resolved chat so send sites don't each
// repeat the "if chatID != 0 { …ToChat(chatID, …) } else { …(…) }" branch.
// A zero chatID means "use the sender's default chat".
type messageTarget struct {
	bot    platform.Sender
	chatID int64
}

func (m messageTarget) text(text string) error {
	if m.chatID != 0 {
		return m.bot.SendTextToChat(m.chatID, text)
	}
	return m.bot.SendText(text)
}

func (m messageTarget) voiceData(audioData []byte) error {
	if m.chatID != 0 {
		return m.bot.SendVoiceDataToChat(m.chatID, audioData)
	}
	return m.bot.SendVoiceData(audioData)
}

// file sends filePath according to sendAs and returns the human label used for
// logging and the result message. Unknown kinds fall back to "document".
func (m messageTarget) file(sendAs, filePath, caption string) (string, error) {
	fs, ok := fileSenders[sendAs]
	if !ok {
		fs = fileSenders["document"]
	}
	if m.chatID != 0 {
		return fs.label, fs.chat(m.bot, m.chatID, filePath, caption)
	}
	return fs.label, fs.def(m.bot, filePath, caption)
}

// fileSenders maps a send_as kind to its label and the two Sender methods
// (default chat vs targeted chat). This is data, not control flow — it replaces
// the former six-case switch that duplicated the chatID branch in every arm.
// voice carries no caption, so its closures ignore the caption argument.
var fileSenders = map[string]struct {
	label string
	def   func(b platform.Sender, filePath, caption string) error
	chat  func(b platform.Sender, chatID int64, filePath, caption string) error
}{
	"voice": {"voice note",
		func(b platform.Sender, p, _ string) error { return b.SendVoice(p) },
		func(b platform.Sender, id int64, p, _ string) error { return b.SendVoiceToChat(id, p) }},
	"video": {"video",
		func(b platform.Sender, p, c string) error { return b.SendVideo(p, c) },
		func(b platform.Sender, id int64, p, c string) error { return b.SendVideoToChat(id, p, c) }},
	"photo": {"photo",
		func(b platform.Sender, p, c string) error { return b.SendPhoto(p, c) },
		func(b platform.Sender, id int64, p, c string) error { return b.SendPhotoToChat(id, p, c) }},
	"audio": {"audio",
		func(b platform.Sender, p, c string) error { return b.SendAudio(p, c) },
		func(b platform.Sender, id int64, p, c string) error { return b.SendAudioToChat(id, p, c) }},
	"animation": {"animation",
		func(b platform.Sender, p, c string) error { return b.SendAnimation(p, c) },
		func(b platform.Sender, id int64, p, c string) error { return b.SendAnimationToChat(id, p, c) }},
	"document": {"document",
		func(b platform.Sender, p, c string) error { return b.SendDocument(p, c) },
		func(b platform.Sender, id int64, p, c string) error { return b.SendDocumentToChat(id, p, c) }},
}

// prepareNamedFile gives filePath a custom display basename by symlinking it
// into a per-call temp dir under name, then returning the symlink path plus a
// cleanup func to remove the temp dir. Platform send methods label attachments
// via filepath.Base, so the symlink name becomes the displayed filename; the
// symlink is followed by os.Open, so size/mime detection still sees the real
// file. When name is empty this is a no-op (returns filePath and a no-op
// cleanup). The caller must defer the returned cleanup.
func prepareNamedFile(filePath, name string) (string, func(), error) {
	noop := func() {}
	if name == "" {
		return filePath, noop, nil
	}
	cleanName := filepath.Base(name) // strip any path components defensively
	if cleanName == "." || cleanName == "/" || cleanName == "" {
		return "", noop, fmt.Errorf("invalid filename %q", name)
	}
	absSrc, err := filepath.Abs(filePath)
	if err != nil {
		return "", noop, fmt.Errorf("resolve source path: %w", err)
	}
	tmpDir, err := tempdir.Mkdir("foci-send-")
	if err != nil {
		return "", noop, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	linkPath := filepath.Join(tmpDir, cleanName)
	if err := os.Symlink(absSrc, linkPath); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("create filename symlink: %w", err)
	}
	return linkPath, cleanup, nil
}

func sendToChatExecute(ctx context.Context, params json.RawMessage, getSender func(sessionKey string) platform.Sender, ttsFn func() voice.TTS, sessionTypeFn func(sessionKey string) session.SessionType) (ToolResult, error) {
	p, err := UnmarshalParams[struct {
		Text     string `json:"text"`
		FilePath string `json:"file"`
		Filename string `json:"filename"`
		SendAs   string `json:"send_as"`
	}](params)
	if err != nil {
		return ToolResult{}, err
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

	// Custom display filename via a per-call symlink (no-op when unset).
	sendPath, cleanup, err := prepareNamedFile(p.FilePath, p.Filename)
	if err != nil {
		return ToolResult{}, err
	}
	defer cleanup()
	p.FilePath = sendPath

	sessionKey := SessionKeyFromContext(ctx)
	bot := getSender(sessionKey)
	if bot == nil {
		return ToolResult{}, fmt.Errorf("messaging not configured")
	}
	chatID := ChatIDFromSessionKey(sessionKey)

	// Facet sessions deliver to their own conversation (the facet bot's
	// session), not the parent/root chat. Non-facet branches (spawn, cron,
	// etc.) resolve to the root chat as before.
	if sessionTypeFn != nil && sessionTypeFn(sessionKey) == session.SessionTypeFacet {
		chatID = 0
	}

	target := messageTarget{bot: bot, chatID: chatID}

	// TTS synthesis: send_as="voice" + text + no file.
	if p.SendAs == "voice" && p.Text != "" && p.FilePath == "" {
		var tts voice.TTS
		if ttsFn != nil {
			tts = ttsFn()
		}
		if tts == nil {
			return ToolResult{}, fmt.Errorf("tts not configured")
		}
		audioData, err := tts.Synthesize(ctx, p.Text)
		if err != nil {
			return ToolResult{}, fmt.Errorf("tts: %w", err)
		}
		if err := target.voiceData(audioData); err != nil {
			return ToolResult{}, fmt.Errorf("send voice note: %w", err)
		}
		logToolSend(sessionKey, target.chatID, "[voice note]")
		return TextResult("Sent: voice note"), nil
	}

	// If the message originates from a different session than the bot's own
	// session, prepend a header so the user knows which session sent it.
	// Placed after the TTS early-return so the header isn't spoken.
	if p.Text != "" && sessionKey != "" {
		botSession := bot.SessionKey()
		if botSession != "" && sessionKey != botSession {
			p.Text = "[[ message from " + sessionKey + " ]]\n" + p.Text
		}
	}

	// Decide whether text rides along as a caption on the file or goes as a
	// separate text message. send_as=voice doesn't caption (foci's voice path
	// is for raw voice notes, no caption param plumbed); long text falls back
	// to a separate message because Telegram caps captions at 1024 chars and
	// Discord at 2000.
	canCaption := p.FilePath != "" &&
		p.SendAs != "voice" &&
		p.Text != "" &&
		len(p.Text) <= platform.MaxCaptionLen
	caption := ""
	if canCaption {
		caption = p.Text
	}

	var sent []string

	// Standalone text — only when not riding as caption.
	if p.Text != "" && !canCaption {
		if err := target.text(p.Text); err != nil {
			return ToolResult{}, fmt.Errorf("send text: %w", err)
		}
		logToolSend(sessionKey, target.chatID, p.Text)
		sent = append(sent, "text")
	}

	if p.FilePath != "" {
		label, err := target.file(p.SendAs, p.FilePath, caption)
		if err != nil {
			return ToolResult{}, fmt.Errorf("send %s: %w", label, err)
		}
		summary := filepath.Base(p.FilePath)
		if caption != "" {
			logToolSend(sessionKey, target.chatID, fmt.Sprintf("[%s %s + caption: %s]", label, p.FilePath, caption))
			sent = append(sent, summary+"+caption")
		} else {
			logToolSend(sessionKey, target.chatID, fmt.Sprintf("[%s %s]", label, p.FilePath))
			sent = append(sent, summary)
		}
	}

	return TextResult(fmt.Sprintf("Sent: %s", joinWords(sent))), nil
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
