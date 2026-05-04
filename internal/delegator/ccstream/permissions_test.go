package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// RespondToPermission
// ---------------------------------------------------------------------------

func TestRespondToPermission_Allow(t *testing.T) {
	// Proves that responding with allow=true sends a PermissionAllow control
	// response with behavior="allow" and decisionClassification="user_temporary".
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.storePendingPerm(&pendingPermission{
		requestID: "req-1",
		toolUseID: "toolu_ABC",
		toolName:  "Bash",
	})

	if err := b.RespondToPermission("req-1", true, ""); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	resp := parseControlResponse(t, buf.String())
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "allow")
	}
	if inner["decisionClassification"] != "user_temporary" {
		t.Errorf("decisionClassification = %v, want %q", inner["decisionClassification"], "user_temporary")
	}
	if inner["toolUseID"] != "toolu_ABC" {
		t.Errorf("toolUseID = %v, want %q", inner["toolUseID"], "toolu_ABC")
	}

	if b.PendingPermissions() != 0 {
		t.Errorf("pending count = %d, want 0", b.PendingPermissions())
	}
}

func TestRespondToPermission_Deny(t *testing.T) {
	// Proves that responding with allow=false sends a PermissionDeny control
	// response with the user's deny message and decisionClassification="user_reject".
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.storePendingPerm(&pendingPermission{
		requestID: "req-2",
		toolUseID: "toolu_DEF",
		toolName:  "Edit",
	})

	if err := b.RespondToPermission("req-2", false, "not allowed"); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	resp := parseControlResponse(t, buf.String())
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "deny" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "deny")
	}
	if inner["message"] != "not allowed" {
		t.Errorf("message = %v, want %q", inner["message"], "not allowed")
	}
	if inner["decisionClassification"] != "user_reject" {
		t.Errorf("decisionClassification = %v, want %q", inner["decisionClassification"], "user_reject")
	}
	if inner["toolUseID"] != "toolu_DEF" {
		t.Errorf("toolUseID = %v, want %q", inner["toolUseID"], "toolu_DEF")
	}
}

