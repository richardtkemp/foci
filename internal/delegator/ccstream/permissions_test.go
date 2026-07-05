package ccstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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

func TestRespondToPermission_FiresOnPromptsCleared(t *testing.T) {
	// Proves that the registry's onEmpty hook is called when the last
	// outstanding prompt is resolved, and NOT called when prompts remain.
	t.Parallel()

	var buf bytes.Buffer
	cleared := 0
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { cleared++ })
	b.storePendingPerm(&pendingPermission{requestID: "req-A", toolUseID: "a"})
	b.storePendingPerm(&pendingPermission{requestID: "req-B", toolUseID: "b"})
	b.outstanding.Register("req-A", delegator.OutstandingPermission)
	b.outstanding.Register("req-B", delegator.OutstandingPermission)

	// Resolve first — still one pending.
	if err := b.RespondToPermission("req-A", true, ""); err != nil {
		t.Fatalf("first respond: %v", err)
	}
	if cleared != 0 {
		t.Errorf("cleared called after first resolve, want 0 calls")
	}

	// Resolve second — registry now empty → onPromptsCleared fires.
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
	}

	err := b.RespondToPermissionWithRule("nonexistent", "Bash:ls")
	if err == nil {
		t.Fatal("expected error for unknown request ID")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestRespondToPermissionWithRule_FiresOnPromptsCleared(t *testing.T) {
	// Proves that onPromptsCleared fires when the last outstanding prompt is
	// resolved via RespondToPermissionWithRule.
	t.Parallel()

	var buf bytes.Buffer
	cleared := false
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { cleared = true })
	b.storePendingPerm(&pendingPermission{requestID: "req-rc", toolUseID: "x"})
	b.outstanding.Register("req-rc", delegator.OutstandingPermission)

	if err := b.RespondToPermissionWithRule("req-rc", "Read"); err != nil {
		t.Fatalf("RespondToPermissionWithRule: %v", err)
	}
	if !cleared {
		t.Error("onPromptsCleared not called after last prompt resolved")
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
		outstanding:      delegator.NewOutstandingRegistry(),
		autoApproveRules: parseAutoApproveRules([]string{"Read"}),
		permPromptFn: func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
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
		outstanding:  delegator.NewOutstandingRegistry(),
		permPromptFn: func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
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
		outstanding:  delegator.NewOutstandingRegistry(),
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

func TestHandlePermissionRequest_RegistersOutstanding(t *testing.T) {
	// Proves that handleToolRequest registers the prompt in the
	// delegator.OutstandingRegistry so the cancel-listener and onEmpty hooks find it.
	// Replaces the legacy onPermPending callback (which only existed in tests).
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
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

	if !b.outstanding.Has("req-pend") {
		t.Error("outstanding registry should contain req-pend after handleToolRequest")
	}
	if b.outstanding.Len() != 1 {
		t.Errorf("registry Len = %d, want 1", b.outstanding.Len())
	}
}

// ---------------------------------------------------------------------------
// handleControlCancel
// ---------------------------------------------------------------------------

func TestHandleControlCancel_RemovesPending(t *testing.T) {
	// Proves that handleControlCancel removes the pending permission and fires
	// onPromptsCleared when no prompts remain.
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { cleared = true })
	b.storePendingPerm(&pendingPermission{requestID: "req-cancel"})
	b.outstanding.Register("req-cancel", delegator.OutstandingPermission)

	b.handleControlCancel("req-cancel")

	if b.PendingPermissions() != 0 {
		t.Errorf("pending = %d, want 0", b.PendingPermissions())
	}
	if !cleared {
		t.Error("onPromptsCleared not called after cancel cleared last prompt")
	}
}

// TestHandleControlCancel_UnknownID proves that cancelling a non-existent
// request is a no-op — the registry doesn't fire its onEmpty hook for a
// no-op cancel. This is a deliberate behavior change from pre-Phase-2,
// where onPermCleared could fire even when no actual prompt was removed
// (the legacy logic checked `len(pendingPerms)==0` regardless of found).
func TestHandleControlCancel_UnknownID(t *testing.T) {
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { cleared = true })

	b.handleControlCancel("nonexistent")

	if cleared {
		t.Error("onPromptsCleared should not fire on no-op cancel")
	}
}

func TestHandleControlCancel_StillPending(t *testing.T) {
	// Proves that onPromptsCleared is NOT called when other prompts remain
	// after cancellation.
	t.Parallel()

	cleared := false
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { cleared = true })
	b.storePendingPerm(&pendingPermission{requestID: "req-stay"})
	b.storePendingPerm(&pendingPermission{requestID: "req-go"})
	b.outstanding.Register("req-stay", delegator.OutstandingPermission)
	b.outstanding.Register("req-go", delegator.OutstandingPermission)

	b.handleControlCancel("req-go")

	if cleared {
		t.Error("onPromptsCleared should not fire while prompts remain")
	}
	if b.PendingPermissions() != 1 {
		t.Errorf("pending = %d, want 1", b.PendingPermissions())
	}
}

