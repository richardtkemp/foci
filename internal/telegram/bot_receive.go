package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"foci/internal/dispatch"
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
	b.mq.Enqueue(b.toPlatformMessage(msg, qm))
}

// toPlatformMessage converts a telegram queuedMessage to a platform.QueuedMessage.
func (b *Bot) toPlatformMessage(msg *gotgbot.Message, qm queuedMessage) platform.QueuedMessage {
	var atts []platform.Attachment
	for _, a := range qm.attachments {
		atts = append(atts, platform.Attachment{
			MimeType:  a.mediaType,
			Data:      a.data,
			SavedPath: a.savedPath,
		})
	}

	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	isMention := isGroup && b.messageContainsMention(msg)

	senderName := ""
	if msg.From != nil {
		senderName = msg.From.Username
		if senderName == "" {
			senderName = msg.From.FirstName
		}
	}

	return platform.QueuedMessage{
		UserID:      qm.userID,
		SenderName:  senderName,
		Text:        qm.text,
		Attachments: atts,
		ChatID:      msg.Chat.Id,
		IsGroupChat: isGroup,
		IsMention:   isMention,
		Original:    msg,
	}
}

// buildReceivedMessage performs auth, text extraction, and attachment downloading.
// Returns a populated queuedMessage and true, or zero value and false if the
// message should be silently dropped (unauthorized, empty, or failed voice).
func (b *Bot) buildReceivedMessage(ctx context.Context, msg *gotgbot.Message) (queuedMessage, bool) {
	userID := fmt.Sprintf("%d", msg.From.Id)

	if len(b.allowedUsers) > 0 && !b.allowedUsers[userID] {
		b.logger().Warnf("rejected message from %s", formatUserInfo(msg.From))
		return queuedMessage{}, false
	}

	// Remember chat ID for notifications (cache bust alerts, etc.)
	b.chatMu.Lock()
	changed := b.chatID != msg.Chat.Id
	b.chatID = msg.Chat.Id
	b.chatMu.Unlock()

	_ = changed // chatID tracked in-memory only; DB is source of truth for default

	// Per-chat session routing: set default chat on first message, record username
	if !b.isSecondary && b.agentID != "" && b.sessionIndex != nil {
		if chatID := b.sessionIndex.DefaultChatForAgent(b.agentID, platformName); chatID == 0 {
			if err := b.sessionIndex.SetDefaultChat(b.agentID, platformName, msg.Chat.Id); err != nil {
				b.logger().Errorf("set default chat: %v", err)
			} else {
				b.logger().Infof("set default chat %d for agent %s", msg.Chat.Id, b.agentID)
			}
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
	interceptor := dispatch.Interceptor{
		Commands:     b.commands,
		LastMsgStore: b.lastMsgStore,
		Handler:      b.handler,
		Dispatcher:   b.dispatcher.Inner(),
		IsSecondary:  b.isSecondary,
		SessionKeyFn: b.SessionKey,
		LogWarnf:     func(f string, a ...any) { b.logger().Warnf(f, a...) },
		LogDebugf:    func(f string, a ...any) { b.logger().Debugf(f, a...) },
	}
	result := interceptor.TryIntercept(ctx, &dispatch.InterceptMessage{
		Text:      qm.text,
		UserID:    qm.userID,
		ChatID:    qm.msg.Chat.Id,
		Timestamp: time.Unix(int64(qm.msg.Date), 0),
	})
	if !result.Consumed {
		return false
	}
	if result.WizardReply != "" {
		b.sendReply(qm.msg, result.WizardReply)
		return true
	}
	if result.Outcome != nil {
		b.renderCommandOutcome(qm.msg, result.Outcome)
		return true
	}
	return true // silently consumed (stale command or idle secondary)
}

