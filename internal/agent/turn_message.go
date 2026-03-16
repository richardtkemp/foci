package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
	"unicode/utf8"

	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/platform"
	"foci/internal/provider"
)

// prepareUserMessage builds the user message with separate content blocks for
// metadata, reminders, state dashboard, attachments, and user text.
// Multiple texts produce separate content blocks: texts[0] gets the full
// annotation treatment; texts[1:] each become a "[follow-up]" block.
func (a *Agent) prepareUserMessage(ctx context.Context, sessionKey string, texts []string, turnModel string, attachments []platform.Attachment, duplicateMessages bool) provider.Message {
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	manaStr, manaReset, manaGood := mana.ManaAndReset(a.SessionUsageClient(sessionKey), a.ManaInvestInterval)

	userMessage := texts[0]

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

	// Build content blocks: binary attachments first
	const maxPDFSize = 32 * 1024 * 1024 // 32MB Anthropic API limit for documents
	var contentBlocks []provider.ContentBlock
	for _, att := range attachments {
		data, mediaType := att.Data, att.MimeType

		// Convertible documents: convert to text and include as a text block
		if platform.IsConvertibleDocMIME(mediaType) {
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

	// First-run onboarding: prepend as a separate content block, then clear.
	if frm, ok := a.FirstRunMessage.Load().(string); ok && frm != "" {
		a.FirstRunMessage.Store("")
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: frm})
	}

	// Metadata, reminders, and state dashboard as separate content blocks.
	contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: metaPrefix})
	if reminderBlock != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: reminderBlock})
	}
	if stateBlock != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: stateBlock})
	}

	// Primary user text with annotations
	userText := userMessage
	if duplicateMessages && isUserTrigger(trigger) {
		userText = userMessage + "\n\n" + userMessage
	}
	msgBody := manaRestoreNote + attachmentPaths + userText
	contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: msgBody})

	// Follow-up texts from batched messages
	for _, t := range texts[1:] {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: "[follow-up] " + t})
	}

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
	if a.MaxResultChars > 0 && utf8.RuneCountInString(text) > a.MaxResultChars {
		a.logger().Infof("session=%s converted %s text too large (%d chars, limit %d), truncating", sessionKey, label, utf8.RuneCountInString(text), a.MaxResultChars)
		text = text[:a.MaxResultChars] // byte slice; may split a multi-byte rune
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
