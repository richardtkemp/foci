package ccstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// pendingPermission tracks an unresolved permission request from CC.
// For regular permissions, the question fields are zero-valued.
// For AskUserQuestion, they hold the sequential question state.
type pendingPermission struct {
	requestID   string
	toolName    string
	toolUseID   string
	description string
	summary     string
	createdAt   time.Time

	// Question-specific fields (zero values for regular permissions).
	questions     []userQuestion    // parsed questions from AskUserQuestion input
	currentIndex  int               // which question is currently being presented
	answers       map[string]string // accumulated answers (question text → answer)
	originalInput json.RawMessage   // preserved for building updatedInput
}

// handleToolRequest is called by the reader goroutine when CC sends a
// can_use_tool control request. It dispatches to tool-specific handlers
// (AskUserQuestion gets interactive question prompts) or falls through
// to the standard permission prompt flow.
func (b *Backend) handleToolRequest(msg *PermissionRequest) {
	log.Debugf("ccstream/perm", "handleToolRequest: req_id=%s tool=%s", msg.RequestID, msg.Request.ToolName)

	// AskUserQuestion requires user interaction — never auto-approve.
	if msg.Request.ToolName == "AskUserQuestion" {
		b.handleUserQuestion(msg)
		return
	}

	// Check auto-approve rules before prompting the user.
	if b.autoApprovePermission(msg) {
		return
	}

	summary := msg.Request.Summary()
	text := msg.Request.DisplayText()
	choices := msg.Request.Choices()

	pp := &pendingPermission{
		requestID:   msg.RequestID,
		toolName:    msg.Request.ToolName,
		toolUseID:   msg.Request.ToolUseID,
		description: msg.Request.Description,
		summary:     summary,
		createdAt:   time.Now(),
	}

	b.storePendingPerm(pp)
	b.outstanding.Register(msg.RequestID, OutstandingPermission)

	if b.permPromptFn != nil {
		log.Debugf("ccstream/perm", "calling permPromptFn for req_id=%s", msg.RequestID)
		b.permPromptFn(msg.RequestID, text, summary, choices)
	} else {
		log.Warnf("ccstream/perm", "permPromptFn nil for req_id=%s, prompt stored but not displayed", msg.RequestID)
	}
}

// RespondToPermission is called by the platform layer when the user responds
// to a permission prompt. It sends an allow or deny control response to CC.
func (b *Backend) RespondToPermission(requestID string, allow bool, message string) error {
	pp, ok := b.removePendingPerm(requestID)
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

	b.outstanding.Resolve(requestID)
	return nil
}

// RespondToPermissionWithRule responds with "always allow" for a given prefix,
// including the permission suggestion in updatedPermissions.
func (b *Backend) RespondToPermissionWithRule(requestID string, prefix string) error {
	pp, ok := b.removePendingPerm(requestID)
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

	b.outstanding.Resolve(requestID)
	return nil
}

// handleControlCancel is called when CC cancels a permission request
// (e.g. a hook resolved it before the user responded, or a follow-up user
// message interrupted the in-flight tool execution). This is the only
// non-user-driven path that clears a permission — surface it at INFO so it
// shows up alongside the corresponding "permission cleared" debug line and
// makes the cause attributable.
//
// The OutstandingRegistry fires any per-prompt cancel listeners registered
// by the platform layer (e.g. to disable the orphaned inline keyboard so
// the user can't click an already-resolved button) and the registry-wide
// onEmpty hook if this was the last outstanding prompt.
func (b *Backend) handleControlCancel(reqID string) {
	pp, _ := b.removePendingPerm(reqID)

	tool := ""
	if pp != nil {
		tool = pp.toolName
	}
	log.Infof("ccstream/perm", "permission auto-cancelled by CC control_cancel_request: reqID=%s tool=%s", reqID, tool)

	b.outstanding.Cancel(reqID, "tool request cancelled by follow-up message")
}

// PendingPermissions returns the count of pending permission requests
// (for diagnostics).
func (b *Backend) PendingPermissions() int {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	return len(b.pendingPerms)
}

// storePendingPerm adds a pending permission under the lock.
func (b *Backend) storePendingPerm(pp *pendingPermission) {
	b.permMu.Lock()
	b.pendingPerms[pp.requestID] = pp
	b.permMu.Unlock()
}

// getPendingPerm returns a pending permission without removing it.
// Used by sequential question flows that need to read state across steps.
func (b *Backend) getPendingPerm(requestID string) *pendingPermission {
	b.permMu.Lock()
	pp := b.pendingPerms[requestID]
	b.permMu.Unlock()
	return pp
}