// TestHandleControlCancel_FiresRegisteredCancelListener proves that the
// per-prompt cancel listener (registered by the platform layer when sending
// the interactive UI) fires with the cancellation reason. Replaces the
// legacy permCancelFn(requestID, toolName, reason) signature — the toolName
// is now captured by the closure that registers the listener.
func TestHandleControlCancel_FiresRegisteredCancelListener(t *testing.T) {
	t.Parallel()

	var (
		gotReason string
		called    int
	)
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-7", toolName: "Bash"})
	b.outstanding.Register("req-7", delegator.OutstandingPermission)
	b.RegisterPromptCancelListener("req-7", func(reason string) {
		called++
		gotReason = reason
	})

	b.handleControlCancel("req-7")

	if called != 1 {
		t.Fatalf("cancel listener called %d times, want 1", called)
	}
	if !strings.Contains(gotReason, "follow-up") {
		t.Errorf("reason = %q, want it to mention 'follow-up'", gotReason)
	}
}

// TestHandleControlCancel_NoFireForUnknownID proves that no listener fires
// for an unknown reqID — Cancel is a no-op when the prompt was never
// registered, and AddCancelListener silently drops listeners for unknown IDs.
func TestHandleControlCancel_NoFireForUnknownID(t *testing.T) {
	t.Parallel()

	called := 0
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	// Listener registration for an unknown ID is itself a no-op.
	b.RegisterPromptCancelListener("ghost", func(string) { called++ })

	b.handleControlCancel("ghost")

	if called != 0 {
		t.Errorf("cancel listener called %d times for unknown id, want 0", called)
	}
}

// TestHandleControlCancel_ListenersBeforeOnEmpty proves that per-prompt
// cancel listeners fire before the registry-wide onEmpty hook. Order
// matters: the platform layer wants to update the per-prompt UI before any
// session-wide "all clear" actions trigger.
func TestHandleControlCancel_ListenersBeforeOnEmpty(t *testing.T) {
	t.Parallel()

	var order []string
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { order = append(order, "cleared") })
	b.storePendingPerm(&pendingPermission{requestID: "req-only"})
	b.outstanding.Register("req-only", delegator.OutstandingPermission)
	b.RegisterPromptCancelListener("req-only", func(string) {
		order = append(order, "cancel")
	})

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
		outstanding:  delegator.NewOutstandingRegistry(),
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

