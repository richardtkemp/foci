package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"foci/internal/dispatch"
	"foci/internal/platform"
	"foci/internal/timeutil"

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
	// A bare /steer or /queue (no message text) is not a prefix and not a
	// command: reply with the one-line usage and deliver nothing (#1318).
	// Runs BEFORE the command gate so the two words never reach the command
	// channel, and only when the message is addressed — in a require-mention
	// group an un-mentioned bare prefix is ignored exactly like any other
	// unaddressed message.
	if qm.bareSteerPrefix {
		if !b.mq.IsAddressed(b.toPlatformMessage(msg, qm)) {
			return
		}
		b.sendReply(msg, platform.SteerPrefixUsage)
		return
	}
	// Non-immediate commands go to the command channel so the worker dispatches
	// them without blocking the polling goroutine. This prevents a deadlock where
	// a long command (e.g. /reset) blocks getUpdates, preventing callback_query
	// delivery for interactive prompts like permission "Allow" buttons.
	// /stop is kept here because it must cancel a live turn immediately.
	// A bare /cancel whose chat has an active wizard is also kept here: there is
	// no /cancel command, so the command channel would answer "unknown command"
	// and strand the wizard — the intercept cancels it instead.
	if dispatch.IsRoutableCommand(qm.text, b.commands) && !b.commands.IsImmediateText(qm.text) &&
		!b.dispatcher.IsWizardCancel(msg.Chat.Id, qm.text) {
		ts := time.Unix(int64(msg.Date), 0)
		if !ts.IsZero() && time.Since(ts) > dispatch.StaleCommandAge {
			b.logger().Warnf("dropping stale command %q (age=%s)", qm.text, time.Since(ts).Truncate(time.Second))
			return
		}
		b.mq.EnqueueCommand(b.toPlatformMessage(msg, qm))
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
		ReceivedAt:  time.Unix(int64(msg.Date), 0),
		Voice:       qm.voice,
		Steer:       qm.steer,
	}
}

