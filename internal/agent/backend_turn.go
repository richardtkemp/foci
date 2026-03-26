package agent

import (
	"context"
	"strings"

	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
)

// handleViaBackend processes a user message through the coding agent backend.
// Sends the composed prompt to the backend and returns immediately — output
// is delivered asynchronously via the watcher's streaming handler.
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

	parts := a.composeTurnText(ctx, sessionKey, a.Model, "", false, texts, attachments)
	prompt := parts.JoinPrompt()

	// Update the backend's reply function to target this session's chat.
	// The reply func is long-lived (outlives this call) — the watcher uses
	// it to deliver all asynchronous output until the next message arrives
	// from a possibly different session.
	if a.BackendSendFunc != nil {
		sk := sessionKey
		a.Backend.SetReplyFunc(func(text string) {
			a.BackendSendFunc(sk, text)
		})
	}

	_, err := a.Backend.SendTurn(ctx, prompt, &backend.EventHandler{})
	if err != nil {
		return "", err
	}

	// Response is delivered asynchronously via streaming — return empty.
	return "", nil
}