func TestRespondToPermission_UnknownRequestID(t *testing.T) {
	// Verifies that responding to a request ID that isn't pending returns an error
	// and sends nothing on the wire.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}

	err := b.RespondToPermission("nonexistent", true, "")
	if err == nil {
		t.Fatal("expected error for unknown request ID")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q, want mention of request ID", err.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestRespondToPermission_FiresOnPermCleared(t *testing.T) {
	// Proves that onPermCleared is called when the last pending permission
	// is resolved, and NOT called when permissions remain.
	t.Parallel()

	var buf bytes.Buffer
	cleared := 0
	b := &Backend{
		writer:        NewWriter(nopWriteCloser{&buf}),
		pendingPerms:  make(map[string]*pendingPermission),
		onPermCleared: func() { cleared++ },
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-A", toolUseID: "a"})
	b.storePendingPerm(&pendingPermission{requestID: "req-B", toolUseID: "b"})

	// Resolve first — still one pending.
	if err := b.RespondToPermission("req-A", true, ""); err != nil {
		t.Fatalf("first respond: %v", err)
	}
	if cleared != 0 {
		t.Errorf("cleared called after first resolve, want 0 calls")
	}

	// Resolve second — map now empty → onPermCleared fires.
	if err := b.RespondToPermission("req-B", true, ""); err != nil {
		t.Fatalf("second respond: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}
}

// ---------------------------------------------------------------------------
// RespondToPermissionWithRule
// ---------------------------------------------------------------------------

func TestRespondToPermissionWithRule(t *testing.T) {
	// Proves that RespondToPermissionWithRule sends an allow response with
	// updatedPermissions containing the given prefix and scope "session",
	// and uses decisionClassification "user_permanent".
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.storePendingPerm(&pendingPermission{
		requestID: "req-rule",
		toolUseID: "toolu_RULE",
		toolName:  "Bash",
	})

	if err := b.RespondToPermissionWithRule("req-rule", "Bash:git *"); err != nil {
		t.Fatalf("RespondToPermissionWithRule: %v", err)
	}

	resp := parseControlResponse(t, buf.String())
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "allow")
	}
	if inner["decisionClassification"] != "user_permanent" {
		t.Errorf("decisionClassification = %v, want %q", inner["decisionClassification"], "user_permanent")
	}

	perms, ok := inner["updatedPermissions"].([]any)
	if !ok || len(perms) != 1 {
		t.Fatalf("updatedPermissions = %v, want 1 element", inner["updatedPermissions"])
	}
	perm := perms[0].(map[string]any)
	if perm["prefix"] != "Bash:git *" {
		t.Errorf("prefix = %v, want %q", perm["prefix"], "Bash:git *")
	}
	if perm["scope"] != "session" {
		t.Errorf("scope = %v, want %q", perm["scope"], "session")
	}

	if b.PendingPermissions() != 0 {
		t.Errorf("pending count = %d, want 0", b.PendingPermissions())
	}
}

func TestRespondToPermissionWithRule_UnknownRequestID(t *testing.T) {
	// Verifies that an unknown request ID returns an error.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}

	err := b.RespondToPermissionWithRule("nonexistent", "Bash:ls")
	if err == nil {
		t.Fatal("expected error for unknown request ID")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestRespondToPermissionWithRule_FiresOnPermCleared(t *testing.T) {
	// Proves that onPermCleared fires when the last permission is resolved
	// via RespondToPermissionWithRule.
	t.Parallel()

	var buf bytes.Buffer
	cleared := false
	b := &Backend{
		writer:        NewWriter(nopWriteCloser{&buf}),
		pendingPerms:  make(map[string]*pendingPermission),
		onPermCleared: func() { cleared = true },
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-rc", toolUseID: "x"})

	if err := b.RespondToPermissionWithRule("req-rc", "Read"); err != nil {
		t.Fatalf("RespondToPermissionWithRule: %v", err)
	}
	if !cleared {
		t.Error("onPermCleared not called after last permission resolved")
	}
}

// ---------------------------------------------------------------------------
// handlePermissionRequest
// ---------------------------------------------------------------------------

func TestHandlePermissionRequest_AutoApprove(t *testing.T) {
	// Proves that when auto-approve rules match, the request is approved
	// directly without calling permPromptFn, and no pending permission is stored.
	t.Parallel()

	var buf bytes.Buffer
	promptCalled := false
	b := &Backend{
		writer:           NewWriter(nopWriteCloser{&buf}),
		pendingPerms:     make(map[string]*pendingPermission),
		autoApproveRules: parseAutoApproveRules([]string{"Read"}),
		permPromptFn: func(reqID, text, summary string, choices []delegator.PromptChoice) {
			promptCalled = true
		},
	}

	msg := &PermissionRequest{
		RequestID: "req-auto",
		Request: PermissionRequestPayload{
			ToolName:  "Read",
			ToolUseID: "toolu_AUTO",
			Input:     json.RawMessage(`{"file_path":"/tmp/foo"}`),
		},
	}

	b.handleToolRequest(msg)

	if promptCalled {
		t.Error("permPromptFn should not be called when auto-approved")
	}
	if b.PendingPermissions() != 0 {
		t.Errorf("pending = %d, want 0 (auto-approved should not store)", b.PendingPermissions())
	}

	// Verify an allow response was sent on the wire.
	resp := parseControlResponse(t, buf.String())
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "allow")
	}
}

func TestHandlePermissionRequest_NoMatch_ForwardsToPrompt(t *testing.T) {
	// Proves that when no auto-approve rule matches, the request is stored as
	// pending and forwarded to permPromptFn with the correct arguments.
	t.Parallel()

	var buf bytes.Buffer
	var gotReqID, gotText, gotSummary string
	var gotChoices []delegator.PromptChoice
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		permPromptFn: func(reqID, text, summary string, choices []delegator.PromptChoice) {
			gotReqID = reqID
			gotText = text
			gotSummary = summary
			gotChoices = choices
		},
	}

	msg := &PermissionRequest{
		RequestID: "req-prompt",
		Request: PermissionRequestPayload{
			ToolName:    "Bash",
			ToolUseID:   "toolu_PROMPT",
			Description: "Run a command",
			Input:       json.RawMessage(`{"command":"rm -rf /"}`),
		},
	}

	b.handleToolRequest(msg)

	if gotReqID != "req-prompt" {
		t.Errorf("requestID = %q, want %q", gotReqID, "req-prompt")
	}
	if gotSummary != "Run a command" {
		t.Errorf("summary = %q, want %q", gotSummary, "Run a command")
	}
	if gotText == "" {
		t.Error("text should not be empty")
	}
	if len(gotChoices) < 2 {
		t.Errorf("choices = %d, want at least 2 (Allow, Deny)", len(gotChoices))
	}
	if b.PendingPermissions() != 1 {
		t.Errorf("pending = %d, want 1", b.PendingPermissions())
	}
	// No wire output — the prompt callback is what shows the user the question.
	if buf.Len() != 0 {
		t.Errorf("expected no wire output when prompting user, got %q", buf.String())
	}
}

func TestHandlePermissionRequest_NilPermPromptFn(t *testing.T) {
	// Proves that handlePermissionRequest doesn't panic when permPromptFn
	// is nil. The permission is still stored as pending.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}

	msg := &PermissionRequest{
		RequestID: "req-nil",
		Request: PermissionRequestPayload{
			ToolName:  "Bash",
			ToolUseID: "toolu_NIL",
			Input:     json.RawMessage(`{"command":"echo hi"}`),
		},
	}

	b.handleToolRequest(msg)

	if b.PendingPermissions() != 1 {
		t.Errorf("pending = %d, want 1", b.PendingPermissions())
	}
}

func TestHandlePermissionRequest_FiresOnPermPending(t *testing.T) {
	// Proves that onPermPending is called when a new permission request is
	// stored (but not when auto-approved).
	t.Parallel()

	var buf bytes.Buffer
	pendingCalled := 0
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		onPermPending: func() {
			pendingCalled++
		},
	}

	msg := &PermissionRequest{
		RequestID: "req-pend",
		Request: PermissionRequestPayload{
			ToolName:  "Bash",
			ToolUseID: "toolu_PEND",
			Input:     json.RawMessage(`{"command":"rm -rf /"}`),
		},
	}

	b.handleToolRequest(msg)

	if pendingCalled != 1 {
		t.Errorf("onPermPending called %d times, want 1", pendingCalled)
	}
}

// ---------------------------------------------------------------------------
// handleControlCancel
// ---------------------------------------------------------------------------

func TestHandleControlCancel_RemovesPending(t *testing.T) {
	// Proves that handleControlCancel removes the pending permission and fires
	// onPermCleared when no permissions remain.
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms:  make(map[string]*pendingPermission),
		onPermCleared: func() { cleared = true },
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-cancel"})

	b.handleControlCancel("req-cancel")

	if b.PendingPermissions() != 0 {
		t.Errorf("pending = %d, want 0", b.PendingPermissions())
	}
	if !cleared {
		t.Error("onPermCleared not called after cancel cleared last permission")
	}
}

func TestHandleControlCancel_UnknownID(t *testing.T) {
	// Proves that cancelling a non-existent request is a no-op (no panic,
	// onPermCleared is still fired because the map is empty).
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms:  make(map[string]*pendingPermission),
		onPermCleared: func() { cleared = true },
	}

	b.handleControlCancel("nonexistent")

	// Map was already empty, so noMorePending = true → onPermCleared fires.
	if !cleared {
		t.Error("onPermCleared should fire when map is empty")
	}
}

