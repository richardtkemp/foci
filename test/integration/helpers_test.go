//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// waitForStderr polls the harness stderr until it contains substr or
// the timeout expires. Returns true on hit. Used for assertions about
// foci's log surface (escalate lines, sanitized error messages).
func waitForStderr(h *testharness.Harness, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Stderr(), substr) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return strings.Contains(h.Stderr(), substr)
}

// hasBlockType reports whether want appears anywhere in the recorded
// content-block types list. Tiny helper kept here so attachment tests
// don't redefine it.
func hasBlockType(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// hasNonTextBlock reports whether the recorded content-block list
// contains any block that is NOT a plain "text" block. Used by
// attachment tests where the exact non-text type ("image", "document",
// etc.) is implementation-dependent but presence-of-any-non-text is
// the structural contract.
func hasNonTextBlock(types []string) bool {
	for _, t := range types {
		if t != "text" {
			return true
		}
	}
	return false
}

// waitForGetUpdatesCount polls the TelegramStub until at least n
// getUpdates calls have been recorded for the given token. Used for
// pacing assertions where the test needs to wait for fault-injection
// drains rather than for an external response. PeekSent doesn't drain,
// so the count is monotonic.
func waitForGetUpdatesCount(stub *testharness.TelegramStub, token string, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		for _, c := range stub.PeekSent(token) {
			if c.Method == "getUpdates" {
				count++
			}
		}
		if count >= n {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// harnessTempDir returns the harness's temp-dir root. Kept as a
// thin wrapper around the public accessor so older test sites don't
// need to be rewritten in one go.
func harnessTempDir(h *testharness.Harness) string {
	return h.TempDir()
}

// agentWorkspace returns the on-disk workspace path the harness
// allocated for an agent. Thin wrapper around the public accessor.
func agentWorkspace(h *testharness.Harness, agentID string) string {
	return h.AgentWorkspace(agentID)
}

// recorderEntry mirrors the JSONL shape cc-stub writes. Kept private to
// the integration test package — it's an internal contract between
// cc-stub and the L2 tests, not a public API.
//
// Seven kinds:
//   "invocation"          — one per process spawn (workdir, resume_id, flags)
//   "user_message"        — one per user message processed (session_id, workdir, text_prefix)
//   "permission_request"  — one per scripted can_use_tool control_request the
//                           stub emitted (control_request_id, outbound_tool_name)
//   "permission_cancel"   — one per scripted control_cancel_request the stub
//                           emitted (control_request_id). Tests pair with the
//                           preceding permission_request to assert ordering.
//   "control_response"    — one per inbound control_response received from foci
//                           (control_request_id + raw inner payload)
//   "init_system"         — one per spawn that received an initialize
//                           control_request (prompt length + sha256 + head)
//   "bash_tool_use"       — one per Bash tool_use cc-stub ran inline
//                           (tool_use_id, bash_command, bash_output, is_error)
type recorderEntry struct {
	Kind             string          `json:"kind"`
	Timestamp        string          `json:"ts"`
	Workdir          string          `json:"workdir"`
	ResumeID         string          `json:"resume_id,omitempty"`
	Model            string          `json:"model,omitempty"`
	Flags            []string        `json:"flags,omitempty"`
	PID              int             `json:"pid,omitempty"`
	SessionID        string          `json:"session_id,omitempty"`
	TextPrefix       string          `json:"text_prefix,omitempty"`
	// ContentBlockTypes — per-block "type" values from the user envelope
	// (e.g. ["text", "image"]). Populated by cc-stub for structured
	// content; empty when the user payload is a flat string.
	ContentBlockTypes []string `json:"content_block_types,omitempty"`
	ControlRequestID string          `json:"control_request_id,omitempty"`
	OutboundToolName string          `json:"outbound_tool_name,omitempty"`
	ControlResponse  json.RawMessage `json:"control_response,omitempty"`
	// init_system kind
	PromptLen    int    `json:"prompt_len,omitempty"`
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	PromptHead   string `json:"prompt_head,omitempty"`
	AppendLen    int    `json:"append_len,omitempty"`
	AppendSHA256 string `json:"append_sha256,omitempty"`
	// bash_tool_use kind
	ToolUseID   string `json:"tool_use_id,omitempty"`
	BashCommand string `json:"bash_command,omitempty"`
	BashOutput  string `json:"bash_output,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
}

// readRecorderEntries parses every JSONL line from the recorder file.
// Missing file returns an empty slice (caller is polling) — that's a
// valid intermediate state, not an error.
func readRecorderEntries(t *testing.T, path string) []recorderEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []recorderEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r recorderEntry
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode recorder line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// invocationsByWorkdir filters to invocation entries whose workdir
// contains the given substring. Order-preserving.
func invocationsByWorkdir(entries []recorderEntry, workdirSubstr string) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "invocation" && strings.Contains(e.Workdir, workdirSubstr) {
			out = append(out, e)
		}
	}
	return out
}
