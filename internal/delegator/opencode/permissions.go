// permissions.go — permission handling for the opencode backend.
// opencode surfaces tool approvals AND the built-in `question` tool's
// prompts through a single event type: permission.updated. This file
// handles both, routing question-type permissions to QuestionResponder
// and everything else to the binary Allow/Deny/Always path.

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"foci/internal/delegator"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// Permission event dispatch (wired from handlers.go's handleEvent)
// ---------------------------------------------------------------------------

// onPermissionUpdated handles a permission.updated SSE event. Stores
// the permission, registers it in the OutstandingRegistry, and surfaces
// it to the user via permPromptFn. Question-type permissions are routed
// to handleQuestionPermission; all others get the binary Allow/Deny/
// Always-Allow keyboard.
func (b *Backend) onPermissionUpdated(perm Permission) {
	// Store under permMu so RespondToPermission (caller's goroutine)
	// and the dispatcher goroutine don't race.
	b.permMu.Lock()
	b.pendingPerms[perm.ID] = &pendingPermission{
		id:       perm.ID,
		permType: perm.Type,
		title:    perm.Title,
		metadata: perm.Metadata,
	}
	b.permMu.Unlock()

	// Register in the OutstandingRegistry so WaitForPermission blocks
	// and the onEmpty drain hook fires when resolved.
	b.outstanding.Register(perm.ID, delegator.OutstandingPermission)

	// Route question-type permissions to the question path.
	if perm.Type == PermQuestion {
		b.handleQuestionPermission(perm)
		return
	}

	// Regular permission: surface via permPromptFn with Allow/Deny/Always.
	if b.permPromptFn == nil {
		log.Warnf(b.logComponent(), "onPermissionUpdated: permPromptFn nil — prompt %s stored but not displayed", perm.ID)
		return
	}

	choices := []delegator.PromptChoice{
		{Label: "Allow", Data: "allow"},
		{Label: "Deny", Data: "deny"},
		{Label: "Always Allow", Data: "always"},
	}
	b.permPromptFn(perm.ID, perm.Title, perm.Title, "", choices)
}

// onPermissionReplied handles a permission.replied SSE event. opencode
// emits this when the user responds out-of-band (e.g. via the TUI).
// We treat it as a cancel of OUR prompt UI: fire the cancel listeners
// (so the platform disables the orphaned inline keyboard) and remove
// from the registry so the drain hook fires.
func (b *Backend) onPermissionReplied(sessionID, permissionID, response string) {
	// Cancel fires cancel listeners + removes from registry + fires
	// onEmpty if empty. The reason tells listeners this was an
	// out-of-band reply, not a user-initiated cancel.
	b.outstanding.Cancel(permissionID, "replied out-of-band")

	b.permMu.Lock()
	delete(b.pendingPerms, permissionID)
	b.permMu.Unlock()

	log.Debugf(b.logComponent(), "permission %s replied out-of-band: %s", permissionID, response)
}

// ---------------------------------------------------------------------------
// RespondToPermission — the platform layer calls this when the user
// clicks Allow/Deny/Always on the inline keyboard.
// ---------------------------------------------------------------------------

// RespondToPermission sends the user's permission response to opencode
// and resolves the outstanding prompt. `allow` selects allow vs deny;
// `remember` maps to opencode's "remember this decision" flag.
func (b *Backend) RespondToPermission(permID string, allow bool, remember bool) error {
	b.permMu.Lock()
	_, ok := b.pendingPerms[permID]
	b.permMu.Unlock()
	if !ok {
		return fmt.Errorf("opencode: no pending permission with ID %q", permID)
	}

	response := "deny"
	if allow {
		response = "allow"
	}

	// POST /session/:id/permissions/:permID { response, remember? }
	if err := b.postPermissionResponse(permID, response, remember); err != nil {
		return err
	}

	// Clean up.
	b.outstanding.Resolve(permID)
	b.permMu.Lock()
	delete(b.pendingPerms, permID)
	b.permMu.Unlock()

	return nil
}

