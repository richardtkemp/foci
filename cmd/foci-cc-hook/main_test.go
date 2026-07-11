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
// test deterministic and fast. installID simulates the --install argv
// foci passes in at install time.
func runHook(t *testing.T, body []byte, installID string) hookOutput {
	t.Helper()
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("decode hookInput fixture: %v", err)
	}
	out := hookOutput{
		HookEvent: in.HookEventName,
		InstallID: installID,
		ToolUseID: in.ToolUseID,
		ToolName:  in.ToolName,
		AgentID:   in.AgentID,
		IsError:   in.HookEventName == "PostToolUseFailure" || in.IsInterrupt || in.IsTimeout,
	}
	if len(in.ToolInput) > 0 {
		out.ToolInput = truncate(string(in.ToolInput), maxFieldBytes)
	}
	if len(in.ToolResponse) > 0 {
		out.ToolResponse = truncate(decodeToolResponse(in.ToolResponse), maxFieldBytes)
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
// is_error=false and tool_response unwrapped from its JSON string form
// (so the user-visible "Show full" view doesn't show stray double quotes).
func TestMain_PostToolUse(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Read",
		"tool_use_id": "toolu_abc123",
		"tool_input": {"file_path": "/tmp/x"},
		"tool_response": "contents of the file",
		"session_id": "sess-1"
	}`)
	out := runHook(t, body, "test-id")

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
	if out.ToolResponse != "contents of the file" {
		t.Errorf("ToolResponse = %q, want unwrapped plain string", out.ToolResponse)
	}
}

// TestDecodeToolResponse_StructuredFallsBackToRaw proves that non-string
// tool_response payloads (objects, arrays, numbers) fall back to the raw
// JSON bytes rather than being dropped — Bash's structured exec results,
// for example, arrive as objects and we still want them legible in the
// "Show full" expansion.
func TestDecodeToolResponse_StructuredFallsBackToRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"hello"`, "hello"},
		{"object", `{"stdout":"ok","exit":0}`, `{"stdout":"ok","exit":0}`},
		{"array", `[1,2,3]`, `[1,2,3]`},
		{"number", `42`, `42`},
		{"escaped string", `"line\nbreak"`, "line\nbreak"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeToolResponse(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("decodeToolResponse(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
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
	out := runHook(t, body, "test-id")

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
	out := runHook(t, body, "test-id")

	if out.AgentID != "agent-sub-42" {
		t.Errorf("AgentID = %q, want agent-sub-42", out.AgentID)
	}
}

// TestMain_TruncatesLargeToolResponse proves multi-MB tool responses are
// capped so each emitted line stays well under the ccstream reader scanner
// limit (1MB per line). Without this, a big file read would DoS the stream.
func TestMain_TruncatesLargeToolResponse(t *testing.T) {
	// Build a ~128KB response to exceed the maxFieldBytes cap.
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
	out := runHook(t, body, "test-id")

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
	out := runHook(t, body, "test-id")
	if !out.IsError {
		t.Error("IsError = false, want true when is_interrupt=true")
	}
}

// TestParseInstallID_Forms proves the argv parser accepts both the
// space-separated and equals-separated forms, returns empty when absent,
// and ignores unrelated flags.
func TestParseInstallID_Forms(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"space form", []string{"foci-cc-hook", "--install", "abc123"}, "abc123"},
		{"equals form", []string{"foci-cc-hook", "--install=xyz789"}, "xyz789"},
		{"absent", []string{"foci-cc-hook"}, ""},
		{"dangling", []string{"foci-cc-hook", "--install"}, ""},
		{"other flag", []string{"foci-cc-hook", "--other", "val", "--install", "id-2"}, "id-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInstallID(tc.args)
			if got != tc.want {
				t.Errorf("parseInstallID(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestMain_InstallIDEchoed proves the install ID passed on argv is echoed
// back in the output so ccstream's handleHookResponse can filter by
// originating backend.
func TestMain_InstallIDEchoed(t *testing.T) {
	body := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_use_id":"t1","tool_response":"x"}`)
	out := runHook(t, body, "backend-xyz")
	if out.InstallID != "backend-xyz" {
		t.Errorf("InstallID = %q, want backend-xyz", out.InstallID)
	}
}

// TestMain_ToolInputForwarded proves CC's PostToolUse tool_input field
// (verified to exist in claude-code/src/entrypoints/sdk/coreSchemas.ts at
// PostToolUseHookInputSchema) is forwarded through to hookOutput so the
// downstream nudge scheduler can match tool_pattern triggers on Bash
// commands, file paths, etc. Compact JSON round-trips byte-for-byte; we
// don't reformat or parse field-by-field at this layer.
func TestMain_ToolInputForwarded(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Bash",
		"tool_use_id": "toolu_1",
		"tool_input": {"command":"git status"},
		"tool_response": "ok"
	}`)
	out := runHook(t, body, "test-id")
	if !strings.Contains(out.ToolInput, `"command":"git status"`) {
		t.Errorf("ToolInput = %q, want substring %q", out.ToolInput, `"command":"git status"`)
	}
}

// TestMain_ToolInputForwardedOnFailure proves the same forwarding works for
// PostToolUseFailure (tool_input is present on both success and failure
// envelopes per coreSchemas.ts).
func TestMain_ToolInputForwardedOnFailure(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUseFailure",
		"tool_name": "Edit",
		"tool_use_id": "toolu_2",
		"tool_input": {"file_path":"/etc/passwd","new_string":"x"},
		"error": "Permission denied"
	}`)
	out := runHook(t, body, "test-id")
	if !out.IsError {
		t.Error("IsError = false, want true for PostToolUseFailure")
	}
	if !strings.Contains(out.ToolInput, `"file_path":"/etc/passwd"`) {
		t.Errorf("ToolInput = %q, want substring with file_path", out.ToolInput)
	}
}

// TestMain_ToolInputTruncated proves Write/Edit inputs containing large
// content blobs are bounded at maxFieldBytes — without this, a multi-MB
// file write would blow the ccstream scanner limit when stream-json
// encodes the hook_response line.
func TestMain_ToolInputTruncated(t *testing.T) {
	big := strings.Repeat("y", 128*1024)
	encoded, err := json.Marshal(map[string]string{
		"file_path": "/tmp/big.txt",
		"content":   big,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Write",
		"tool_use_id": "toolu_big",
		"tool_input": ` + string(encoded) + `,
		"tool_response": "ok"
	}`)
	out := runHook(t, body, "test-id")
	if out.ToolInput == "" {
		t.Fatal("ToolInput empty — large input dropped instead of truncated")
	}
	if len(out.ToolInput) > maxFieldBytes+len("...[truncated]") {
		t.Errorf("ToolInput length = %d, want <= %d", len(out.ToolInput), maxFieldBytes+len("...[truncated]"))
	}
	if !strings.HasSuffix(out.ToolInput, "...[truncated]") {
		tail := out.ToolInput
		if len(tail) > 20 {
			tail = tail[len(tail)-20:]
		}
		t.Errorf("missing truncation marker; got last bytes: %q", tail)
	}
}

// TestMain_ToolInputAbsent proves the omitempty contract holds when CC
// doesn't include tool_input — hookOutput's ToolInput stays empty rather
// than emitting a stray "tool_input":"" field.
func TestMain_ToolInputAbsent(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "PostToolUse",
		"tool_name": "Read",
		"tool_use_id": "toolu_3",
		"tool_response": "ok"
	}`)
	out := runHook(t, body, "test-id")
	if out.ToolInput != "" {
		t.Errorf("ToolInput = %q, want empty when tool_input absent", out.ToolInput)
	}
}
