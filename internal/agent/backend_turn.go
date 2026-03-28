package agent

import (
	"context"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
)

// handleViaBackend processes a user message through a coding agent backend.
// Gets or creates a per-session Backend, sends the composed prompt, and
// returns immediately — output is delivered asynchronously via the watcher.
func (a *Agent) handleViaBackend(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (string, error) {
	for _, fn := range a.OnActivity {
		fn(sessionKey)
	}

	meta := TurnMetadataFromContext(ctx)
	if meta == nil {
		meta = &TurnMetadata{}
	}
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    meta.UserID,
		Username:  meta.Username,
		ChatID:    meta.ChatID,
		Text:      strings.Join(texts, "\n"),
		Session:   sessionKey,
	})

	// Get session metadata — composeTurnText reads lastMessageTime for the
	// gap calculation, so we must NOT update it until after composition.
	sm := a.getSessionMeta(sessionKey)

	// Get or create the Backend for this session.
	be, err := a.BackendManager.Get(ctx, sessionKey)
	if err != nil {
		return "", err
	}

	parts := a.composeTurnText(ctx, sessionKey, a.Model, "", false, texts, attachments)
	prompt := parts.JoinPrompt()

	// Inject nudges — composeTurnText no longer includes them (nudge logic
	// is owned by TurnContract.InjectNudges; this inline version keeps the
	// live backend path working until the Stage 6 switchover).
	if a.Nudger != nil && len(texts) > 0 {
		a.Nudger.StartTurn(texts[0])
		var nudges []string
		for _, r := range a.Nudger.CheckTurnInterval() {
			nudges = append(nudges, nudgeHeader+r)
		}
		for _, r := range a.Nudger.CheckRegex() {
			nudges = append(nudges, nudgeHeader+r)
		}
		if len(nudges) > 0 {
			prompt = strings.Join(nudges, "\n") + "\n" + prompt
		}
	}

	// Update lastMessageTime AFTER composition so the gap is calculated
	// against the previous message, not the current one.
	sm.lastMessageTime = time.Now()

	_, err = be.SendTurn(ctx, prompt, &backend.EventHandler{})
	if err != nil {
		return "", err
	}

	return "", nil
}

// SendPermissionResponse sends a keystroke to the backend agent's TUI
// for the given session key. Used for permission prompt responses where
// the CC TUI expects a keypress, not pasted text.
func (a *Agent) SendPermissionResponse(ctx context.Context, sessionKey string, key string) error {
	if a.BackendManager == nil {
		return nil
	}
	be, err := a.BackendManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}
	return be.SendKeystroke(ctx, key)
}