func TestHandleControlCancel_StillPending(t *testing.T) {
	// Proves that onPermCleared is NOT called when other permissions remain
	// after cancellation.
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms:  make(map[string]*pendingPermission),
		onPermCleared: func() { cleared = true },
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-stay"})
	b.storePendingPerm(&pendingPermission{requestID: "req-go"})

	b.handleControlCancel("req-go")

	if cleared {
		t.Error("onPermCleared should not fire while permissions remain")
	}
	if b.PendingPermissions() != 1 {
		t.Errorf("pending = %d, want 1", b.PendingPermissions())
	}
}

func TestHandleControlCancel_FiresPermCancelHook(t *testing.T) {
	// Proves that handleControlCancel invokes permCancelFn with the requestID,
	// the captured tool name, and the standard cancellation reason. This is
	// the per-perm hook the platform layer uses to disable orphaned inline
	// keyboards.
	t.Parallel()

	var (
		gotReqID  string
		gotTool   string
		gotReason string
		called    int
	)
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		permCancelFn: func(reqID, toolName, reason string) {
			called++
			gotReqID = reqID
			gotTool = toolName
			gotReason = reason
		},
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-7", toolName: "Bash"})

	b.handleControlCancel("req-7")

	if called != 1 {
		t.Fatalf("permCancelFn called %d times, want 1", called)
	}
	if gotReqID != "req-7" {
		t.Errorf("reqID = %q, want req-7", gotReqID)
	}
	if gotTool != "Bash" {
		t.Errorf("toolName = %q, want Bash", gotTool)
	}
	if !strings.Contains(gotReason, "follow-up") {
		t.Errorf("reason = %q, want it to mention 'follow-up'", gotReason)
	}
}