// buildReceivedMessage performs auth, text extraction, and attachment downloading.
// Returns a populated queuedMessage and true, or zero value and false if the
// message should be silently dropped (unauthorized, empty, or failed voice).
func (b *Bot) buildReceivedMessage(ctx context.Context, msg *gotgbot.Message) (queuedMessage, bool) {
	// Anonymous / post-as-channel messages have no From. There is no sender to
	// authorize, so drop them — and this avoids a nil-deref on msg.From.Id below
	// (pre-auth DoS on the poll path).
	if msg.From == nil {
		b.logger().Debugf("dropping message with no sender (chat %d)", msg.Chat.Id)
		return queuedMessage{}, false
	}
	userID := fmt.Sprintf("%d", msg.From.Id)

	if !platform.UserAllowed(b.allowedUsers, b.allowedUsersOnly, userID) {
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

	// Last-user-activity is recorded per-session in the turn path (see
	// Agent.touchUserActivity, gated on isInteractiveTrigger) — not here. That keeps it
	// keyed by the session the message lands on, includes facet/secondary bots,
	// and derives the agent-level value as a max over sessions.
	if b.OnUserMessage != nil {
		b.OnUserMessage()
	}

	// Get text from message or caption (photos use caption); each text form
	// has its own entity list, indexing that text.
	text, entities := msg.Text, msg.Entities
	if text == "" {
		text, entities = msg.Caption, msg.CaptionEntities
	}

	// /steer|/queue delivery-mode prefix (#1318): parse the user's own text
	// BEFORE the reply/quote header is prepended (a reply whose text is
	// "/queue x" still parses — the header would mask it) and strip the
	// prefix, so the payload reaches the agent alone, carrying the choice on
	// the queued message. A bare prefix (no message text) keeps the text and
	// flags the bare form for receiveMessage's usage reply. The leading
	// addressing tokens go first — the mention strip is entity-based, so it
	// must run before any text edit shifts entity offsets, and it is what
	// makes "@mybot /queue x" parse (the discord twin of this path strips
	// mentions before parsing too).
	text = b.stripSelfMention(text, entities)
	text = b.stripSteerTarget(text)
	steer := platform.SteerDefault
	bareSteerPrefix := false
	if pref, rest, matched := platform.ParseSteerPrefix(text); matched {
		if rest == "" {
			bareSteerPrefix = true
		} else {
			text, steer = rest, pref
		}
	}

	// Include quoted message context when user replies to a specific message.
	// Prefer the specific quote text (user highlighted a portion) over the
	// full replied-to message, which may be very long.
	// origTS renders the replied-to message's send time (a quote is always part
	// of a reply, so ReplyToMessage carries the original's Date for both cases).
	origTS := func() string {
		if msg.ReplyToMessage == nil {
			return ""
		}
		return " (" + timeutil.Format(time.Unix(msg.ReplyToMessage.Date, 0)) + ")"
	}
	if msg.Quote != nil && msg.Quote.Text != "" {
		text = fmt.Sprintf("[Quoting%s: %s]\n\n%s", origTS(), msg.Quote.Text, text)
	} else if msg.ReplyToMessage != nil {
		quoted := msg.ReplyToMessage.Text
		if quoted == "" {
			quoted = msg.ReplyToMessage.Caption
		}
		if quoted != "" {
			text = fmt.Sprintf("[Replying to%s: %s]\n\n%s", origTS(), quoted, text)
		}
	}

	// Handle voice notes: download, transcribe, tag with [voice]
	isVoice := false
	if msg.Voice != nil && b.transcriber != nil {
		if data, err := b.downloadFile(msg.Voice.FileId); err != nil {
			b.logger().Errorf("download voice: %s", b.sanitizeError(err))
			if b.handler == nil || b.handler.Warnings() == nil || !b.handler.Warnings().Enabled() {
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
			isVoice = true
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
		if att, ok := b.downloadAttachment(photo.FileId, "image/jpeg", msg.Chat.Id, msg.MediaGroupId); ok {
			attachments = append(attachments, att)
		}
	} else if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id, msg.MediaGroupId); ok {
			attachments = append(attachments, att)
		}
	} else if msg.Document != nil && isPDFMIME(msg.Document.MimeType) {
		// PDFs under 32MB go through the content-block path (like images);
		// over-size PDFs fall back to save-to-disk via handleMediaMessage.
		const maxPDFSize = 32 * 1024 * 1024
		if msg.Document.FileSize > 0 && msg.Document.FileSize > maxPDFSize {
			text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "PDF", msg.Chat.Id, ".pdf", msg.MediaGroupId)
		} else if att, ok := b.downloadAttachment(msg.Document.FileId, msg.Document.MimeType, msg.Chat.Id, msg.MediaGroupId); ok {
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
		if att, ok := b.downloadAttachment(msg.Document.FileId, normalizedMIME, msg.Chat.Id, msg.MediaGroupId); ok {
			attachments = append(attachments, att)
		}
	}

	// Handle video messages
	if msg.Video != nil {
		text = b.handleMediaMessage(text, msg.Video.FileId, msg.Video.FileSize, "video", "Video", msg.Chat.Id, extForVideo(msg.Video.MimeType), msg.MediaGroupId)
	}

	// Handle video notes (circular video messages)
	if msg.VideoNote != nil {
		text = b.handleMediaMessage(text, msg.VideoNote.FileId, msg.VideoNote.FileSize, "videonote", "Video", msg.Chat.Id, ".mp4", msg.MediaGroupId)
	}

	// Handle remaining document types (not image, not PDF, not convertible)
	if msg.Document != nil && !isImageMIME(msg.Document.MimeType) && !isPDFMIME(msg.Document.MimeType) && !platform.IsConvertibleDocMIME(msg.Document.MimeType) {
		ext := filepath.Ext(msg.Document.FileName)
		if ext == "" {
			ext = extForMIME(msg.Document.MimeType)
		}
		text = b.handleMediaMessage(text, msg.Document.FileId, msg.Document.FileSize, "document", "Document", msg.Chat.Id, ext, msg.MediaGroupId)
	}

	// Drop messages with no text and no attachments
	if text == "" && len(attachments) == 0 {
		return queuedMessage{}, false
	}

	logText := text
	if len(attachments) > 0 {
		logText = fmt.Sprintf("[%d attachment(s)] %s", len(attachments), text)
	}
	if b.getDisplay().MessagesInLog {
		b.logger().Infof("message from %s: %s", formatUserInfo(msg.From), truncate(logText, 100))
	} else {
		b.logger().Debugf("message from %s", formatUserInfo(msg.From))
	}

	return queuedMessage{msg: msg, userID: userID, text: text, attachments: attachments, voice: isVoice, steer: steer, bareSteerPrefix: bareSteerPrefix}, true
}

// stripSelfMention removes a LEADING @mention of this bot — the first
// entity, at the start of the text — so the addressing token is not part of
// the payload: "@mybot /queue x" parses as the /queue prefix and the agent
// sees only the user's own words (#1318). Only the leading token: a
// trailing mention ("/queue do it @mybot") is ordinary payload, and mentions
// of other bots are left verbatim. Must run before any text edit (offsets
// shift); discord's receive path strips mentions before parsing the same
// way.
func (b *Bot) stripSelfMention(text string, entities []gotgbot.MessageEntity) string {
	if b.api == nil || b.api.User.Id == 0 {
		return text
	}
	start := len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace))
	for _, e := range entities {
		if e.Offset != int64(start) {
			continue
		}
		self := (e.Type == "mention" && b.api.User.Username != "" &&
			entityText(text, e) == "@"+b.api.User.Username) ||
			(e.Type == "text_mention" && e.User != nil && e.User.Id == b.api.User.Id)
		if self {
			return text[:start] + strings.TrimLeftFunc(text[start+int(e.Length):], unicode.IsSpace)
		}
	}
	return text
}

