package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func formatUserInfo(user *gotgbot.User) string {
	id := fmt.Sprintf("%d", user.Id)
	if user.Username != "" {
		return fmt.Sprintf("%s (%s)", id, user.Username)
	}
	if user.FirstName != "" {
		return fmt.Sprintf("%s (%s)", id, user.FirstName)
	}
	return id
}

func (b *Bot) receiveMessage(ctx context.Context, msg *gotgbot.Message) {
	qm, ok := b.buildReceivedMessage(ctx, msg)
	if !ok {
		return
	}
	if b.tryIntercept(ctx, &qm) {
		return
	}
	b.enqueue(qm)
}

// buildReceivedMessage performs auth, text extraction, and attachment downloading.
// Returns a populated queuedMessage and true, or zero value and false if the
// message should be silently dropped (unauthorized, empty, or failed voice).
func (b *Bot) buildReceivedMessage(ctx context.Context, msg *gotgbot.Message) (queuedMessage, bool) {
	userID := fmt.Sprintf("%d", msg.From.Id)

	if !b.allowedUsers[userID] {
		b.logger().Warnf("rejected message from %s", formatUserInfo(msg.From))
		return queuedMessage{}, false
	}

	// Remember chat ID for notifications (cache bust alerts, etc.)
	b.chatMu.Lock()
	changed := b.chatID != msg.Chat.Id
	b.chatID = msg.Chat.Id
	b.chatMu.Unlock()

	if changed && b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetAgentMetadata(b.agentID, "bot_chat_id", fmt.Sprintf("%d", msg.Chat.Id)); err != nil {
			b.logger().Errorf("persist chat ID: %v", err)
		}
	}

	// Per-chat session routing: set default chat on first message, record username
	if !b.isSecondary && b.agentID != "" {
		if b.defaultChatID() == 0 {
			b.setDefaultChat(msg.Chat.Id)
			b.logger().Infof("set default chat %d for agent %s", msg.Chat.Id, b.agentID)
		}
		if msg.From != nil {
			b.recordChatUsername(msg.Chat.Id, msg.From.Username)
		}
	}

	// Record last real user activity (for --if-active gating on CLI commands).
	// Only primary bots track this — secondary (facet) bots don't count.
	if !b.isSecondary && b.agentID != "" && b.sessionIndex != nil {
		_ = b.sessionIndex.SetAgentMetadata(b.agentID, "last_user_activity", fmt.Sprintf("%d", time.Now().Unix()))
	}
	if b.OnUserMessage != nil {
		b.OnUserMessage()
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
			b.logger().Errorf("download voice: %s", b.sanitizeError(err))
			if b.handler == nil || b.handler.Warnings() == nil {
				b.sendReply(msg, "Could not download voice note — please try again.")
			}
		} else {
			transcript, err := b.transcriber.Transcribe(ctx, data, "voice.ogg")
			if err != nil {
				b.logger().Errorf("transcribe voice: %v", err)
				b.sendReply(msg, "Could not transcribe voice note.")
				return queuedMessage{}, false
			}
			b.logger().Infof("voice transcription from %s: %s", formatUserInfo(msg.From), truncate(transcript, 100))
			text = "[voice] " + transcript
		}
	} else if msg.Voice != nil && b.transcriber == nil {
		b.sendReply(msg, "Voice notes require an STT provider. Set groq.api_key in secrets.toml or configure [voice] stt_endpoint.")
		return queuedMessage{}, false
	}

	// Download attachments from photos or documents
	var attachments []attachment
	if len(msg.Photo) > 0 {
		// Take the largest photo (last in the array)
		photo := msg.Photo[len(msg.Photo)-1]
		if att, ok := b.downloadAttachment(photo.FileId, "image/jpeg", msg.Chat.Id); ok {
			attachments = append(attachments, att)
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id); ok {
			attachments = append(attachments, att)
		}
	} else if msg.Document != nil && isPDFMIME(msg.Document.MimeType) {
		// PDFs under 32MB go through the content-block path (like images);
		// over-size PDFs fall back to save-to-disk via handleMediaMessage.
		const maxPDFSize = 32 * 1024 * 1024
		if msg.Document.FileSize > 0 && msg.Document.FileSize > maxPDFSize {
			text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "PDF", msg.Chat.Id, ".pdf")
		} else if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id); ok {
			if len(att.data) > maxPDFSize {
				// Downloaded size exceeded limit — save to disk instead
				if att.savedPath != "" {
					text = fmt.Sprintf("[PDF saved to: %s]\n\n%s", att.savedPath, text)
				}
			} else {
				attachments = append(attachments, att)
			}
		}
	} else if msg.Document != nil && platform.IsConvertibleDocMIME(msg.Document.MimeType) {
		// Convertible documents (docx, xlsx, pptx, HTML, CSV, plain text):
		// download and pass through the attachment pipeline for text conversion.
		// Use normalized MIME so the agent layer sees a canonical type.
		normalizedMIME := platform.NormalizeMIME(msg.Document.MimeType)
		if att, ok := b.downloadAttachment(msg.Document.FileId, normalizedMIME, msg.Chat.Id); ok {
			attachments = append(attachments, att)
		}
	}

	// Handle video messages
	if msg.Video != nil {
		text = b.handleMediaMessage(text, msg.Video.FileId, msg.Video.FileSize, "video", "Video", msg.Chat.Id, extForVideo(msg.Video.MimeType))
	}

	// Handle video notes (circular video messages)
	if msg.VideoNote != nil {
		text = b.handleMediaMessage(text, msg.VideoNote.FileId, msg.VideoNote.FileSize, "videonote", "Video", msg.Chat.Id, ".mp4")
	}

	// Handle remaining document types (not image, not PDF, not convertible)
	if msg.Document != nil && !isImageMIME(msg.Document.MimeType) && !isPDFMIME(msg.Document.MimeType) && !platform.IsConvertibleDocMIME(msg.Document.MimeType) {
		ext := filepath.Ext(msg.Document.FileName)
		if ext == "" {
			ext = extForMIME(msg.Document.MimeType)
		}
		text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "Document", msg.Chat.Id, ext)
	}

	// Drop messages with no text and no attachments
	if text == "" && len(attachments) == 0 {
		return queuedMessage{}, false
	}

	logText := text
	if len(attachments) > 0 {
		logText = fmt.Sprintf("[%d attachment(s)] %s", len(attachments), text)
	}
	if b.display.MessagesInLog {
		b.logger().Infof("message from %s: %s", formatUserInfo(msg.From), truncate(logText, 100))
	} else {
		b.logger().Debugf("message from %s", formatUserInfo(msg.From))
	}

	return queuedMessage{msg: msg, userID: userID, text: text, attachments: attachments}, true
}