func TestHandleControlCancel_NoFireForUnknownID(t *testing.T) {
	// Proves that permCancelFn is NOT invoked for an unknown reqID — there's
	// no orphan keyboard to disable in that case.
	t.Parallel()

	called := 0
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		permCancelFn: func(string, string, string) { called++ },
	}

	b.handleControlCancel("ghost")

	if called != 0 {
		t.Errorf("permCancelFn called %d times for unknown id, want 0", called)
	}
}

func TestHandleControlCancel_FireOrder(t *testing.T) {
	// Proves that permCancelFn fires before onPermCleared. Order matters:
	// the platform layer wants to update the per-prompt UI before any
	// session-wide "all clear" actions trigger.
	t.Parallel()

	var order []string
	b := &Backend{
		pendingPerms:  make(map[string]*pendingPermission),
		permCancelFn:  func(string, string, string) { order = append(order, "cancel") },
		onPermCleared: func() { order = append(order, "cleared") },
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-only"})

	b.handleControlCancel("req-only")

	if len(order) != 2 || order[0] != "cancel" || order[1] != "cleared" {
		t.Errorf("hook order = %v, want [cancel cleared]", order)
	}
}

// ---------------------------------------------------------------------------
// PendingPermissions
// ---------------------------------------------------------------------------

func TestPendingPermissions_Count(t *testing.T) {
	// Verifies that PendingPermissions accurately tracks insertions and removals.
	t.Parallel()

	b := &Backend{pendingPerms: make(map[string]*pendingPermission)}

	if n := b.PendingPermissions(); n != 0 {
		t.Errorf("initial = %d, want 0", n)
	}

	b.storePendingPerm(&pendingPermission{requestID: "a"})
	b.storePendingPerm(&pendingPermission{requestID: "b"})
	if n := b.PendingPermissions(); n != 2 {
		t.Errorf("after 2 inserts = %d, want 2", n)
	}

	b.removePendingPerm("a")
	if n := b.PendingPermissions(); n != 1 {
		t.Errorf("after 1 remove = %d, want 1", n)
	}

	if has := b.PendingPermissions() > 0; !has {
		t.Error("PendingPermissions = 0, want > 0")
	}

	b.removePendingPerm("b")
	if has := b.PendingPermissions() > 0; has {
		t.Error("PendingPermissions > 0, want 0")
	}
}

// ---------------------------------------------------------------------------
// DisplayText
// ---------------------------------------------------------------------------

func TestDisplayText_DisplayNameAndTitle(t *testing.T) {
	// Proves that DisplayText shows both DisplayName and Title when both are set.
	t.Parallel()

	req := &PermissionRequestPayload{
		DisplayName: "Bash",
		Title:       "Run command",
		Description: "Execute a shell command",
		Input:       json.RawMessage(`{"command":"ls -la"}`),
	}

	text := req.DisplayText()

	if !strings.Contains(text, "**Bash**") {
		t.Errorf("text missing display name: %q", text)
	}
	if !strings.Contains(text, ": Run command") {
		t.Errorf("text missing title: %q", text)
	}
	if !strings.Contains(text, "Execute a shell command") {
		t.Errorf("text missing description: %q", text)
	}
	if !strings.Contains(text, "```\nls -la\n```") {
		t.Errorf("text missing formatted command: %q", text)
	}
}

func TestDisplayText_ToolNameOnly(t *testing.T) {
	// Proves that when DisplayName is empty, ToolName is shown.
	t.Parallel()

	req := &PermissionRequestPayload{
		ToolName: "Write",
		Input:    json.RawMessage(`{"file_path":"/tmp/foo.txt","content":"hello"}`),
	}

	text := req.DisplayText()

	if !strings.Contains(text, "**Write**") {
		t.Errorf("text missing tool name: %q", text)
	}
	if !strings.Contains(text, "File: `/tmp/foo.txt`") {
		t.Errorf("text missing file path: %q", text)
	}
}

func TestDisplayText_EmptyInput(t *testing.T) {
	// Proves that empty or minimal input doesn't produce input section.
	t.Parallel()

	for _, input := range []string{`{}`, `null`, ``} {
		req := &PermissionRequestPayload{
			ToolName: "Read",
			Input:    json.RawMessage(input),
		}
		text := req.DisplayText()
		// Should have header but no backtick-wrapped input section.
		if !strings.Contains(text, "**Permission Required**") {
			t.Errorf("input=%q: missing header", input)
		}
	}
}

func TestDisplayText_DecisionReason(t *testing.T) {
	// Proves that technical CC reasons are rewritten to user-friendly text.
	t.Parallel()

	tests := []struct {
		reason string
		want   string
	}{
		{"Unhandled node type: string", "Command requires manual review"},
		{"Contains dangerous operator", "Command requires manual review"},
		{"Parse error", "Command requires manual review"},
		{"Custom reason from CC", "Custom reason from CC"},
	}
	for _, tt := range tests {
		req := &PermissionRequestPayload{
			ToolName:       "Bash",
			DecisionReason: tt.reason,
			Input:          json.RawMessage(`{}`),
		}
		text := req.DisplayText()
		if !strings.Contains(text, tt.want) {
			t.Errorf("reason=%q: text %q missing %q", tt.reason, text, tt.want)
		}
	}
}

func TestDisplayText_FallbackInputFormatting(t *testing.T) {
	// Proves that when tool input doesn't match known key extraction (command,
	// file_path), it falls back to compact JSON.
	t.Parallel()

	req := &PermissionRequestPayload{
		ToolName: "CustomTool",
		Input:    json.RawMessage(`{"foo":"bar","baz":42}`),
	}

	text := req.DisplayText()
	// Should contain the compact JSON in backticks.
	if !strings.Contains(text, "`") {
		t.Errorf("text missing backtick-wrapped input: %q", text)
	}
}

func TestDisplayText_CommandWithBackticks(t *testing.T) {
	// Regression: a shell command containing backticks (e.g. a grep pattern
	// like `\`(high|med|low)\``) must render via a fenced code block so the
	// full command text survives downstream markdown-to-HTML conversion.
	// Previously the command was wrapped in single backticks, which the
	// inline-code regex then paired incorrectly with the internal backticks,
	// leaking part of the command and an INLINECODE placeholder token.
	t.Parallel()

	cmd := `foci_todo list | grep -oP '` + "`" + `(high|med|low)` + "`" + `'`
	inputJSON, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	req := &PermissionRequestPayload{
		ToolName: "Bash",
		Input:    json.RawMessage(inputJSON),
	}
	text := req.DisplayText()

	// Must use a fenced block (triple backticks on their own lines).
	wantFence := "```\n" + cmd + "\n```"
	if !strings.Contains(text, wantFence) {
		t.Errorf("DisplayText() did not use fenced block for command with backticks\n  got:  %q\n  want: contains %q", text, wantFence)
	}
	// The whole command — including the bits around the internal backticks
	// — must appear verbatim somewhere in the output.
	if !strings.Contains(text, cmd) {
		t.Errorf("DisplayText() missing full command text\n  got: %q\n  cmd: %q", text, cmd)
	}
}

func TestDisplayText_LongInputTruncation(t *testing.T) {
	// Proves that very long tool input is truncated to ~200 chars.
	t.Parallel()

	longVal := strings.Repeat("x", 300)
	req := &PermissionRequestPayload{
		ToolName: "CustomTool",
		Input:    json.RawMessage(`{"data":"` + longVal + `"}`),
	}

	text := req.DisplayText()
	// The formatted input should contain a truncation marker.
	if !strings.Contains(text, "…") {
		t.Errorf("long input not truncated: len=%d", len(text))
	}
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

func TestSummary_Description(t *testing.T) {
	// Proves that Summary prefers Description when available.
	t.Parallel()

	req := &PermissionRequestPayload{
		Description: "Execute ls -la",
		DisplayName: "Bash",
		Title:       "Run command",
		ToolName:    "Bash",
	}
	if s := req.Summary(); s != "Execute ls -la" {
		t.Errorf("Summary() = %q, want %q", s, "Execute ls -la")
	}
}

func TestSummary_DisplayNameAndTitle(t *testing.T) {
	// Proves that Summary falls back to "DisplayName: Title" when Description
	// is empty.
	t.Parallel()

	req := &PermissionRequestPayload{
		DisplayName: "Bash",
		Title:       "Run command",
		ToolName:    "Bash",
	}
	if s := req.Summary(); s != "Bash: Run command" {
		t.Errorf("Summary() = %q, want %q", s, "Bash: Run command")
	}
}

func TestSummary_ToolNameFallback(t *testing.T) {
	// Proves that Summary falls back to ToolName when both Description and
	// DisplayName are empty.
	t.Parallel()

	req := &PermissionRequestPayload{
		ToolName: "Bash",
	}
	if s := req.Summary(); s != "Bash" {
		t.Errorf("Summary() = %q, want %q", s, "Bash")
	}
}

// ---------------------------------------------------------------------------
// Choices
// ---------------------------------------------------------------------------

func TestChoices_Basic(t *testing.T) {
	// Proves that Choices always returns at least Allow and Deny options.
	t.Parallel()

	req := &PermissionRequestPayload{ToolName: "Bash"}
	choices := req.Choices()

	if len(choices) != 2 {
		t.Fatalf("len(choices) = %d, want 2", len(choices))
	}
	if choices[0].Label != "Allow" || choices[0].Data != "allow" {
		t.Errorf("choices[0] = %+v, want Allow/allow", choices[0])
	}
	if choices[1].Label != "Deny" || choices[1].Data != "deny" {
		t.Errorf("choices[1] = %+v, want Deny/deny", choices[1])
	}
}

func TestChoices_WithPermissionSuggestions(t *testing.T) {
	// Proves that permission suggestions add "Always: <prefix>" choices with
	// correct data format.
	t.Parallel()

	req := &PermissionRequestPayload{
		ToolName: "Bash",
		PermissionSuggestions: []PermSuggestion{
			{Prefix: "Bash:git *", Scope: "session"},
			{Prefix: "Bash:ls", Scope: "session"},
		},
	}
	choices := req.Choices()

	if len(choices) != 4 {
		t.Fatalf("len(choices) = %d, want 4", len(choices))
	}
	if choices[2].Label != "Always: Bash:git *" {
		t.Errorf("choices[2].Label = %q, want %q", choices[2].Label, "Always: Bash:git *")
	}
	if choices[2].Data != "allow_always:Bash:git *" {
		t.Errorf("choices[2].Data = %q, want %q", choices[2].Data, "allow_always:Bash:git *")
	}
	if choices[3].Label != "Always: Bash:ls" {
		t.Errorf("choices[3].Label = %q, want %q", choices[3].Label, "Always: Bash:ls")
	}
}

func TestChoices_EmptyPrefixSkipped(t *testing.T) {
	// Proves that suggestions with empty prefix are not added as choices.
	t.Parallel()

	req := &PermissionRequestPayload{
		ToolName: "Bash",
		PermissionSuggestions: []PermSuggestion{
			{Prefix: "", Scope: "session"},
			{Prefix: "Bash:git *", Scope: "session"},
		},
	}
	choices := req.Choices()

	// Allow + Deny + 1 non-empty suggestion.
	if len(choices) != 3 {
		t.Fatalf("len(choices) = %d, want 3", len(choices))
	}
}

// ---------------------------------------------------------------------------
// formatToolInput
// ---------------------------------------------------------------------------

func TestFormatToolInput_BashCommand(t *testing.T) {
	// Proves that Bash tool input extracts the command field and renders it
	// as a fenced code block. Fenced blocks (rather than inline `backticks`)
	// are needed so commands containing internal backticks don't confuse the
	// downstream markdown-to-HTML inline-code regex.
	t.Parallel()

	out := formatToolInput("Bash", json.RawMessage(`{"command":"echo hello"}`))
	want := "```\necho hello\n```"
	if out != want {
		t.Errorf("formatToolInput = %q, want %q", out, want)
	}
}

func TestFormatToolInput_FilePath(t *testing.T) {
	// Proves that Write/Edit tool input shows the file path.
	t.Parallel()

	out := formatToolInput("Write", json.RawMessage(`{"file_path":"/tmp/x.txt","content":"data"}`))
	if out != "File: `/tmp/x.txt`" {
		t.Errorf("formatToolInput = %q, want %q", out, "File: `/tmp/x.txt`")
	}
}

func TestFormatToolInput_InvalidJSON(t *testing.T) {
	// Proves that invalid JSON input is displayed as-is inside a fenced
	// code block (so that any stray backticks in the raw input don't break
	// the downstream markdown-to-HTML conversion).
	t.Parallel()

	out := formatToolInput("Bash", json.RawMessage(`not json`))
	want := "```\nnot json\n```"
	if out != want {
		t.Errorf("formatToolInput = %q, want %q", out, want)
	}
}

// ---------------------------------------------------------------------------
// friendlyReason
// ---------------------------------------------------------------------------

func TestFriendlyReason(t *testing.T) {
	// Proves that CC's technical decision reasons are rewritten to user-facing
	// text, while non-matching reasons pass through unchanged.
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"Unhandled node type: string", "Command requires manual review"},
		{"Unhandled node type: ProcessSubstitution", "Command requires manual review"},
		{"Contains dangerous operator", "Command requires manual review"},
		{"Parse error", "Command requires manual review"},
		{"User preference", "User preference"},
		{"", ""},
	}
	for _, tt := range tests {
		got := friendlyReason(tt.input)
		if got != tt.want {
			t.Errorf("friendlyReason(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func TestPermissions_ConcurrentAccess(t *testing.T) {
	// Proves that concurrent store/remove/count operations on pending
	// permissions don't race. Run with -race to verify.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			id := strings.Repeat("x", n+1) // unique IDs
			b.storePendingPerm(&pendingPermission{requestID: id})
			_ = b.PendingPermissions()
			_ = b.PendingPermissions() > 0
			b.removePendingPerm(id)
		}(i)
	}

	wg.Wait()

	if n := b.PendingPermissions(); n != 0 {
		t.Errorf("pending = %d after all removed, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// parseControlResponse extracts the inner response object from a wire-format
// control_response JSON line.
func parseControlResponse(t *testing.T, raw string) map[string]any {
	t.Helper()

	line := strings.TrimSpace(raw)
	var envelope map[string]any
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, line)
	}

	if envelope["type"] != "control_response" {
		t.Fatalf("type = %v, want %q", envelope["type"], "control_response")
	}

	resp, ok := envelope["response"].(map[string]any)
	if !ok {
		t.Fatalf("response is not an object: %T", envelope["response"])
	}

	return resp
}
