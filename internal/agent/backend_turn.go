package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
)

// handleViaBackend processes a user message through the coding agent backend
// instead of the traditional agent loop. The backend owns inference and tool
// execution; Foci composes the prompt (metadata, reminders, nudges, state)
// and relays streaming events to the platform.
func (a *Agent) handleViaBackend(ctx context.Context, sessionKey string, texts []string) (string, error) {
	// Touch session activity for index tracking.
	for _, fn := range a.OnActivity {
		fn(sessionKey)
	}

	// Log received message.
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

	// Compose the prompt with Foci's metadata, reminders, nudges, state.
	prompt := a.composeBackendPrompt(ctx, sessionKey, texts)

	// Wire EventHandler to TurnCallbacks from context.
	cb := TurnCallbacksFromContext(ctx)
	handler := &backend.EventHandler{}
	if cb != nil {
		if cb.ReplyFunc != nil {
			handler.OnText = func(text string) {
				cb.ReplyFunc(text)
			}
		}
		if cb.ToolCallObserver != nil {
			handler.OnToolStart = func(name string, input string) {
				cb.ToolCallObserver(name, json.RawMessage(input))
			}
		}
	}

	startTime := time.Now()

	result, err := a.Backend.SendTurn(ctx, prompt, handler)
	if err != nil {
		return "", err
	}

	duration := time.Since(startTime)
	a.logger().Infof("backend_turn session=%s duration=%s tools=%d text_len=%d",
		sessionKey, duration.Round(time.Millisecond), result.ToolCalls, len(result.Text))

	return result.Text, nil
}

// composeBackendPrompt builds the prompt text for a backend turn.
// Includes metadata prefix, reminders, state dashboard, nudges, and user text.
func (a *Agent) composeBackendPrompt(ctx context.Context, sessionKey string, texts []string) string {
	var parts []string

	// Metadata prefix (simplified — no mana for backend agents in alpha).
	sm := a.getSessionMeta(sessionKey)
	now := time.Now()
	trigger := TriggerFromContext(ctx)
	platName := triggerToPlatform(trigger)
	metaLine := buildMetaPrefix(now, a.Model, platName, "", false, sm)
	if metaLine != "" {
		parts = append(parts, metaLine)
	}

	// Reminders.
	if reminders := a.collectReminders(sessionKey); reminders != "" {
		parts = append(parts, reminders)
	}

	// State dashboard (tasks, todos, scratchpad).
	if dashboard := a.collectStateDashboard(sessionKey); dashboard != "" {
		parts = append(parts, dashboard)
	}

	// Nudges.
	if a.Nudger != nil && len(texts) > 0 {
		a.Nudger.StartTurn(texts[0])
		for _, r := range a.Nudger.CheckTurnInterval() {
			parts = append(parts, nudgeHeader+r)
		}
		for _, r := range a.Nudger.CheckRegex() {
			parts = append(parts, nudgeHeader+r)
		}
	}

	// User text.
	parts = append(parts, texts...)

	return strings.Join(parts, "\n")
}