// tryIntercept handles local consumption of a message: wizard intercept,
// last-message recording, stale command drops, command dispatch, message
// transforms, and secondary bot idle drops. Returns true if the message
// was consumed and should not be enqueued.
func (b *Bot) tryIntercept(ctx context.Context, qm *queuedMessage) bool {
	// Wizard intercept — route all messages to active wizard before normal dispatch
	if qm.text != "" {
		if result, ok := b.commands.HandleMessage(qm.text); ok {
			b.sendReply(qm.msg, result)
			return true
		}
	}

	// Record the message for // (repeat) command
	if qm.text != "" && !strings.HasPrefix(qm.text, "/") {
		b.lastMsgStore.Record(qm.userID, qm.text)
	}

	// Drop stale slash commands (e.g. /restart replayed from the update
	// queue after a crash). Agent messages are still delivered since the
	// agent can reason about timeliness, but slash commands execute
	// unconditionally so stale ones must be dropped.
	if qm.text != "" && strings.HasPrefix(qm.text, "/") {
		if age := time.Since(time.Unix(int64(qm.msg.Date), 0)); age > 30*time.Second {
			b.logger().Warnf("dropping stale command %q (age=%s)", strings.ToLower(qm.text), age.Truncate(time.Second))
			return true
		}
	}

	// Try dispatching the original message as a command (slash or dot-prefix).
	if b.tryDispatchCommand(ctx, qm.msg, qm.text) {
		return true
	}

	// Apply message transforms to non-command messages.
	// Transforms may produce a command (e.g. "m" → "/mana").
	if b.handler != nil {
		if transformed := b.handler.TransformMessage(qm.text); transformed != qm.text {
			qm.text = transformed
			if b.tryDispatchCommand(ctx, qm.msg, qm.text) {
				return true
			}
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	// Replying would cause spurious "idle" messages on restart when stale
	// Telegram updates are replayed.
	if b.isSecondary && b.SessionKey() == "" {
		b.logger().Debugf("dropping message to idle secondary bot from %s", formatUserInfo(qm.msg.From))
		return true
	}

	return false
}

// enqueue delivers the message to the steer buffer (if a turn is active and
// steer mode is on) or to the main message queue.
func (b *Bot) enqueue(qm queuedMessage) {
	// Steer mode: if a turn is active, route text to the steer buffer
	// so it gets injected between tool calls instead of queuing behind the turn lock.
	if b.display.SteerMode && qm.text != "" && len(qm.attachments) == 0 {
		b.turnMu.Lock()
		active := b.turnCancel != nil
		b.turnMu.Unlock()
		if active {
			b.appendSteer(qm.text)
			b.logger().Infof("steer: buffered message from %s", formatUserInfo(qm.msg.From))
			return
		}
	}

	select {
	case b.queue <- qm:
	default:
		b.logger().Warnf("message queue full, dropping message from %s", formatUserInfo(qm.msg.From))
		b.sendReply(qm.msg, "Busy — message queue is full. Try again shortly.")
	}
}
