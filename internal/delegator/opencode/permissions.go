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
	"path/filepath"
	"strings"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
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
	b.surfacePermission(pendingPermission{
		id:        perm.ID,
		permType:  perm.Type,
		title:     perm.Title,
		metadata:  perm.Metadata,
		replyNext: false, // legacy: reply via POST /session/:id/permissions/:id
	})
}

// onPermissionAsked handles a permission.asked SSE event (opencode 1.2.x). It
// maps the PermissionRequest onto the same surfacing path as the legacy
// permission.updated, building a human title (the request carries none) and
// flagging the reply to go via POST /permission/{id}/reply (#arnix-perm).
func (b *Backend) onPermissionAsked(req PermissionRequest) {
	title := req.Permission
	if len(req.Patterns) > 0 {
		title = req.Permission + ": " + strings.Join(req.Patterns, ", ")
	}
	b.surfacePermission(pendingPermission{
		id:        req.ID,
		permType:  req.Permission,
		title:     title,
		patterns:  req.Patterns,
		metadata:  req.Metadata,
		replyNext: true, // 1.2.x: reply via POST /permission/{id}/reply
	})
}

// surfacePermission stores a pending permission, registers it for
// WaitForPermission, and surfaces it to the user — routing question-type
// permissions to handleQuestionPermission and everything else to the binary
// Allow/Deny/Always keyboard. Shared by both opencode permission models.
func (b *Backend) surfacePermission(pp pendingPermission) {
	// Store under permMu so RespondToPermission (caller's goroutine) and the
	// dispatcher goroutine don't race.
	b.permMu.Lock()
	// Dedup: if a regular (non-question) permission for the SAME target is
	// already pending and surfaced, mark this one an alias instead of raising a
	// second identical prompt. opencode emits one permission object per tool
	// call, so two calls touching the same dir produce two asks in the same
	// instant; without this the user gets duplicate prompts. We still answer
	// every alias (see RespondToPermission) — opencode blocks each separately.
	if pp.permType != PermQuestion {
		key := permDedupKey(pp)
		for _, ex := range b.pendingPerms {
			if ex.aliasOf == "" && ex.permType != PermQuestion && permDedupKey(*ex) == key {
				pp.aliasOf = ex.id
				break
			}
		}
	}
	stored := pp
	b.pendingPerms[pp.id] = &stored
	b.permMu.Unlock()

	// Register in the OutstandingRegistry so WaitForPermission blocks and the
	// onEmpty drain hook fires when resolved. Aliases register too — opencode
	// is genuinely waiting on them; the turn isn't done until all are replied.
	b.outstanding.Register(pp.id, delegator.OutstandingPermission)

	// Aliased duplicate: tracked + answered with its primary, but no 2nd prompt.
	if stored.aliasOf != "" {
		log.Debugf(b.logComponent(), "permission %s deduped: alias of %s (target=%q)", pp.id, stored.aliasOf, pp.title)
		return
	}

	// Auto-approve pre-filter: if foci's compiled rules match ALL patterns,
	// reply "allow" (one-shot) directly without prompting the user. This
	// restores the defense-in-depth the ccstream auto-approve engine
	// provided — bash command segmentation, unsafe-flag rejection, path
	// traversal canonicalization, symlink escape detection.
	if b.checkAutoApprove(pp) {
		log.Infof(b.logComponent(), "auto-approved: type=%s patterns=%v id=%s", pp.permType, pp.patterns, pp.id)
		if err := b.sendPermissionReply(pp, true, false); err != nil {
			log.Warnf(b.logComponent(), "auto-approve reply failed (%v) — falling through to user prompt", err)
		} else {
			b.outstanding.Cancel(pp.id, "auto-approved")
			b.permMu.Lock()
			delete(b.pendingPerms, pp.id)
			b.permMu.Unlock()
			return
		}
	}

	// Route question-type permissions to the question path.
	if pp.permType == PermQuestion {
		b.handleQuestionPermission(Permission{ID: pp.id, Type: pp.permType, Title: pp.title, Metadata: pp.metadata})
		return
	}

	// Regular permission: surface via permPromptFn with Allow/Deny/Always.
	if b.permPromptFn == nil {
		log.Warnf(b.logComponent(), "surfacePermission: permPromptFn nil — prompt %s stored but not displayed", pp.id)
		return
	}

	choices := []delegator.PromptChoice{
		{Label: "Allow", Data: "allow"},
		{Label: "Deny", Data: "deny"},
		{Label: "Always Allow", Data: "always"},
	}
	b.permPromptFn(pp.id, pp.title, pp.title, "", choices)
}

// permTypeToToolName maps opencode permission types to the tool names used
// by the auto-approve engine (matching ccstream's naming convention).
// Returns "" for types foci doesn't auto-approve (external_directory,
// question, unknown types).
var permTypeToToolName = map[string]string{
	"bash":      "Bash",
	"read":      "Read",
	"edit":      "Edit",
	"write":     "Write",
	"glob":      "Glob",
	"grep":      "Grep",
	"webfetch":  "WebFetch",
	"websearch": "WebSearch",
}

