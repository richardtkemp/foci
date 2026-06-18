package agent

import (
	"context"
	"strings"
	"time"

	"foci/internal/platform"
)

// turnTextParts holds the common text components assembled for any turn,
// regardless of whether it's sent via the API or delegated transport.
// Nudges are NOT included — each transport handles nudge injection
// separately via TurnContract.InjectNudges.
type turnTextParts struct {
	MetaPrefix      string
	Reminders       string
	StateDashboard  string
	ManaRestore     string
	AttachmentPaths string
	UserTexts       []string // texts[0] is primary, texts[1:] are follow-ups
}

// composeTurnText assembles the common text parts for a turn. Used by both
// the traditional API path (which converts these to content blocks) and the
// delegated path (which joins them into a single prompt string).
func (a *Agent) composeTurnText(ctx context.Context, sessionKey string, turnModel string, manaStr string, manaGood bool, texts []string, attachments []platform.Attachment) turnTextParts {
	// Prefer the platform receipt time so queued/steered messages render the
	// time the user actually sent them. Fall back to wall clock for
	// system-initiated turns (cron, keepalive, etc.).
	now := ReceivedAtFromContext(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	sm := a.getSessionMeta(sessionKey)
	trigger := TriggerFromContext(ctx)
	platName := triggerToPlatform(trigger)

	var p turnTextParts

	// The statusline template owns both the [meta] and [state] lines (#831).
	// Empty config falls back to the default template, which reproduces the
	// historical two-line header byte-for-byte. The whole rendered block goes
	// into MetaPrefix; StateDashboard stays empty (the [state] line, if any, is
	// inside the rendered template).
	tmpl := a.Statusline
	if tmpl == "" {
		tmpl = DefaultStatuslineTemplate
	}
	p.MetaPrefix = a.renderStatusline(ctx, tmpl, statuslineInputs{
		now:        now,
		model:      turnModel,
		platform:   platName,
		manaStr:    manaStr,
		manaGood:   manaGood,
		sm:         sm,
		agent:      a,
		sessionKey: sessionKey,
	})
	p.Reminders = a.collectReminders(sessionKey)

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

	p.UserTexts = texts
	return p
}

// JoinPrompt joins all non-empty parts into a single prompt string.
// Used by the delegated path.
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
	if len(p.UserTexts) > 0 {
		parts = append(parts, p.UserTexts[0])
		for _, t := range p.UserTexts[1:] {
			parts = append(parts, "[follow-up] "+t)
		}
	}
	return strings.Join(parts, "\n")
}
