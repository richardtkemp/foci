package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/anthropic"
	"foci/log"
)

// SessionAppender appends a message to a session.
type SessionAppender interface {
	Append(key string, msg anthropic.Message) error
}

// SessionNotifyFn handles routing a response back to the target session's
// own Telegram chat, rather than the calling session.
type SessionNotifyFn func(sessionKey, message string)

// NewSendToSessionTool creates a tool that injects a user-role message into
// another session and triggers processing via the notifier.
//
// The sessionNotifyFn is called when reply_to="session" — it routes the
// response to the target session's own chat instead of back to the caller.
func NewSendToSessionTool(sessions SessionAppender, notifier *AsyncNotifier, sessionNotifyFn SessionNotifyFn) *Tool {
	return &Tool{
		Name:        "send_to_session",
		Description: "Send a message to another session. The message is injected as a user-role message and the target session's agent will see and respond to it. Use this to communicate with branch sessions or the main session. By default the target's reply routes back to you (the caller); set reply_to to \"session\" to have the reply go to the target session's own Telegram chat instead.",
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
				},
				"reply_to": {
					"type": "string",
					"enum": ["caller", "session"],
					"description": "Where the target's reply goes: 'caller' (back to this session, default) or 'session' (to the target session's own Telegram chat)"
				}
			},
			"required": ["session_key", "message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				SessionKey string `json:"session_key"`
				Message    string `json:"message"`
				ReplyTo    string `json:"reply_to"`
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
			if p.ReplyTo == "" {
				p.ReplyTo = "caller"
			}
			if p.ReplyTo != "caller" && p.ReplyTo != "session" {
				return "", fmt.Errorf("reply_to must be 'caller' or 'session', got %q", p.ReplyTo)
			}

			originSession := SessionKeyFromContext(ctx)

			// Tag the message with its origin so the recipient knows where it came from.
			tagged := fmt.Sprintf("[Message from session %s]\n\n%s", originSession, p.Message)

			log.Infof("send_to_session", "from=%s to=%s reply_to=%s len=%d", originSession, p.SessionKey, p.ReplyTo, len(p.Message))

			if p.ReplyTo == "session" {
				// Route the response to the target session's own chat.
				// Don't Append here — sessionNotifyFn calls HandleMessage which
				// loads the session and appends the message itself.
				if sessionNotifyFn != nil {
					sessionNotifyFn(p.SessionKey, tagged)
				}
			} else {
				// Default: route the response back to the caller.
				// Append first since the notifier just triggers processing
				// of the already-appended message.
				msg := anthropic.Message{
					Role:    "user",
					Content: anthropic.TextContent(tagged),
				}
				if err := sessions.Append(p.SessionKey, msg); err != nil {
					return "", fmt.Errorf("append to session %s: %w", p.SessionKey, err)
				}
				if notifier != nil {
					notifier.Notify(p.SessionKey, tagged)
				}
			}

			return fmt.Sprintf("Message sent to session %s (reply_to=%s).", p.SessionKey, p.ReplyTo), nil
		},
	}
}
