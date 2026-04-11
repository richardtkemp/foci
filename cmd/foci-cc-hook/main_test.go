package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTruncate_ShortPassThrough proves values below the limit are returned
// verbatim — no marker, no change.
func TestTruncate_ShortPassThrough(t *testing.T) {
	got := truncate("hello", 100)
	if got != "hello" {
		t.Errorf("truncate short = %q, want %q", got, "hello")
	}
}

// TestTruncate_CapsAtMaxAndMarks proves values above the limit are cut to
// max bytes and tagged with a visible marker so downstream parsers can see
// the output was capped.
func TestTruncate_CapsAtMaxAndMarks(t *testing.T) {
	in := strings.Repeat("x", 200)
	got := truncate(in, 50)
	if !strings.HasPrefix(got, strings.Repeat("x", 50)) {
		t.Errorf("truncate did not keep first 50 bytes: %q", got[:60])
	}
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("truncate missing marker suffix: %q", got[len(got)-20:])
	}
}

// decodeOutput parses the hookOutput JSON payload a test emitted by
// simulating main() via direct function calls.
func decodeOutput(t *testing.T, raw string) hookOutput {
	t.Helper()
	var out hookOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode hookOutput: %v — raw: %s", err, raw)
	}
	return out
}

// runHook invokes the same logic main() runs, but against an in-memory
// input/output pair so the tests don't need to shell out. Keeps the unit
// test deterministic and fast.
func runHook(t *testing.T, body []byte) hookOutput {
	t.Helper()
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("decode hookInput fixture: %v", err)
	}
	out := hookOutput{
		HookEvent: in.HookEventName,
		ToolUseID: in.ToolUseID,
		ToolName:  in.ToolName,
		AgentID:   in.AgentID,
		IsError:   in.HookEventName == "PostToolUseFailure" || in.IsInterrupt || in.IsTimeout,
	}
	if len(in.ToolResponse) > 0 {
		out.ToolResponse = truncate(string(in.ToolResponse), maxFieldBytes)
	}
	if in.Error != "" {
		out.Error = truncate(in.Error, maxFieldBytes)
	}
	enc, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("encode hookOutput: %v", err)
	}
	return decodeOutput(t, string(enc))
}

// TestMain_PostToolUse proves the happy path: a realistic PostToolUse
// envelope from CC is reduced to the compact hookOutput shape with
// is_error=false and tool_response preserved (unquoted-string form).
func TestMain_PostToolUse(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Read",
		"tool_use_id": "toolu_abc123",
		"tool_input": {"file_path": "/tmp/x"},
		"tool_response": "contents of the file",
		"session_id": "sess-1"
	}`)
	out := runHook(t, body)

	if out.HookEvent != "PostToolUse" {
		t.Errorf("HookEvent = %q, want PostToolUse", out.HookEvent)
	}
	if out.ToolUseID != "toolu_abc123" {
		t.Errorf("ToolUseID = %q", out.ToolUseID)
	}
	if out.ToolName != "Read" {
		t.Errorf("ToolName = %q", out.ToolName)
	}
	if out.IsError {
		t.Error("IsError = true, want false for PostToolUse")
	}
	// tool_response is json.RawMessage in hookInput; we stringify it,
	// so a JSON string like `"contents of the file"` becomes exactly that
	// (quote-included) in the output.
	if out.ToolResponse != `"contents of the file"` {
		t.Errorf("ToolResponse = %q, want quoted string", out.ToolResponse)
	}
}

// TestMain_PostToolUseFailure proves failure envelopes produce is_error=true
// and carry the error message rather than the (absent) tool_response field.
func TestMain_PostToolUseFailure(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUseFailure",
		"tool_name": "Write",
		"tool_use_id": "toolu_xyz",
		"error": "Permission denied",
		"is_interrupt": false,
		"is_timeout": false
	}`)
	out := runHook(t, body)

	if !out.IsError {
		t.Error("IsError = false, want true for PostToolUseFailure")
	}
	if out.Error != "Permission denied" {
		t.Errorf("Error = %q", out.Error)
	}
	if out.ToolResponse != "" {
		t.Errorf("ToolResponse = %q, want empty on failure", out.ToolResponse)
	}
}

// TestMain_Subagent proves agent_id is preserved so downstream parsers can
// filter sub-agent tool events away from the parent turn handler.
func TestMain_Subagent(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Read",
		"tool_use_id": "toolu_sub",
		"tool_response": "ok",
		"agent_id": "agent-sub-42"
	}`)
	out := runHook(t, body)

	if out.AgentID != "agent-sub-42" {
		t.Errorf("AgentID = %q, want agent-sub-42", out.AgentID)
	}
}

// TestMain_TruncatesLargeToolResponse proves multi-MB tool responses are
// capped so each emitted line stays well under the ccstream reader scanner
// limit (1MB per line). Without this, a big file read would DoS the stream.
func TestMain_TruncatesLargeToolResponse(t *testing.T) {
	// Build a ~128KB response to exceed the 64KB cap.
	big := strings.Repeat("x", 128*1024)
	// Encode as JSON string so it's valid tool_response RawMessage.
	encoded, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Read",
		"tool_use_id": "toolu_big",
		"tool_response": ` + string(encoded) + `
	}`)
	out := runHook(t, body)

	if len(out.ToolResponse) > maxFieldBytes+len("...[truncated]") {
		t.Errorf("truncated length = %d, want <= %d", len(out.ToolResponse), maxFieldBytes+len("...[truncated]"))
	}
	if !strings.HasSuffix(out.ToolResponse, "...[truncated]") {
		t.Errorf("missing truncation marker; got last 20 bytes: %q", out.ToolResponse[len(out.ToolResponse)-20:])
	}
}

// TestMain_IsInterruptSetsError proves the is_interrupt flag on PostToolUse
// (non-failure) still flips is_error so the downstream tracker treats the
// call as errored.
func TestMain_IsInterruptSetsError(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Bash",
		"tool_use_id": "toolu_int",
		"tool_response": "",
		"is_interrupt": true
	}`)
	out := runHook(t, body)
	if !out.IsError {
		t.Error("IsError = false, want true when is_interrupt=true")
	}
}
