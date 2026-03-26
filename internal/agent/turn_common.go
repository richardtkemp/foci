package agent

import (
	"context"
	"strings"
	"time"

	"foci/internal/platform"
)

// turnTextParts holds the common text components assembled for any turn,
// regardless of whether it's sent to an API or a coding agent backend.
type turnTextParts struct {
	MetaPrefix     string
	Reminders      string
	StateDashboard string
	ManaRestore    string
	AttachmentPaths string
	Nudges         []string
	UserTexts      []string // texts[0] is primary, texts[1:] are follow-ups
}

// composeTurnText assembles the common text parts for a turn. Used by both
// the traditional API path (which converts these to content blocks) and the
// backend path (which joins them into a single prompt string).
func (a *Agent) composeTurnText(ctx context.Context, sessionKey string, turnModel string, manaStr string, manaGood bool, texts []string, attachments []platform.Attachment) turnTextParts {
	now := time.Now()
	sm := a.getSessionMeta(sessionKey)
	trigger := TriggerFromContext(ctx)
	platName := triggerToPlatform(trigger)

	var p turnTextParts

	p.MetaPrefix = buildMetaPrefix(now, turnModel, platName, manaStr, manaGood, sm)
	p.Reminders = a.collectReminders(sessionKey)
	p.StateDashboard = a.collectStateDashboard(sessionKey)

	// Attachment path annotations.
	var attachParts []string
	for _, att := range attachments {
		if att.SavedPath != "" {
			label := labelForMIME(att.MimeType)
			attachParts = append(attachParts, "["+label+" saved to: "+att.SavedPath+"]")
		}
	}
	if len(attachParts) > 0 {
		p.AttachmentPaths = strings.Join(attachParts, "\n")
	}

	// Nudges.
	if a.Nudger != nil && len(texts) > 0 {
		a.Nudger.StartTurn(texts[0])
		for _, r := range a.Nudger.CheckTurnInterval() {
			p.Nudges = append(p.Nudges, nudgeHeader+r)
		}
		for _, r := range a.Nudger.CheckRegex() {
			p.Nudges = append(p.Nudges, nudgeHeader+r)
		}
	}

	p.UserTexts = texts
	return p
}

// JoinPrompt joins all non-empty parts into a single prompt string.
// Used by the backend path.
func (p turnTextParts) JoinPrompt() string {
	var parts []string
	if p.MetaPrefix != "" {
		parts = append(parts, p.MetaPrefix)
	}
	if p.Reminders != "" {
		parts = append(parts, p.Reminders)
	}
	if p.StateDashboard != "" {
		parts = append(parts, p.StateDashboard)
	}
	if p.ManaRestore != "" {
		parts = append(parts, p.ManaRestore)
	}
	if p.AttachmentPaths != "" {
		parts = append(parts, p.AttachmentPaths)
	}
	parts = append(parts, p.Nudges...)
	if len(p.UserTexts) > 0 {
		parts = append(parts, p.UserTexts[0])
		for _, t := range p.UserTexts[1:] {
			parts = append(parts, "[follow-up] "+t)
		}
	}
	return strings.Join(parts, "\n")
}
