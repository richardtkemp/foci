package ccstream

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/delegator"
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
	b.logger().Debugf("handleToolRequest: req_id=%s tool=%s", msg.RequestID, msg.Request.ToolName)

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

	// ExitPlanMode: the plan markdown is large, and the generic formatter would
	// truncate input.plan to a 200-char JSON blob. CC already writes the full
	// plan to input.planFilePath (under ~/.claude/plans); attach that file and
	// replace the prompt body with a short caption. If the file is missing or
	// unreadable, attachmentPath stays "" and we fall back to the generic
	// (truncated) rendering. The Allow/Deny choices are unchanged: over the
	// stdio permission protocol ExitPlanMode is a plain binary gate — the
	// auto-accept/manual/keep-planning menu is a CC-TUI-only feature and is not
	// exposed here (verified empirically: the can_use_tool request carries no
	// permission_suggestions/choices).
	var attachmentPath string
	if msg.Request.ToolName == "ExitPlanMode" {
		if p := planAttachmentPath(msg.Request.Input); p != "" {
			attachmentPath = p
			text = "📋 **Plan ready** — see the attached file.\n\nApprove to exit plan mode and proceed?"
			summary = "Plan"
		}
	}

	pp := &pendingPermission{
		requestID:   msg.RequestID,
		toolName:    msg.Request.ToolName,
		toolUseID:   msg.Request.ToolUseID,
		description: msg.Request.Description,
		summary:     summary,
		createdAt:   time.Now(),
	}

	b.storePendingPerm(pp)
	b.outstanding.Register(msg.RequestID, delegator.OutstandingPermission)

	if b.permPromptFn != nil {
		b.logger().Debugf("calling permPromptFn for req_id=%s", msg.RequestID)
		b.permPromptFn(msg.RequestID, text, summary, attachmentPath, choices)
	} else {
		b.logger().Warnf("permPromptFn nil for req_id=%s, prompt stored but not displayed", msg.RequestID)
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

// HasPendingPlanPermission returns the request ID of a pending ExitPlanMode
// (plan-approval) permission, or "" if none. Used by the inbound path to detect
// when a typed message should cancel the plan (delivering the text as revision
// feedback) instead of queuing behind it. Distinct from HasPendingQuestion: a
// plan permission has toolName "ExitPlanMode" and no question state.
func (b *Backend) HasPendingPlanPermission() string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	for _, pp := range b.pendingPerms {
		if pp.toolName == "ExitPlanMode" {
			return pp.requestID
		}
	}
	return ""
}

