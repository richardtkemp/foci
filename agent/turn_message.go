package agent

import (
	"context"
	"encoding/base64"
	"time"

	"foci/anthropic"
	"foci/log"
)

// prepareUserMessage builds the annotated user message with mana warnings,
// attachment path annotations, metadata prefix, reminders, and content blocks.
func (a *Agent) prepareUserMessage(ctx context.Context, sessionKey, userMessage, turnModel string, images []Attachment) anthropic.Message {
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	mana, manaReset, manaGood := a.manaAndReset()

	// Check mana thresholds and notify user for active conversations only
	var manaRestoreNote string
	if a.ManaWatcher != nil && !isSystemMessage(userMessage) {
		a.ManaWatcher.CheckAndWarn(mana, manaReset, func(warn string) {
			if a.ManaWarnFunc != nil {
				a.ManaWarnFunc(warn)
			}
		})
		if msg := a.ManaWatcher.CheckRestore(mana); msg != "" {
			manaRestoreNote = "[" + msg + "]\n"
			log.Infof("mana", "restore: %s", msg)
		}
	}

	// Annotate with saved attachment paths so the agent knows where files are
	var imagePaths string
	for _, img := range images {
		if img.SavedPath != "" {
			label := "Image"
			if img.MediaType == "application/pdf" {
				label = "PDF"
			}
			imagePaths += "[" + label + " saved to: " + img.SavedPath + "]\n"
		}
	}

	metaPrefix := buildMetaPrefix(now, turnModel, mana, manaGood, sm)
	reminderBlock := a.collectReminders(sessionKey)
	msgBody := manaRestoreNote + imagePaths + userMessage
	trigger := TriggerFromContext(ctx)
	if a.DuplicateMessages && isUserTrigger(trigger) {
		msgBody = userMessage + "\n\n" + userMessage
	}
	annotatedMessage := metaPrefix + reminderBlock + "\n" + msgBody

	// Build content blocks: attachments first, then text
	const maxPDFSize = 32 * 1024 * 1024 // 32MB Anthropic API limit for documents
	var contentBlocks []anthropic.ContentBlock
	for _, img := range images {
		encoded := base64.StdEncoding.EncodeToString(img.Data)
		if img.MediaType == "application/pdf" {
			if len(img.Data) > maxPDFSize {
				continue // over-size PDFs already have save-to-disk annotation
			}
			contentBlocks = append(contentBlocks, anthropic.DocumentBlock(img.MediaType, encoded))
		} else {
			contentBlocks = append(contentBlocks, anthropic.ImageBlock(img.MediaType, encoded))
		}
	}
	contentBlocks = append(contentBlocks, anthropic.ContentBlock{Type: "text", Text: annotatedMessage})

	return anthropic.Message{
		Role:    "user",
		Content: contentBlocks,
	}
}
