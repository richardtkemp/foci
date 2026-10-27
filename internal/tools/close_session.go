package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/session"
)

// SessionCloser closes the open app chat(s) bound to a session, as if the user
// had closed them on a device, and reports how many were open. Satisfied by
// app.CloseSessionConversations; injected so the tool is testable without a hub.
type SessionCloser func(agentID, sessionKey string) (int, error)

// SessionReader marks the app chat bound to a session read on every device,
// once the turn in flight (if any) has delivered its final reply, and reports
// how many chats it marked or scheduled. Satisfied by app.MarkSessionRead.
type SessionReader func(agentID, sessionKey string) (int, error)

// NewCloseSessionTool creates the close_session tool (#2573): the agent closes
// the app chat it is running in. Closing is exactly the user's own Close chat —
// the tab leaves every device, the history and roster row stay, and the user's
// next message reopens it — so the tool is safe to call in the same turn as the
// final reply. That reply still lands in the chat and shows as unread unless
// mark_read is set (#2592): then readFn marks the chat read through the end of
// this turn, final reply included, as if the user had read it.
func NewCloseSessionTool(closeFn SessionCloser, readFn SessionReader) *Tool {
	return &Tool{
		Name: "close_session",
		Description: "Close this app chat, exactly as if the user closed it: the chat leaves the open tabs on every device; " +
			"its history stays, and the user's next message in it reopens it. Call it only when the conversation is finished " +
			"with nothing outstanding (or when the user asks you to close it). With mark_read=true the chat is also marked read " +
			"on every device once this turn ends, so your final reply does not show as unread.",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"mark_read":{"type":"boolean","description":"Also mark the chat read, up to and including your final reply of this turn. Default false."}}}`),
		Execute: func(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
			var args struct {
				MarkRead bool `json:"mark_read"`
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return TextResult(fmt.Sprintf("Error: invalid arguments: %v", err)), nil
				}
			}
			sk := SessionKeyFromContext(ctx)
			if sk == "" {
				return TextResult("Error: no session key in context"), nil
			}
			key, err := session.ParseSessionKey(sk)
			if err != nil {
				return TextResult(fmt.Sprintf("Error: parse session key: %v", err)), nil
			}
			n, err := closeFn(key.AgentID, sk)
			if err != nil {
				return TextResult(fmt.Sprintf("Error: cannot close this chat: %v", err)), nil
			}
			msg := "Closed this chat. The user's next message in it reopens it."
			if n == 0 {
				msg = "Nothing to close: this session has no open app chat (it is already closed, or is not an app chat)."
			}
			if !args.MarkRead {
				return TextResult(msg), nil
			}
			// Marked even when nothing was open: an already-closed chat can still
			// hold the unread reply the caller wants gone.
			r, err := readFn(key.AgentID, sk)
			switch {
			case err != nil:
				msg += fmt.Sprintf(" Error: cannot mark it read: %v", err)
			case r == 0:
				msg += " Nothing to mark read: this session has no app chat."
			default:
				msg += " It will be marked read once this turn ends."
			}
			return TextResult(msg), nil
		},
	}
}
