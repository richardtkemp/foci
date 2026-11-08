package tools

import (
	"context"
	"encoding/json"
	"errors"
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
//
// onChanged, when non-nil, is called after the alias is persisted, so a client
// that shows the name can refresh it: the app only learns a title from a roster
// push, and without one it kept the old name until it reconnected (#2157).
func NewSetSessionAliasTool(idx AliasSetter, onChanged func(agentID, platform string, chatID int64)) *Tool {
	return &Tool{
		Name:        "set_session_alias",
		Description: "Set a short descriptive name for this conversation (shown in the chat list). Keep it under 5 words. A name the user set by hand is kept unless force is set; set it ONLY when the user explicitly asks you to rename this chat.",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"alias": {
					"type": "string",
					"description": "Short name for this conversation (e.g. 'Debugging scroll bug', 'Planning API migration')"
				},
				"force": {
					"type": "boolean",
					"description": "Overwrite a name the user set by hand. Use ONLY when the user explicitly asks for this rename; the new name then counts as the user's own."
				}
			},
			"required": ["alias"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Alias string `json:"alias"`
				Force bool   `json:"force"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, errors.New("invalid parameters")
			}
			alias := strings.TrimSpace(p.Alias)
			if alias == "" {
				return ToolResult{}, errors.New("alias is required")
			}

			sk := SessionKeyFromContext(ctx)
			if sk == "" {
				return ToolResult{}, errors.New("no session key in context")
			}
			key, err := session.ParseSessionKey(sk)
			if err != nil {
				return ToolResult{}, fmt.Errorf("parse session key: %w", err)
			}
			if key.Type != 'c' {
				return ToolResult{}, errors.New("alias can only be set on chat sessions")
			}

			chatID := session.ChatIDFromKey(sk)
			if chatID == 0 {
				return ToolResult{}, errors.New("no chat ID in session key")
			}

			platform := idx.PlatformForChat(key.AgentID, chatID)
			if platform == "" {
				return ToolResult{}, errors.New("no platform found for this chat")
			}

			// Don't overwrite a user-set alias unless the user asked for it (#2166).
			existing, _ := idx.GetChatMetadata(key.AgentID, platform, chatID, "alias")
			isAuto, _ := idx.GetChatMetadata(key.AgentID, platform, chatID, "alias_auto")
			if existing != "" && isAuto != "1" && !p.Force {
				return ToolResult{}, fmt.Errorf("you ran set_session_alias without --force, but the user already set a name "+
					"for this chat by hand: %q. Typically you should not override a manually set name, but you may "+
					"if you need to by using the --force flag", existing)
			}

			if err := idx.SetChatAliasUnique(key.AgentID, platform, chatID, alias); err != nil {
				return ToolResult{}, fmt.Errorf("setting alias: %w", err)
			}
			if onChanged != nil {
				onChanged(key.AgentID, platform, chatID)
			}
			// A rename the user asked for counts as theirs, so later unprompted
			// calls cannot clobber it (same as an app-side rename).
			autoFlag := "1"
			if p.Force {
				autoFlag = ""
			}
			if e := idx.SetChatMetadata(key.AgentID, platform, chatID, "alias_auto", autoFlag); e != nil {
				return ToolResult{}, fmt.Errorf("alias set, but flag failed: %w", e)
			}
			return TextResult(fmt.Sprintf("Set conversation name: %q", alias)), nil
		},
	}
}
