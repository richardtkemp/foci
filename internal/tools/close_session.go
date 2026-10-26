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

// NewCloseSessionTool creates the close_session tool (#2573): the agent closes
// the app chat it is running in. Closing is exactly the user's own Close chat —
// the tab leaves every device, the history and roster row stay, and the user's
// next message reopens it — so the tool is safe to call in the same turn as the
// final reply (the reply still lands in the chat and shows as unread).
func NewCloseSessionTool(closeFn SessionCloser) *Tool {
	return &Tool{
		Name: "close_session",
		Description: "Close this app chat, exactly as if the user closed it: the chat leaves the open tabs on every device; " +
			"its history stays, and the user's next message in it reopens it. Call it only when the conversation is finished " +
			"with nothing outstanding (or when the user asks you to close it). Takes no arguments.",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
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
			if n == 0 {
				return TextResult("Nothing to close: this session has no open app chat (it is already closed, or is not an app chat)."), nil
			}
			return TextResult("Closed this chat. The user's next message in it reopens it."), nil
		},
	}
}
