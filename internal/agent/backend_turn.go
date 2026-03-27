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

	// Update session metadata so gap calculation and keepalive work correctly.
	sm := a.getSessionMeta(sessionKey)
	sm.lastMessageTime = time.Now()

	// Get or create the Backend for this session.
	be, err := a.BackendManager.Get(ctx, sessionKey)
	if err != nil {
		return "", err
	}

	parts := a.composeTurnText(ctx, sessionKey, a.Model, "", false, texts, attachments)
	prompt := parts.JoinPrompt()

	_, err = be.SendTurn(ctx, prompt, &backend.EventHandler{})
	if err != nil {
		return "", err
	}

	return "", nil
}
