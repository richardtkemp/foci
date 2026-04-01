package ccstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
)

// PermissionRequestBody is the structured body of a permission request.
// Alias for the protocol type used by helper functions in this file.
type PermissionRequestBody = PermissionRequestPayload

// pendingPermission tracks an unresolved permission request from CC.
type pendingPermission struct {
	requestID   string
	toolName    string
	toolUseID   string
	description string
	summary     string
	createdAt   time.Time
}

// handlePermissionRequest is called by the reader goroutine when CC sends a
// can_use_tool control request. It stores the pending permission, notifies
// the DelegatedManager, and sends a prompt to the platform UI.
func (b *Backend) handlePermissionRequest(msg *PermissionRequest) {
	log.Debugf("ccstream/perm", "handlePermissionRequest: req_id=%s tool=%s", msg.RequestID, msg.Request.ToolName)
	summary := buildPermissionSummary(&msg.Request)
	text := buildPermissionText(&msg.Request)
	choices := buildPermissionChoices(&msg.Request)

	pp := &pendingPermission{
		requestID:   msg.RequestID,
		toolName:    msg.Request.ToolName,
		toolUseID:   msg.Request.ToolUseID,
		description: msg.Request.Description,
		summary:     summary,
		createdAt:   time.Now(),
	}

	b.permMu.Lock()
	b.pendingPerms[msg.RequestID] = pp
	b.permMu.Unlock()

	if b.onPermPending != nil {
		b.onPermPending()
	}

	if b.permPromptFn != nil {
		log.Debugf("ccstream/perm", "calling permPromptFn for req_id=%s", msg.RequestID)
		b.permPromptFn(text, summary, choices)
	} else if b.replyFunc != nil {
		log.Warnf("ccstream/perm", "permPromptFn nil, falling back to replyFunc for req_id=%s", msg.RequestID)
		b.replyFunc(text)
	} else {
		log.Warnf("ccstream/perm", "both permPromptFn and replyFunc nil for req_id=%s", msg.RequestID)
	}
}

// RespondToPermission is called by the platform layer when the user responds
// to a permission prompt. It sends an allow or deny control response to CC.
func (b *Backend) RespondToPermission(requestID string, allow bool, message string) error {
	b.permMu.Lock()
	pp, ok := b.pendingPerms[requestID]
	delete(b.pendingPerms, requestID)
	noMorePending := len(b.pendingPerms) == 0
	b.permMu.Unlock()

	if !ok {
		return fmt.Errorf("ccstream: no pending permission with request ID %q", requestID)
	}

	var err error
	if allow {
		resp := &PermissionAllow{
			Behavior:               "allow",
			UpdatedInput:           json.RawMessage(`{}`),
			ToolUseID:              pp.toolUseID,
			DecisionClassification: "user_temporary",
		}
		err = b.writer.SendControlResponse(requestID, resp)
	} else {
		resp := &PermissionDeny{
			Behavior:               "deny",
			Message:                message,
			Interrupt:              false,
			ToolUseID:              pp.toolUseID,
			DecisionClassification: "user_reject",
		}
		err = b.writer.SendControlResponse(requestID, resp)
	}

	if err != nil {
		return err
	}

	if noMorePending && b.onPermCleared != nil {
		b.onPermCleared()
	}

	return nil
}

// RespondToPermissionWithRule responds with "always allow" for a given prefix,
// including the permission suggestion in updatedPermissions.
func (b *Backend) RespondToPermissionWithRule(requestID string, prefix string) error {
	b.permMu.Lock()
	pp, ok := b.pendingPerms[requestID]
	delete(b.pendingPerms, requestID)
	noMorePending := len(b.pendingPerms) == 0
	b.permMu.Unlock()

	if !ok {
		return fmt.Errorf("ccstream: no pending permission with request ID %q", requestID)
	}

	resp := &PermissionAllow{
		Behavior:     "allow",
		UpdatedInput: json.RawMessage(`{}`),
		UpdatedPermissions: []PermSuggestion{
			{Prefix: prefix, Scope: "session"},
		},
		ToolUseID:              pp.toolUseID,
		DecisionClassification: "user_permanent",
	}
	if err := b.writer.SendControlResponse(requestID, resp); err != nil {
		return err
	}

	if noMorePending && b.onPermCleared != nil {
		b.onPermCleared()
	}

	return nil
}

// handleControlCancel is called when CC cancels a permission request
// (e.g. a hook resolved it before the user responded).
func (b *Backend) handleControlCancel(reqID string) {
	b.permMu.Lock()
	delete(b.pendingPerms, reqID)
	noMorePending := len(b.pendingPerms) == 0
	b.permMu.Unlock()

	if noMorePending && b.onPermCleared != nil {
		b.onPermCleared()
	}
}

// PendingPermissions returns the count of pending permission requests
// (for diagnostics).
func (b *Backend) PendingPermissions() int {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	return len(b.pendingPerms)
}

// hasPendingPermissions reports whether any permission requests are pending.
func (b *Backend) hasPendingPermissions() bool {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	return len(b.pendingPerms) > 0
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildPermissionText formats the permission request for display.
func buildPermissionText(req *PermissionRequestBody) string {
	var b strings.Builder
	b.WriteString("**Permission Required**\n\n")
	if req.DisplayName != "" {
		b.WriteString(fmt.Sprintf("**%s**", req.DisplayName))
		if req.Title != "" {
			b.WriteString(fmt.Sprintf(": %s", req.Title))
		}
		b.WriteString("\n\n")
	}
	if req.Description != "" {
		b.WriteString(req.Description)
		b.WriteString("\n\n")
	}
	if req.DecisionReason != "" {
		b.WriteString(fmt.Sprintf("_Reason: %s_", req.DecisionReason))
	}
	return b.String()
}

// buildPermissionSummary creates a short summary for post-approval display.
func buildPermissionSummary(req *PermissionRequestBody) string {
	if req.Description != "" {
		return req.Description
	}
	if req.DisplayName != "" && req.Title != "" {
		return fmt.Sprintf("%s: %s", req.DisplayName, req.Title)
	}
	return req.ToolName
}

// buildPermissionChoices creates the button choices for the platform UI.
func buildPermissionChoices(req *PermissionRequestBody) []backend.PromptChoice {
	choices := []backend.PromptChoice{
		{Label: "Allow", Data: "allow"},
		{Label: "Deny", Data: "deny"},
	}
	for _, s := range req.PermissionSuggestions {
		if s.Prefix != "" {
			choices = append(choices, backend.PromptChoice{
				Label: fmt.Sprintf("Always: %s", s.Prefix),
				Data:  fmt.Sprintf("allow_always:%s", s.Prefix),
			})
		}
	}
	return choices
}