// checkAutoApprove evaluates pending permission patterns against foci's
// compiled auto-approve rules. Returns true if ALL patterns independently
// match and the permission should be auto-approved without prompting the
// user.
//
// For Bash, each pattern is a command source text (already AST-split by
// opencode's tree-sitter) — the engine re-parses it with mvdan/sh for
// structural safety (redirects, process substitution, unsafe flags).
// For path tools, patterns are workspace-relative paths resolved to
// absolute so the engine can canonicalize and match against workspace
// boundaries.
func (b *Backend) checkAutoApprove(pp pendingPermission) bool {
	if len(b.autoApproveRules) == 0 || len(pp.patterns) == 0 {
		return false
	}

	toolName, ok := permTypeToToolName[pp.permType]
	if !ok {
		return false
	}

	for _, pattern := range pp.patterns {
		var input json.RawMessage
		switch toolName {
		case "Bash":
			input, _ = json.Marshal(map[string]string{"command": pattern})
		case "Read", "Edit", "Write":
			absPath := pattern
			if !filepath.IsAbs(absPath) && b.workDir != "" {
				absPath = filepath.Join(b.workDir, pattern)
			}
			input, _ = json.Marshal(map[string]string{"file_path": absPath})
		default:
			input, _ = json.Marshal(map[string]string{"pattern": pattern})
		}

		if !autoapprove.Match(b.autoApproveRules, toolName, input) {
			return false
		}
	}

	return true
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

	log.Debugf(b.logComponent(), "permission %s replied out-of-band (session=%s): %s", permissionID, sessionID, response)
}

// ---------------------------------------------------------------------------
// RespondToPermission — the platform layer calls this when the user
// clicks Allow/Deny/Always on the inline keyboard.
// ---------------------------------------------------------------------------

// RespondToPermission sends the user's permission response to opencode
// and resolves the outstanding prompt. `allow` selects allow vs deny;
// `remember` maps to opencode's "remember this decision" flag.
//
// The user only ever sees the primary prompt for a deduped group, so the
// single decision is fanned out to the primary AND every alias pointing at it
// — each is a distinct opencode permission object blocking its own tool call.
func (b *Backend) RespondToPermission(permID string, allow bool, remember bool) error {
	b.permMu.Lock()
	pp, ok := b.pendingPerms[permID]
	if !ok {
		b.permMu.Unlock()
		return fmt.Errorf("opencode: no pending permission with ID %q", permID)
	}
	// Resolve to the group root (in case permID is itself an alias) and gather
	// the whole group: the root plus every alias pointing at it.
	root := permID
	if pp.aliasOf != "" {
		root = pp.aliasOf
	}
	var group []pendingPermission
	for id, ex := range b.pendingPerms {
		if id == root || ex.aliasOf == root {
			group = append(group, *ex)
		}
	}
	b.permMu.Unlock()

	// Reply to every member with the same decision. Resolve/delete only the
	// members that succeed, so a failed POST leaves that one tracked.
	var firstErr error
	for _, member := range group {
		if err := b.sendPermissionReply(member, allow, remember); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Warnf(b.logComponent(), "reply to permission %s failed: %v", member.id, err)
			continue
		}
		b.outstanding.Resolve(member.id)
		b.permMu.Lock()
		delete(b.pendingPerms, member.id)
		b.permMu.Unlock()
	}
	return firstErr
}

// sendPermissionReply POSTs a single permission decision to opencode, choosing
// the transport from the permission's replyNext flag. Does not touch
// outstanding/pendingPerms — the caller owns lifecycle cleanup.
func (b *Backend) sendPermissionReply(pp pendingPermission, allow, remember bool) error {
	if pp.replyNext {
		// opencode 1.2.x: POST /permission/{id}/reply { reply }. allow+remember →
		// "always" (persist the rule), allow → "once", deny → "reject".
		reply := "reject"
		if allow {
			reply = "once"
			if remember {
				reply = "always"
			}
		}
		return b.replyPermissionNext(pp.id, reply)
	}
	// Legacy: POST /session/:id/permissions/:permID { response, remember? }.
	response := "deny"
	if allow {
		response = "allow"
	}
	return b.postPermissionResponse(pp.id, response, remember)
}

// permDedupKey identifies a permission's target for dedup: same type + same
// title (the title encodes the permission patterns for permission.asked and the
// free-form title for legacy permission.updated).
func permDedupKey(pp pendingPermission) string {
	return pp.permType + "\x00" + pp.title
}

// replyPermissionNext replies to a permission.asked request via opencode 1.2.x's
// POST /permission/{requestID}/reply { reply } endpoint (reply ∈ once|always|
// reject). Verified against the live 1.2.18 OpenAPI.
func (b *Backend) replyPermissionNext(requestID, reply string) error {
	payload, _ := json.Marshal(map[string]any{"reply": reply})
	url := fmt.Sprintf("%s/permission/%s/reply", b.server.baseURL, requestID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("POST /permission/%s/reply: %w", requestID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /permission/%s/reply: HTTP %d", requestID, resp.StatusCode)
	}
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
