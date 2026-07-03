package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/shared/prompts"
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
// own chat, rather than the calling session.
type SessionNotifyFn func(sessionKey, message string)

// SessionKeyResolverFn resolves a loose session target — anything that does
// not parse as a full session key: a bare agent name ("scout" → the agent's
// default session), a named session ("scout/research"), or a chat alias.
// Returns the resolved key and which resolution rung matched (for the tool's
// receipt), or an error describing why nothing matched.
type SessionKeyResolverFn func(target string) (key, via string, err error)

// NewSendToSessionTool creates a tool that injects a user-role message into
// another session and triggers processing via the notifier.
//
// The sessionNotifyFn is called when reply_to="session" — it routes the
// response to the target session's own chat instead of back to the caller.
// The resolveKeyFn is optional — if provided, loose targets (bare agent
// names) are resolved to the active session key.
func NewSendToSessionTool(sessions SessionAppender, notifier *AsyncNotifier, sessionNotifyFn SessionNotifyFn, resolveKeyFn SessionKeyResolverFn) *Tool {
	return &Tool{
		Name:       "send_to_session",
		ExecExport: true,
		// Generic shell generator supports one positional only — pick
		// session_key (the routing target) and let message come via stdin
		// or --message. Usage: foci_send_to_session <session_key> --message X
		// or: echo "..." | foci_send_to_session <session_key>
		Positional:  []string{"session_key"},
		StdinParam:  "message",
		Description: "Send a message to another session. The message is injected as a user-role message and the target session's agent will see and respond to it. Use this to communicate with branch sessions or the main session. By default the target's reply routes back to you (the caller); set reply_to to \"session\" to have the reply go to the target session's own chat instead.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_key": {
					"type": "string",
					"description": "Target session. Accepts a full session key (e.g. scout/c5970082313), a bare agent name (e.g. scout — resolves to the agent's default session), an agent-qualified session name (e.g. scout/research), or a chat alias."
				},
				"message": {
					"type": "string",
					"description": "Message to send to the target session"
				},
				"reply_to": {
					"type": "string",
					"enum": ["caller", "session"],
					"description": "Where the target's reply goes: 'caller' (back to this session, default) or 'session' (to the target session's own chat)"
				}
			},
			"required": ["session_key", "message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				SessionKey string `json:"session_key"`
				Message    string `json:"message"`
				ReplyTo    string `json:"reply_to"`
			}](params)
			if err != nil {
				return ToolResult{}, err
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

			// Resolve loose targets — bare agent names, session names, chat
			// aliases — through the shared route ladder. Full session keys
			// parse cleanly and skip resolution.
			targetKey := p.SessionKey
			resolvedVia := "exact"
			if _, err := session.ParseSessionKey(targetKey); err != nil && resolveKeyFn != nil {
				key, via, rerr := resolveKeyFn(targetKey)
				if rerr != nil {
					return ToolResult{}, fmt.Errorf("could not resolve %q to a session: %w", p.SessionKey, rerr)
				}
				targetKey = key
				resolvedVia = via
				log.Infof("send_to_session", "resolved %q → %s (via %s)", p.SessionKey, targetKey, via)
			}

			originSession := SessionKeyFromContext(ctx)

			// Tag the message with its origin, timestamp, and delivery context.
			// The note tells the receiving agent where its reply will go so it
			// can decide whether to use send_to_chat.
			var contextNote string
			if p.ReplyTo == "caller" {
				contextNote = "[SYSTEM INJECTION — This is a user-role message sent by another session, NOT by the user. The user has not seen it. Your reply will be routed back to the calling session — it will NOT appear in the user's chat. Reply normally to communicate with the calling session. If you also want to inform the user, use `send_to_chat`. If you have nothing to say, respond with `[[NO_RESPONSE]]` and nothing else.]"
			} else {
				contextNote = "[SYSTEM INJECTION — This is a user-role message sent by another session, NOT by the user. The user has not seen it. Your reply will be delivered to the user's chat normally. If the user already knows about it or you don't want to bother them, respond with `[[NO_RESPONSE]]` and nothing else. Otherwise actively *tell* the user about it and explain (e.g. \"I received a notification that...\", \"The system reports...\"). Do NOT passively comment on or observe the content — either `[[NO_RESPONSE]]` or proactively inform the user.]"
			}
			tagged := prompts.FormatInjectedMessage(
				fmt.Sprintf("MESSAGE FROM SESSION %s", originSession),
				time.Now(),
				p.Message,
				contextNote,
			)

			log.Infof("send_to_session", "from=%s to=%s reply_to=%s len=%d", originSession, targetKey, p.ReplyTo, len(p.Message))

			if p.ReplyTo == "session" {
				// Route the response to the target session's own chat.
				// Don't Append here — sessionNotifyFn calls HandleMessage which
				// loads the session and appends the message itself.
				if sessionNotifyFn != nil {
					sessionNotifyFn(targetKey, tagged)
				}
			} else {
				// Default: route the response back to the caller.
				// Don't Append here — InjectToAgent triggers HandleMessage which
				// loads the session and appends the message itself.
				if notifier != nil {
					// Pass originSession so response routes back to caller
					notifier.InjectToAgent(targetKey, tagged, originSession, "async_notify")
				}
			}

			return TextResult(fmt.Sprintf("Message sent to session %s (resolved via %s, reply_to=%s).", targetKey, resolvedVia, p.ReplyTo)), nil
		},
	}
}
