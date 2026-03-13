package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/platform"
	"foci/internal/provider"
)

// prepareUserMessage builds the annotated user message with mana warnings,
// attachment path annotations, metadata prefix, reminders, and content blocks.
func (a *Agent) prepareUserMessage(ctx context.Context, sessionKey, userMessage, turnModel string, attachments []platform.Attachment, duplicateMessages bool) provider.Message {
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	manaStr, manaReset, manaGood := mana.ManaAndReset(a.SessionUsageClient(sessionKey), a.ManaInvestInterval)

	// Check mana thresholds and notify user for active conversations only
	var manaRestoreNote string
	if a.ManaWatcher != nil && !isSystemMessage(userMessage) {
		a.ManaWatcher.CheckAndWarn(manaStr, manaReset, func(warn string) {
			for _, fn := range a.ManaWarnFunc {
				fn(warn)
			}
		})
		if msg := a.ManaWatcher.CheckRestore(manaStr); msg != "" {
			manaRestoreNote = "[" + msg + "]\n"
			log.Infof("mana", "session=%s restore: %s", sessionKey, msg)
		}
	}

	// Annotate with saved attachment paths so the agent knows where files are
	var attachmentPaths string
	for _, att := range attachments {
		if att.SavedPath != "" {
			label := labelForMIME(att.MimeType)
			attachmentPaths += "[" + label + " saved to: " + att.SavedPath + "]\n"
		}
	}

	trigger := TriggerFromContext(ctx)
	plat := triggerToPlatform(trigger)
	metaPrefix := buildMetaPrefix(now, turnModel, plat, manaStr, manaGood, sm)
	reminderBlock := a.collectReminders(sessionKey)
	stateBlock := a.collectStateDashboard(sessionKey)
	msgBody := manaRestoreNote + attachmentPaths + userMessage
	if duplicateMessages && isUserTrigger(trigger) {
		msgBody = userMessage + "\n\n" + userMessage
	}
	annotatedMessage := metaPrefix + reminderBlock + stateBlock + "\n" + msgBody

	// Build content blocks: attachments first, then text
	const maxPDFSize = 32 * 1024 * 1024 // 32MB Anthropic API limit for documents
	var contentBlocks []provider.ContentBlock
	for _, att := range attachments {
		data, mediaType := att.Data, att.MimeType

		// Convertible documents: convert to text and include as a text block
		if isConvertibleMIME(mediaType) {
			textBlock := a.convertAttachmentToText(sessionKey, att)
			if textBlock != "" {
				contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: textBlock})
			}
			continue
		}

		if mediaType == "application/pdf" {
			if len(data) > maxPDFSize {
				continue // over-size PDFs already have save-to-disk annotation
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			contentBlocks = append(contentBlocks, provider.DocumentBlock(mediaType, encoded))
		} else {
			data, mediaType = maybeDownscaleImage(sessionKey, data, mediaType, a.MaxImagePixels)
			encoded := base64.StdEncoding.EncodeToString(data)
			contentBlocks = append(contentBlocks, provider.ImageBlock(mediaType, encoded))
		}
	}
	contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: annotatedMessage})

	return provider.Message{
		Role:    "user",
		Content: contentBlocks,
	}
}

// convertAttachmentToText converts a document attachment to text for the LLM.
// Applies the tool result size guard if the converted text is too large.
func (a *Agent) convertAttachmentToText(sessionKey string, att platform.Attachment) string {
	label := labelForMIME(att.MimeType)
	result := convertDocument(att.Data, att.MimeType, att.SavedPath)

	if result.Err != "" {
		a.logger().Warnf("session=%s document conversion failed for %s: %s", sessionKey, label, result.Err)
		msg := fmt.Sprintf("[%s document", label)
		if att.SavedPath != "" {
			msg += " saved to: " + att.SavedPath
		}
		msg += "]\n" + result.Err
		return msg
	}

	text := result.Text
	if text == "" {
		return ""
	}

	// Apply size guard: if converted text exceeds MaxResultChars, truncate with hint
	if a.MaxResultChars > 0 && len(text) > a.MaxResultChars {
		a.logger().Infof("session=%s converted %s text too large (%d chars, limit %d), truncating", sessionKey, label, len(text), a.MaxResultChars)
		text = text[:a.MaxResultChars]
		truncNote := fmt.Sprintf("\n[... truncated — full document is on disk")
		if att.SavedPath != "" {
			truncNote += " at " + att.SavedPath
		}
		truncNote += "]"
		text += truncNote
	}

	header := fmt.Sprintf("[%s document", label)
	if att.SavedPath != "" {
		header += " from: " + att.SavedPath
	}
	header += "]\n"
	return header + text
}