// CancelPlanWithFeedback denies a pending ExitPlanMode permission, passing the
// user's typed text to CC as the rejection message. CC stays in plan mode and
// revises the plan using the feedback (its native plan-revise loop), then
// re-presents. The prompt's registered cancel listener is fired via
// outstanding.Cancel so the now-stale Allow/Deny buttons are edited away, and
// clearing the outstanding entry (→ onEmpty) unblocks any worker parked in
// WaitForPermission. Only valid for an ExitPlanMode request.
func (b *Backend) CancelPlanWithFeedback(requestID, feedback string) error {
	pp, ok := b.removePendingPerm(requestID)
	if !ok {
		return fmt.Errorf("ccstream: no pending plan permission with request ID %q", requestID)
	}
	resp := &PermissionDeny{
		Behavior:               "deny",
		Message:                feedback,
		Interrupt:              false,
		ToolUseID:              pp.toolUseID,
		DecisionClassification: "user_reject",
	}
	if err := b.writer.SendControlResponse(requestID, resp); err != nil {
		return err
	}
	// Use Cancel, not Resolve: only Cancel notifies the prompt's cancel listener,
	// which performs the button edit ("❌ Plan cancelled by follow-up message").
	b.outstanding.Cancel(requestID, "plan revised by follow-up message")
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
// The delegator.OutstandingRegistry fires any per-prompt cancel listeners registered
// by the platform layer (e.g. to disable the orphaned inline keyboard so
// the user can't click an already-resolved button) and the registry-wide
// onEmpty hook if this was the last outstanding prompt.
func (b *Backend) handleControlCancel(reqID string) {
	pp, _ := b.removePendingPerm(reqID)

	tool := ""
	if pp != nil {
		tool = pp.toolName
	}
	b.logger().Infof("permission auto-cancelled by CC control_cancel_request: reqID=%s tool=%s", reqID, tool)

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
// "all-clear" signal is fired by delegator.OutstandingRegistry's onEmpty hook, not
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

// toolMatchKeys maps CC tool names to the JSON input field used for display.
// Mirrors the map in the shared autoapprove package (which uses it for
// security-critical matching); this local copy is for display only.
var toolMatchKeys = map[string]string{
	"Bash":         "command",
	"Read":         "file_path",
	"Edit":         "file_path",
	"Write":        "file_path",
	"NotebookEdit": "file_path",
	"Glob":         "pattern",
	"Grep":         "pattern",
	"WebFetch":     "url",
	"WebSearch":    "query",
}

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

// maxDiffChars bounds the rendered diff so the re-rendered prompt stays within
// platform message-length limits (Telegram caps at 4096); an over-limit edit
// would silently fail and the toggle would appear dead.
const maxDiffChars = 3500

// formatEditDiff renders the proposed change for an Edit/MultiEdit permission
// request as a red/green line diff, fenced to protect arbitrary code content
// from the markdown converter. Returns "" for other tools or unparseable input.
func formatEditDiff(toolName string, input json.RawMessage) string {
	switch toolName {
	case "Edit":
		var e struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if json.Unmarshal(input, &e) != nil {
			return ""
		}
		return fencedBlock(truncateDiff(diffLines(e.OldString, e.NewString)))
	case "MultiEdit":
		var m struct {
			Edits []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(input, &m) != nil || len(m.Edits) == 0 {
			return ""
		}
		parts := make([]string, len(m.Edits))
		for i, e := range m.Edits {
			parts[i] = fmt.Sprintf("── edit %d ──\n%s", i+1, diffLines(e.OldString, e.NewString))
		}
		return fencedBlock(truncateDiff(strings.Join(parts, "\n\n")))
	}
	return ""
}

func diffLines(oldS, newS string) string {
	var b strings.Builder
	for _, ln := range strings.Split(oldS, "\n") {
		b.WriteString("🔴 " + neutralizeFence(ln) + "\n")
	}
	for _, ln := range strings.Split(newS, "\n") {
		b.WriteString("🟢 " + neutralizeFence(ln) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// neutralizeFence breaks any triple-backtick run in edited content so it can't
// close the code fence that wraps the diff (common when editing markdown/Go
// files). A zero-width space between the backticks is invisible in the client.
func neutralizeFence(s string) string {
	return strings.ReplaceAll(s, "```", "`​`​`")
}

func truncateDiff(s string) string {
	if len(s) <= maxDiffChars {
		return s
	}
	return s[:maxDiffChars] + "\n… (truncated)"
}

// planAttachmentPath returns a readable file path holding the full plan
// markdown for an ExitPlanMode request, or "" if none is available. CC writes
// the plan to input.planFilePath itself (under ~/.claude/plans), so we attach
// that file directly — there is no temp file for foci to create or clean up.
// Returns "" when the field is absent or the file is missing/unreadable, in
// which case the caller falls back to the generic prompt rendering.
func planAttachmentPath(input json.RawMessage) string {
	var p struct {
		PlanFilePath string `json:"planFilePath"`
	}
	if err := json.Unmarshal(input, &p); err != nil || p.PlanFilePath == "" {
		return ""
	}
	st, err := os.Stat(p.PlanFilePath)
	if err != nil || st.IsDir() {
		return ""
	}
	return p.PlanFilePath
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
	if diff := formatEditDiff(req.ToolName, req.Input); diff != "" {
		choices = append(choices, delegator.PromptChoice{
			Label: "Show diff",
			Data:  "showdiff",
			Toggle: &delegator.PromptToggle{
				ExtraBody: diff,
				ShowLabel: "Show diff",
				HideLabel: "Hide diff",
			},
		})
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