// postPermissionResponse POSTs the user's decision to opencode.
func (b *Backend) postPermissionResponse(permID, response string, remember bool) error {
	body := map[string]any{"response": response}
	if remember {
		body["remember"] = true
	}
	payload, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/session/%s/permissions/%s", b.server.baseURL, b.sessionID, permID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("POST /permissions/%s: %w", permID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /permissions/%s: HTTP %d", permID, resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// QuestionResponder — delegator.QuestionResponder implementation.
// opencode's built-in `question` tool surfaces as a permission.updated
// event with type:"question". The metadata carries the question schema
// (header, text, options). The user responds via button click (option
// label) or typed answer; the response goes back through the same
// POST /session/:id/permissions/:permID endpoint.
// ---------------------------------------------------------------------------

// questionMetadata is the metadata payload for a question-type permission.
type questionMetadata struct {
	Header  string         `json:"header"`
	Text    string         `json:"text"`
	Options []questionOption `json:"options"`
}

type questionOption struct {
	Label string `json:"label"`
}

// handleQuestionPermission renders a question-type permission via
// permPromptFn. The option list becomes the button choices; the user
// can also type a custom answer (the platform layer intercepts typed
// text and calls RespondToQuestion).
func (b *Backend) handleQuestionPermission(perm Permission) {
	if b.permPromptFn == nil {
		log.Warnf(b.logComponent(), "handleQuestionPermission: permPromptFn nil — question %s not displayed", perm.ID)
		return
	}

	// Parse the question metadata.
	var qm questionMetadata
	if err := json.Unmarshal(perm.Metadata, &qm); err != nil {
		log.Warnf(b.logComponent(), "handleQuestionPermission: unmarshal metadata: %v", err)
		// Fall back to the raw title as the prompt text.
		b.permPromptFn(perm.ID, perm.Title, perm.Title, "", []delegator.PromptChoice{})
		return
	}

	// Build choices from the options.
	choices := make([]delegator.PromptChoice, len(qm.Options))
	for i, opt := range qm.Options {
		choices[i] = delegator.PromptChoice{Label: opt.Label, Data: opt.Label}
	}

	// Prompt text: "Header: Text" or just "Text".
	text := qm.Text
	if qm.Header != "" {
		text = qm.Header + ": " + qm.Text
	}
	b.permPromptFn(perm.ID, text, qm.Header, "", choices)
}

// RespondToQuestion sends the user's answer to a question-type permission.
// `choice` is either the option label (from a button click) or a typed
// string. Implements delegator.QuestionResponder.
func (b *Backend) RespondToQuestion(requestID, choice string) error {
	b.permMu.Lock()
	pp, ok := b.pendingPerms[requestID]
	b.permMu.Unlock()
	if !ok {
		return fmt.Errorf("opencode: no pending permission with ID %q", requestID)
	}
	if pp.permType != PermQuestion {
		return fmt.Errorf("opencode: permission %q is type %q, not a question", requestID, pp.permType)
	}

	// The response for a question is the chosen option or typed text.
	if err := b.postPermissionResponse(requestID, choice, false); err != nil {
		return err
	}

	b.outstanding.Resolve(requestID)
	b.permMu.Lock()
	delete(b.pendingPerms, requestID)
	b.permMu.Unlock()
	return nil
}

// CancelQuestion cancels a pending question. Sends "deny" as the
// response — opencode treats this as "user declined to answer".
// Implements delegator.QuestionResponder.
func (b *Backend) CancelQuestion(requestID string) error {
	b.permMu.Lock()
	_, ok := b.pendingPerms[requestID]
	b.permMu.Unlock()
	if !ok {
		return fmt.Errorf("opencode: no pending permission with ID %q", requestID)
	}

	if err := b.postPermissionResponse(requestID, "deny", false); err != nil {
		return err
	}

	b.outstanding.Resolve(requestID)
	b.permMu.Lock()
	delete(b.pendingPerms, requestID)
	b.permMu.Unlock()
	return nil
}

// HasPendingQuestion returns the request ID of a pending question-type
// permission, or "" if none. Implements delegator.QuestionResponder.
// Guards against returning a non-question permission ID.
func (b *Backend) HasPendingQuestion() string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	for id, pp := range b.pendingPerms {
		if pp.permType == PermQuestion {
			return id
		}
	}
	return ""
}