// stripSteerTarget removes THIS bot's @username suffix from a /steer|/queue
// first word ("/queue@mybot x" → "/queue x"), so a Telegram command-menu tap
// in a group parses as the prefix (#1318). Deliberately narrow: only the two
// prefix words, and only this bot's own suffix — "/status@mybot" keeps its
// shape (command routing is unchanged by this ticket), and "/queue@otherbot"
// is another bot's menu tap, not ours to answer.
func (b *Bot) stripSteerTarget(text string) string {
	username := ""
	if b.api != nil {
		username = b.api.Username
	}
	if username == "" {
		return text
	}
	start := len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace))
	rest := text[start:]
	word := rest
	if end := strings.IndexFunc(rest, unicode.IsSpace); end >= 0 {
		word = rest[:end]
	}
	if len(word) <= len("@")+len(username) ||
		!strings.EqualFold(word[len(word)-len(username)-1:], "@"+username) {
		return text
	}
	base := word[:len(word)-len(username)-1]
	if !strings.EqualFold(base, "/steer") && !strings.EqualFold(base, "/queue") {
		return text
	}
	return text[:start] + base + rest[len(word):]
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
		Dispatcher:   b.dispatcher,
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
	qm.text = result.Text // pick up any message transforms
	if !result.Consumed {
		return false
	}
	if result.WizardReply != "" {
		b.sendWizardStep(qm.msg.Chat.Id, result.WizardReply, result.WizardStep)
		_ = platform.SendDocAndRemove(b, qm.msg.Chat.Id, result.WizardDocPath, "")
		return true
	}
	if result.Outcome != nil {
		b.renderCommandOutcome(qm.msg, result.Outcome)
		return true
	}
	return true // silently consumed (stale command or idle secondary)
}