// removePendingPerm removes and returns a pending permission. The
// "all-clear" signal is fired by OutstandingRegistry's onEmpty hook, not
// inferred locally — both perms and elicitations live in one registry, so
// the registry's view of "empty" is the only correct one.
func (b *Backend) removePendingPerm(requestID string) (pp *pendingPermission, found bool) {
	b.permMu.Lock()
	pp, found = b.pendingPerms[requestID]
	delete(b.pendingPerms, requestID)
	b.permMu.Unlock()
	return
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// DisplayText formats the permission request for display.
func (req *PermissionRequestPayload) DisplayText() string {
	var b strings.Builder
	b.WriteString("**Permission Required**\n\n")
	if req.DisplayName != "" {
		b.WriteString(fmt.Sprintf("**%s**", req.DisplayName))
		if req.Title != "" {
			b.WriteString(fmt.Sprintf(": %s", req.Title))
		}
		b.WriteString("\n\n")
	} else if req.ToolName != "" {
		b.WriteString(fmt.Sprintf("**%s**\n\n", req.ToolName))
	}
	if req.Description != "" {
		b.WriteString(req.Description)
		b.WriteString("\n\n")
	}
	// Show tool input (e.g. the command being run) when available.
	if len(req.Input) > 0 && string(req.Input) != "{}" && string(req.Input) != "null" {
		toolName := req.ToolName
		if toolName == "" {
			toolName = req.DisplayName
		}
		b.WriteString(formatToolInput(toolName, req.Input))
		b.WriteString("\n\n")
	}
	if req.DecisionReason != "" {
		b.WriteString(fmt.Sprintf("_Reason: %s_", friendlyReason(req.DecisionReason)))
	}
	return b.String()
}

// formatToolInput extracts a human-readable summary from tool input JSON.
// Uses toolName to determine which input field to display, aligned with
// toolMatchKeys in autoapprove.go.
//
// Fields that may contain arbitrary shell/regex text (command, pattern, url,
// query, and the JSON fallback) are rendered as fenced code blocks rather
// than inline code. Inline code delimiters (single backticks) would be
// broken by internal backticks in the content — e.g. a grep command like
// `grep -oP '`(high|med|low)`'` would pair the wrapper backticks with the
// internal ones, losing part of the command in the markdown converter.
// Fenced blocks delimit on triple backticks, which shell content practically
// never contains.
func formatToolInput(toolName string, input json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return fencedBlock(string(input))
	}

	// Use toolMatchKeys to find the primary input field for this tool.
	if key, ok := toolMatchKeys[toolName]; ok {
		if raw, ok := m[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				switch key {
				case "command":
					return fencedBlock(s)
				case "file_path":
					return fmt.Sprintf("File: `%s`", s)
				case "pattern":
					return "Pattern:\n" + fencedBlock(s)
				case "url":
					return "URL:\n" + fencedBlock(s)
				case "query":
					return "Query:\n" + fencedBlock(s)
				default:
					return fencedBlock(s)
				}
			}
		}
	}

	// Fallback: compact JSON.
	compact, err := json.Marshal(m)
	if err != nil {
		return fencedBlock(string(input))
	}
	s := string(compact)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fencedBlock(s)
}

// fencedBlock wraps s in a triple-backtick markdown code fence. The fence
// protects content from inline-code regex matching in the downstream
// markdown-to-HTML converter — single backticks inside s are passed through
// untouched.
func fencedBlock(s string) string {
	return "```\n" + s + "\n```"
}

// friendlyReason rewrites CC's internal decision reasons into user-facing text.
// CC's bash security parser emits technical strings like "Unhandled node type: string"
// which are meaningless to users.
func friendlyReason(reason string) string {
	if strings.HasPrefix(reason, "Unhandled node type:") ||
		strings.HasPrefix(reason, "Contains ") ||
		reason == "Parse error" {
		return "Command requires manual review"
	}
	return reason
}

// Summary creates a short summary for post-approval display.
func (req *PermissionRequestPayload) Summary() string {
	if req.Description != "" {
		return req.Description
	}
	if req.DisplayName != "" && req.Title != "" {
		return fmt.Sprintf("%s: %s", req.DisplayName, req.Title)
	}
	return req.ToolName
}

// Choices creates the button choices for the platform UI.
func (req *PermissionRequestPayload) Choices() []delegator.PromptChoice {
	choices := []delegator.PromptChoice{
		{Label: "Allow", Data: "allow"},
		{Label: "Deny", Data: "deny"},
	}
	for _, s := range req.PermissionSuggestions {
		if s.Prefix != "" {
			choices = append(choices, delegator.PromptChoice{
				Label: fmt.Sprintf("Always: %s", s.Prefix),
				Data:  fmt.Sprintf("allow_always:%s", s.Prefix),
			})
		}
	}
	return choices
}
