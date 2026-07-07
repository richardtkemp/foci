//go:build ignore
// 5 edge-case tests preserved from ccstream era as spec for the opencode port
// (see #159). The other 15 disabled tests in the original file were removed:
// the question tool is implemented on opencode (permissions.go:319/345/367) and
// those happy paths are covered live by permissions_test.go +
// perm_asked_test.go. These 5 cover edge cases not exercised by any live test.
// The bodies still reference ccstream-only symbols (NewWriter, nopWriteCloser,
// PermissionRequest, handleUserQuestion) and must be rewritten against the
// opencode HTTP-POST path before un-ignoring.
package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// End-to-end response flow — testBackend helper used by the kept tests below.
// ---------------------------------------------------------------------------

// testBackend creates a minimal Backend suitable for question tests.
// It captures control responses written to the Writer.
// NB: references deleted ccstream symbols (NewWriter, nopWriteCloser) — kept
// only as spec for the port; the opencode version should use the
// recordingHandler / newPermTestBackend pattern from permissions_test.go.
func testBackend(t *testing.T) (*Backend, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(nopWriteCloser{&buf})
	b := &Backend{
		writer:       w,
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	return b, &buf
}

// ---------------------------------------------------------------------------
// Edge cases NOT covered by any live opencode test — port per #159.
// ---------------------------------------------------------------------------

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_SequentialMultiQuestion(t *testing.T) {
// 	// Verifies sequential multi-question flow: answer Q1 → prompt Q2 →
// 	// answer Q2 → PermissionAllow with both answers.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
//
// 	var promptCalls int
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		promptCalls++
// 	}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-multi",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-2",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q1?","header":"H1","options":[{"label":"A1"}]},{"question":"Q2?","header":"H2","options":[{"label":"A2"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	if promptCalls != 1 {
// 		t.Fatalf("after handleUserQuestion: prompt called %d times, want 1", promptCalls)
// 	}
//
// 	// Answer Q1.
// 	if err := b.RespondToQuestion("req-multi", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q1: %v", err)
// 	}
//
// 	// Should have prompted Q2 (no control response yet).
// 	if promptCalls != 2 {
// 		t.Fatalf("after Q1 answer: prompt called %d times, want 2", promptCalls)
// 	}
// 	if buf.Len() != 0 {
// 		t.Error("should not have sent control response after Q1")
// 	}
//
// 	// Answer Q2.
// 	if err := b.RespondToQuestion("req-multi", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q2: %v", err)
// 	}
//
// 	// Now control response should be sent with both answers.
// 	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
// 	if len(lines) != 1 {
// 		t.Fatalf("got %d response lines, want 1", len(lines))
// 	}
//
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				UpdatedInput json.RawMessage `json:"updatedInput"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	var updated map[string]json.RawMessage
// 	if err := json.Unmarshal(resp.Response.Response.UpdatedInput, &updated); err != nil {
// 		t.Fatalf("unmarshal updatedInput: %v", err)
// 	}
// 	var answers map[string]string
// 	if err := json.Unmarshal(updated["answers"], &answers); err != nil {
// 		t.Fatalf("unmarshal answers: %v", err)
// 	}
// 	if answers["Q1?"] != "A1" {
// 		t.Errorf("answers[Q1?] = %q, want %q", answers["Q1?"], "A1")
// 	}
// 	if answers["Q2?"] != "A2" {
// 		t.Errorf("answers[Q2?] = %q, want %q", answers["Q2?"], "A2")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestCancelQuestion_MidSequence(t *testing.T) {
// 	// Verifies that cancelling mid-sequence discards accumulated answers.
// 	t.Parallel()
//
// 	b, buf := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-mid",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-5",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q1?","header":"H1","options":[{"label":"A"}]},{"question":"Q2?","header":"H2","options":[{"label":"B"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// Answer Q1.
// 	if err := b.RespondToQuestion("req-mid", "qa:0"); err != nil {
// 		t.Fatalf("RespondToQuestion Q1: %v", err)
// 	}
//
// 	// Cancel before answering Q2.
// 	if err := b.CancelQuestion("req-mid"); err != nil {
// 		t.Fatalf("CancelQuestion: %v", err)
// 	}
//
// 	// Should send deny, not allow with partial answers.
// 	var resp struct {
// 		Response struct {
// 			Response struct {
// 				Behavior string `json:"behavior"`
// 			} `json:"response"`
// 		} `json:"response"`
// 	}
// 	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &resp); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
// 	if resp.Response.Response.Behavior != "deny" {
// 		t.Errorf("behavior = %q, want %q", resp.Response.Response.Behavior, "deny")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_AlreadyAnswered(t *testing.T) {
// 	// Verifies graceful error when responding to a question that was already
// 	// resolved (e.g., button click after text answer consumed the question).
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-race",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-7",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	// First answer succeeds.
// 	if err := b.RespondToQuestion("req-race", "qa:0"); err != nil {
// 		t.Fatalf("first RespondToQuestion: %v", err)
// 	}
//
// 	// Second answer should fail gracefully.
// 	err := b.RespondToQuestion("req-race", "qa:0")
// 	if err == nil {
// 		t.Error("expected error for already-answered question")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_InvalidOptionIndex(t *testing.T) {
// 	// Verifies error on out-of-range option index.
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-bad-idx",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-8",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"Only"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	err := b.RespondToQuestion("req-bad-idx", "qa:5")
// 	if err == nil {
// 		t.Error("expected error for out-of-range index")
// 	}
// }

// TODO(opencode): rewrite — opencode has a question tool surfaced via permission.updated; see plan section 9.4
// func TestRespondToQuestion_ConcurrentAccess(t *testing.T) {
// 	// Verifies that concurrent RespondToQuestion/HasPendingQuestion calls
// 	// don't race (detected by -race flag).
// 	t.Parallel()
//
// 	b, _ := testBackend(t)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	msg := &PermissionRequest{
// 		Type:      "control_request",
// 		RequestID: "req-conc",
// 		Request: PermissionRequestPayload{
// 			Subtype:   "can_use_tool",
// 			ToolName:  "AskUserQuestion",
// 			ToolUseID: "tu-11",
// 			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"}]}]}`),
// 		},
// 	}
// 	b.handleUserQuestion(msg)
//
// 	var wg sync.WaitGroup
// 	wg.Add(2)
//
// 	go func() {
// 		defer wg.Done()
// 		_ = b.HasPendingQuestion()
// 	}()
// 	go func() {
// 		defer wg.Done()
// 		_ = b.RespondToQuestion("req-conc", "qa:0")
// 	}()
//
// 	wg.Wait()
// }
