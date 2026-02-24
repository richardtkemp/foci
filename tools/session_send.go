package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"clod/anthropic"
	"clod/log"
)

// SessionAppender appends a message to a session.
type SessionAppender interface {
	Append(key string, msg anthropic.Message) error
}

// NewSendToSessionTool creates a tool that injects a user-role message into
// another session and triggers processing via the notifier.
func NewSendToSessionTool(sessions SessionAppender, notifier *AsyncNotifier) *Tool {
	return &Tool{
		Name:        "send_to_session",
		Description: "Send a message to another session. The message is injected as a user-role message and the target session's agent will see and respond to it. Use this to communicate with branch sessions or the main session.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_key": {
					"type": "string",
					"description": "Target session key (e.g. agent:myagent:main or agent:myagent:multiball:mb-123456)"
				},
				"message": {
					"type": "string",
					"description": "Message to send to the target session"
				}
			},
			"required": ["session_key", "message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				SessionKey string `json:"session_key"`
				Message    string `json:"message"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.SessionKey == "" {
				return "", fmt.Errorf("session_key is required")
			}
			if p.Message == "" {
				return "", fmt.Errorf("message is required")
			}

			originSession := SessionKeyFromContext(ctx)

			// Tag the message with its origin so the recipient knows where it came from.
			tagged := fmt.Sprintf("[Message from session %s]\n\n%s", originSession, p.Message)

			msg := anthropic.Message{
				Role:    "user",
				Content: anthropic.TextContent(tagged),
			}
			if err := sessions.Append(p.SessionKey, msg); err != nil {
				return "", fmt.Errorf("append to session %s: %w", p.SessionKey, err)
			}

			log.Infof("send_to_session", "from=%s to=%s len=%d", originSession, p.SessionKey, len(p.Message))

			// Trigger the target session to process the injected message.
			if notifier != nil {
				notifier.Notify(p.SessionKey, tagged)
			}

			return fmt.Sprintf("Message sent to session %s.", p.SessionKey), nil
		},
	}
}