// TestPlanAttachmentPath covers the ExitPlanMode plan-file resolver: it returns
// the on-disk path CC wrote (input.planFilePath) when readable, and "" in every
// degraded case so the caller falls back to the generic prompt rendering.
func TestPlanAttachmentPath(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(planFile, []byte("# Plan\n\nstep one"), 0o600); err != nil {
		t.Fatalf("write plan file: %v", err)
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"existing file", `{"plan":"# Plan","planFilePath":"` + planFile + `"}`, planFile},
		{"missing file", `{"plan":"# Plan","planFilePath":"` + filepath.Join(dir, "nope.md") + `"}`, ""},
		{"no planFilePath", `{"plan":"# Plan"}`, ""},
		{"empty planFilePath", `{"plan":"# Plan","planFilePath":""}`, ""},
		{"path is a directory", `{"planFilePath":"` + dir + `"}`, ""},
		{"malformed json", `not json`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := planAttachmentPath(json.RawMessage(tc.input)); got != tc.want {
				t.Errorf("planAttachmentPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHandlePermissionRequest_ExitPlanMode_AttachesPlanFile proves that an
// ExitPlanMode request is forwarded to permPromptFn with the plan file as an
// attachment and a clean caption (not the truncated-JSON generic rendering),
// while keeping the standard Allow/Deny choices.
func TestHandlePermissionRequest_ExitPlanMode_AttachesPlanFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan-trivial.md")
	if err := os.WriteFile(planFile, []byte("# Plan\n\ndo the thing"), 0o600); err != nil {
		t.Fatalf("write plan file: %v", err)
	}

	var buf bytes.Buffer
	var gotText, gotSummary, gotAttachment string
	var gotChoices []delegator.PromptChoice
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
		permPromptFn: func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
			gotText = text
			gotSummary = summary
			gotAttachment = attachmentPath
			gotChoices = choices
		},
	}

	input := json.RawMessage(`{"plan":"# Plan\n\ndo the thing","planFilePath":"` + planFile + `"}`)
	msg := &PermissionRequest{
		RequestID: "req-plan",
		Request: PermissionRequestPayload{
			ToolName:  "ExitPlanMode",
			ToolUseID: "toolu_PLAN",
			Input:     input,
		},
	}

	b.handleToolRequest(msg)

	if gotAttachment != planFile {
		t.Errorf("attachmentPath = %q, want %q", gotAttachment, planFile)
	}
	if !strings.Contains(gotText, "Plan ready") {
		t.Errorf("text = %q, want a clean caption containing %q (not truncated JSON)", gotText, "Plan ready")
	}
	if strings.Contains(gotText, "planFilePath") {
		t.Errorf("text leaked raw JSON input: %q", gotText)
	}
	if gotSummary != "Plan" {
		t.Errorf("summary = %q, want %q", gotSummary, "Plan")
	}
	// Choices unchanged: plain binary Allow/Deny over the stdio protocol.
	if len(gotChoices) != 2 || gotChoices[0].Data != "allow" || gotChoices[1].Data != "deny" {
		t.Errorf("choices = %+v, want [Allow Deny]", gotChoices)
	}
	if b.PendingPermissions() != 1 {
		t.Errorf("pending = %d, want 1", b.PendingPermissions())
	}
}

// TestHandlePermissionRequest_ExitPlanMode_MissingFileFallsBack proves that when
// the plan file is absent, no attachment is set and the prompt falls back to the
// generic rendering (graceful degradation rather than an empty/broken prompt).
func TestHandlePermissionRequest_ExitPlanMode_MissingFileFallsBack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var gotText, gotAttachment string
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
		permPromptFn: func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
			gotText = text
			gotAttachment = attachmentPath
		},
	}

	input := json.RawMessage(`{"plan":"# Plan","planFilePath":"/no/such/plan.md"}`)
	msg := &PermissionRequest{
		RequestID: "req-plan-missing",
		Request: PermissionRequestPayload{
			ToolName:  "ExitPlanMode",
			ToolUseID: "toolu_PLAN2",
			Input:     input,
		},
	}

	b.handleToolRequest(msg)

	if gotAttachment != "" {
		t.Errorf("attachmentPath = %q, want empty (missing file)", gotAttachment)
	}
	// Falls back to generic DisplayText, which includes the tool name.
	if !strings.Contains(gotText, "ExitPlanMode") {
		t.Errorf("fallback text = %q, want generic rendering mentioning the tool", gotText)
	}
}

// ---------------------------------------------------------------------------
// Plan-cancel-by-message (HasPendingPlanPermission / CancelPlanWithFeedback)
// ---------------------------------------------------------------------------

// TestHasPendingPlanPermission proves it returns the requestID of a pending
// ExitPlanMode permission and "" for non-plan permissions or none.
func TestHasPendingPlanPermission(t *testing.T) {
	t.Parallel()

	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	if got := b.HasPendingPlanPermission(); got != "" {
		t.Errorf("empty backend: got %q, want \"\"", got)
	}
	// A non-plan permission must not match.
	b.storePendingPerm(&pendingPermission{requestID: "req-bash", toolName: "Bash"})
	if got := b.HasPendingPlanPermission(); got != "" {
		t.Errorf("only a Bash perm pending: got %q, want \"\"", got)
	}
	// An ExitPlanMode permission matches.
	b.storePendingPerm(&pendingPermission{requestID: "req-plan", toolName: "ExitPlanMode"})
	if got := b.HasPendingPlanPermission(); got != "req-plan" {
		t.Errorf("plan perm pending: got %q, want %q", got, "req-plan")
	}
	b.removePendingPerm("req-plan")
	if got := b.HasPendingPlanPermission(); got != "" {
		t.Errorf("after removing plan perm: got %q, want \"\"", got)
	}
}

