package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/prompts"
)

// SessionAppender is the interface for session write operations.
// IMPORTANT: Tools must use For(sessionKey) to get a SessionWriter that prevents
// cross-session writes. All write operations on SessionWriter check that the target
// session matches the bound session. Attempting to write to a different session
// will fail with an error.
type SessionAppender interface {
	For(sessionKey string) session.SessionWriter
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				SessionKey string `json:"session_key"`
				Message    string `json:"message"`
				ReplyTo    string `json:"reply_to"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			if p.SessionKey == "" {
				return ToolResult{}, fmt.Errorf("session_key is required")
			}
			if p.Message == "" {
				return ToolResult{}, fmt.Errorf("message is required")
			}
			if p.ReplyTo == "" {
				p.ReplyTo = "caller"
			}
			if p.ReplyTo != "caller" && p.ReplyTo != "session" {
				return ToolResult{}, fmt.Errorf("reply_to must be 'caller' or 'session', got %q", p.ReplyTo)
			}

			originSession := SessionKeyFromContext(ctx)

			// Tag the message with its origin, timestamp, and context reminder.
			tagged := prompts.FormatInjectedMessage(
				fmt.Sprintf("MESSAGE FROM SESSION %s", originSession),
				time.Now(),
				p.Message,
			)

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
				// Don't Append here — InjectToAgent triggers HandleMessage which
				// loads the session and appends the message itself.
				if notifier != nil {
					// Pass originSession so response routes back to caller
					notifier.InjectToAgent(p.SessionKey, tagged, originSession)
				}
			}

			return TextResult(fmt.Sprintf("Message sent to session %s (reply_to=%s).", p.SessionKey, p.ReplyTo)), nil
		},
	}
}
