package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"unicode/utf8"

	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/platform"
	"foci/internal/provider"
)

// consumeFirstRunMessage atomically returns and clears the first-run onboarding
// message, firing OnFirstRunConsumed exactly once when a non-empty message is
// actually consumed. Returns "" if there is no pending onboarding.
//
// Both turn paths call this: the API path (prepareUserMessage) and the
// delegated/claude-code path (ComposePrompt). Previously only the API path
// consumed it, so onboarding was silently dropped on every claude-code agent —
// and first_run_completed was marked anyway by a generic OnActivity callback,
// losing the onboarding for good. Tying the completion marker to the actual
// consumption (this method) keeps the two in lockstep regardless of backend.
//
// The CompareAndSwap makes consumption exactly-once under concurrent turns.
func (a *Agent) consumeFirstRunMessage() string {
	frm, ok := a.FirstRunMessage.Load().(string)
	if !ok || frm == "" {
		return ""
	}
	if !a.FirstRunMessage.CompareAndSwap(frm, "") {
		return "" // another turn consumed it first
	}
	if a.OnFirstRunConsumed != nil {
		a.OnFirstRunConsumed()
	}
	return frm
}

// prepareUserMessage builds the user message with separate content blocks for
// metadata, reminders, state dashboard, attachments, orientation, and user text.
// Multiple texts produce separate content blocks: texts[0] gets the full
// annotation treatment; texts[1:] each become a "[follow-up]" block.
// If orientation is non-empty, it is included as a content block before the
// user text — this is how branch orientation is delivered on the first turn
// instead of being a separate user message (which would cause consecutive users).
func (a *Agent) prepareUserMessage(ctx context.Context, sessionKey string, texts []string, turnModel string, attachments []platform.Attachment, duplicateMessages bool, orientation string) provider.Message {
	trigger := TriggerFromContext(ctx)

	// Resolve mana for both meta prefix and watcher.
	manaStr, manaReset, manaGood := mana.ManaAndReset(a.SessionUsageClient(sessionKey), a.ManaInvestInterval)

	// Shared prompt composition (metadata, reminders, state, attachment paths).
	// Nudges are NOT included — handled separately by InjectNudges / lines 484-501.
	tp := a.composeTurnText(ctx, sessionKey, turnModel, manaStr, manaGood, texts, attachments)
	if a.ManaWatcher != nil && len(texts) > 0 && !isSystemMessage(texts[0]) {
		a.ManaWatcher.CheckAndWarn(manaStr, manaReset, func(warn string) {
			for _, fn := range a.ManaWarnFunc {
				fn(warn)
			}
		})
		if msg := a.ManaWatcher.CheckRestore(manaStr); msg != "" {
			tp.ManaRestore = "[" + msg + "]"
			log.Infof("mana", "session=%s restore: %s", sessionKey, msg)
		}
	}

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
	if frm := a.consumeFirstRunMessage(); frm != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: frm})
	}

	// Common text parts as separate content blocks.
	contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: tp.MetaPrefix})
	if tp.Reminders != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: tp.Reminders})
	}
	if tp.StateDashboard != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: tp.StateDashboard})
	}
	if tp.ManaRestore != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: tp.ManaRestore})
	}
	if tp.AttachmentPaths != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: tp.AttachmentPaths})
	}

	// Branch orientation (first turn only).
	if orientation != "" {
		contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: orientation})
	}

	// Primary user text
	userText := texts[0]
	if duplicateMessages && isUserTrigger(trigger) {
		userText = texts[0] + "\n\n" + texts[0]
	}
	contentBlocks = append(contentBlocks, provider.ContentBlock{Type: "text", Text: userText})

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