// TestCancelPlanWithFeedback proves it denies the plan with the user's feedback
// as the message, clears the pending permission, and fires the prompt's cancel
// listener (which edits the Allow/Deny buttons away).
func TestCancelPlanWithFeedback(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.storePendingPerm(&pendingPermission{requestID: "req-plan", toolUseID: "toolu_PLAN", toolName: "ExitPlanMode"})
	b.outstanding.Register("req-plan", delegator.OutstandingPermission)
	cancelled := 0
	var cancelReason string
	b.RegisterPromptCancelListener("req-plan", func(reason string) {
		cancelled++
		cancelReason = reason
	})

	if err := b.CancelPlanWithFeedback("req-plan", "use postgres not sqlite"); err != nil {
		t.Fatalf("CancelPlanWithFeedback: %v", err)
	}

	resp := parseControlResponse(t, buf.String())
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "deny" {
		t.Errorf("behavior = %v, want %q", inner["behavior"], "deny")
	}
	if inner["message"] != "use postgres not sqlite" {
		t.Errorf("message = %v, want the user's feedback", inner["message"])
	}
	if inner["toolUseID"] != "toolu_PLAN" {
		t.Errorf("toolUseID = %v, want %q", inner["toolUseID"], "toolu_PLAN")
	}
	if b.PendingPermissions() != 0 {
		t.Errorf("pending = %d, want 0", b.PendingPermissions())
	}
	if cancelled != 1 {
		t.Errorf("cancel listener fired %d times, want 1 (buttons edited away)", cancelled)
	}
	if !strings.Contains(cancelReason, "follow-up") {
		t.Errorf("cancel reason = %q, want mention of 'follow-up'", cancelReason)
	}
	if b.outstanding.Has("req-plan") {
		t.Error("outstanding entry should be cleared after cancel")
	}
}

// TestCancelPlanWithFeedback_UnknownID proves an unknown request id errors and
// writes nothing on the wire.
func TestCancelPlanWithFeedback_UnknownID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:       NewWriter(nopWriteCloser{&buf}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	if err := b.CancelPlanWithFeedback("nope", "feedback"); err == nil {
		t.Fatal("expected error for unknown request id")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no wire output, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// formatEditDiff & Choices (show/hide diff toggle)
// ---------------------------------------------------------------------------

func TestFormatEditDiff_Edit(t *testing.T) {
	in := json.RawMessage(`{"file_path":"/a.go","old_string":"foo\nbar","new_string":"baz"}`)
	got := formatEditDiff("Edit", in)
	for _, want := range []string{"🔴 foo", "🔴 bar", "🟢 baz", "```"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestFormatEditDiff_MultiEdit(t *testing.T) {
	in := json.RawMessage(`{"file_path":"/a.go","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d"}]}`)
	got := formatEditDiff("MultiEdit", in)
	for _, want := range []string{"edit 1", "edit 2", "🔴 a", "🟢 b", "🔴 c", "🟢 d"} {
		if !strings.Contains(got, want) {
			t.Errorf("multiedit diff missing %q:\n%s", want, got)
		}
	}
}

func TestFormatEditDiff_NonEditTool(t *testing.T) {
	if got := formatEditDiff("Bash", json.RawMessage(`{"command":"ls"}`)); got != "" {
		t.Errorf("non-edit tool got %q, want empty", got)
	}
}

func TestFormatEditDiff_NeutralizesInnerFence(t *testing.T) {
	in := json.RawMessage(`{"old_string":"x","new_string":"pre\n` + "```" + `go\ncode"}`)
	got := formatEditDiff("Edit", in)
	if strings.Contains(got, "\n```go") {
		t.Errorf("inner triple-backtick not neutralized:\n%q", got)
	}
	if strings.Count(got, "```") != 2 {
		t.Errorf("want exactly 2 fence markers (open+close), got %d:\n%q", strings.Count(got, "```"), got)
	}
}

func TestFormatEditDiff_Truncates(t *testing.T) {
	big := strings.Repeat("x", maxDiffChars+500)
	in := json.RawMessage(`{"old_string":"","new_string":"` + big + `"}`)
	got := formatEditDiff("Edit", in)
	if !strings.Contains(got, "… (truncated)") {
		t.Error("oversized diff should be truncated")
	}
}

func TestChoices_EditAddsToggle(t *testing.T) {
	req := &PermissionRequestPayload{
		ToolName: "Edit",
		Input:    json.RawMessage(`{"file_path":"/a.go","old_string":"foo","new_string":"bar"}`),
	}
	choices := req.Choices()
	var toggle *delegator.PromptChoice
	for i := range choices {
		if choices[i].Toggle != nil {
			toggle = &choices[i]
		}
	}
	if toggle == nil {
		t.Fatal("Edit request should carry a toggle choice")
	}
	if toggle.Toggle.ShowLabel != "Show diff" || toggle.Toggle.HideLabel != "Hide diff" {
		t.Errorf("toggle labels = %q/%q", toggle.Toggle.ShowLabel, toggle.Toggle.HideLabel)
	}
	if !strings.Contains(toggle.Toggle.ExtraBody, "🟢 bar") {
		t.Errorf("toggle body missing diff: %q", toggle.Toggle.ExtraBody)
	}
}

func TestChoices_BashNoToggle(t *testing.T) {
	req := &PermissionRequestPayload{ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}
	for _, c := range req.Choices() {
		if c.Toggle != nil {
			t.Error("Bash request should not carry a toggle choice")
		}
	}
}
