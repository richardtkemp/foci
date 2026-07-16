package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/session"
)

// AliasSetter provides the SessionIndex methods needed to set a conversation
// alias. Satisfied by *session.SessionIndex.
type AliasSetter interface {
	PlatformForChat(agentID string, chatID int64) string
	GetChatMetadata(agentID, platform string, chatID int64, key string) (string, error)
	SetChatAliasUnique(agentID, platform string, chatID int64, alias string) error
	SetChatMetadata(agentID, platform string, chatID int64, key, value string) error
}

// NewSetSessionAliasTool creates a tool that lets the agent set a descriptive
// name for the current conversation. Provided to agents on backends that
// don't auto-generate session names (CC, opencode). Gated out for backends
// that do (Codex) via the tool table's enabled func.
func NewSetSessionAliasTool(idx AliasSetter) *Tool {
	return &Tool{
		Name:        "set_session_alias",
		Description: "Set a short descriptive name for this conversation (shown in the chat list). Call once after the first exchange to name what the conversation is about. Keep it under 5 words.",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"alias": {
					"type": "string",
					"description": "Short name for this conversation (e.g. 'Debugging scroll bug', 'Planning API migration')"
				}
			},
			"required": ["alias"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Alias string `json:"alias"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return TextResult("Error: invalid parameters"), nil
			}
			alias := strings.TrimSpace(p.Alias)
			if alias == "" {
				return TextResult("Error: alias is required"), nil
			}

			sk := SessionKeyFromContext(ctx)
			if sk == "" {
				return TextResult("Error: no session key in context"), nil
			}
			key, err := session.ParseSessionKey(sk)
			if err != nil {
				return TextResult(fmt.Sprintf("Error: parse session key: %v", err)), nil
			}
			if key.Type != 'c' {
				return TextResult("Alias can only be set on chat sessions."), nil
			}

			chatID := session.ChatIDFromKey(sk)
			if chatID == 0 {
				return TextResult("Error: no chat ID in session key"), nil
			}

			platform := idx.PlatformForChat(key.AgentID, chatID)
			if platform == "" {
				return TextResult("Error: no platform found for this chat"), nil
			}

			// Don't overwrite a user-set alias.
			existing, _ := idx.GetChatMetadata(key.AgentID, platform, chatID, "alias")
			isAuto, _ := idx.GetChatMetadata(key.AgentID, platform, chatID, "alias_auto")
			if existing != "" && isAuto != "1" {
				return TextResult(fmt.Sprintf("Skipped — this chat already has a manual name: %q", existing)), nil
			}

			if err := idx.SetChatAliasUnique(key.AgentID, platform, chatID, alias); err != nil {
				return TextResult(fmt.Sprintf("Error setting alias: %v", err)), nil
			}
			if e := idx.SetChatMetadata(key.AgentID, platform, chatID, "alias_auto", "1"); e != nil {
				return TextResult(fmt.Sprintf("Alias set, but flag failed: %v", e)), nil
			}
			return TextResult(fmt.Sprintf("Set conversation name: %q", alias)), nil
		},
	}
}
