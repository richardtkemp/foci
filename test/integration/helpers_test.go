//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// scopedLoggingTOML returns a "[logging]" TOML block that fully scopes ALL
// FOUR log-path keys (event_file, api_file, payload_file, archive_dir) under
// tempDir, with an explicit event_file override at eventLogPath and the
// given log_rotation setting.
//
// Use this — never a hand-written "[logging]\nevent_file = ...\n" block —
// whenever a test needs a custom event log path. writeTestConfig only emits
// its own tempdir-scoped [logging] defaults when a test's ExtraConfigTOML
// has NO "[logging]" header at all (skip-if-overridden, since TOML rejects
// duplicate tables): a test-supplied header that overrides only SOME keys
// leaves the rest at their package defaults ("logs/api.jsonl" etc, see
// types.go), which config.ResolvePath joins against the REAL host
// os.UserHomeDir() — this test process never overrides its own $HOME, only
// the subprocess it spawns does. Those defaults can then alias the host's
// actual production log files. This is exactly what caused foci_todo
// #1479's live incident (a test-spawned foci-gw's startup rotation pass
// truncated the production api.jsonl/api-payload.jsonl out from under the
// live foci-gw holding them open) and is guarded structurally in two
// places: this helper (so new tests don't reintroduce the partial-override
// shape) and testharness.verifyGeneratedLogPaths (a harness-level assertion
// that fails loudly, before ANY config with an escaping log path ever
// reaches a spawned foci-gw, regardless of how it was generated) — see
// foci_todo #1492.
func scopedLoggingTOML(tempDir, eventLogPath string, rotation bool) string {
	rot := "false"
	if rotation {
		rot = "true"
	}
	return "[logging]\n" +
		"event_file = \"" + eventLogPath + "\"\n" +
		"api_file = \"" + tempDir + "/api.jsonl\"\n" +
		"payload_file = \"" + tempDir + "/api-payload.jsonl\"\n" +
		"archive_dir = \"" + tempDir + "/archive\"\n" +
		"log_rotation = " + rot + "\n"
}

// waitForStderr polls the harness stderr until it contains substr or
// the timeout expires. Returns true on hit. Used for assertions about
// foci's log surface (escalate lines, sanitized error messages).
func waitForStderr(h *testharness.Harness, substr string, timeout time.Duration) bool {
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
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
//
//	"invocation"          — one per process spawn (workdir, resume_id, flags)
//	"user_message"        — one per user message processed (session_id, workdir, text_prefix)
//	"permission_request"  — one per scripted can_use_tool control_request the
//	                        stub emitted (control_request_id, outbound_tool_name)
//	"permission_cancel"   — one per scripted control_cancel_request the stub
//	                        emitted (control_request_id). Tests pair with the
//	                        preceding permission_request to assert ordering.
//	"control_response"    — one per inbound control_response received from foci
//	                        (control_request_id + raw inner payload)
//	"init_system"         — one per spawn that received an initialize
//	                        control_request (prompt length + sha256 + head)
//	"bash_tool_use"       — one per Bash tool_use cc-stub ran inline
//	                        (tool_use_id, bash_command, bash_output, is_error)
type recorderEntry struct {
	Kind      string   `json:"kind"`
	Timestamp string   `json:"ts"`
	Workdir   string   `json:"workdir"`
	ResumeID  string   `json:"resume_id,omitempty"`
	Model     string   `json:"model,omitempty"`
	Flags     []string `json:"flags,omitempty"`
	PID       int      `json:"pid,omitempty"`
	// Binary captures os.Args[0] from the spawned cc-stub — i.e. the
	// resolved path foci called claude_binary at. Per-agent override
	// tests use this to confirm a spawn landed at the override path
	// rather than the global cc_backend.claude_binary path.
	Binary     string `json:"binary,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	TextPrefix string `json:"text_prefix,omitempty"`
	// ContentBlockTypes — per-block "type" values from the user envelope
	// (e.g. ["text", "image"]). Populated by cc-stub for structured
	// content; empty when the user payload is a flat string.
	ContentBlockTypes []string        `json:"content_block_types,omitempty"`
	ControlRequestID  string          `json:"control_request_id,omitempty"`
	OutboundToolName  string          `json:"outbound_tool_name,omitempty"`
	ControlResponse   json.RawMessage `json:"control_response,omitempty"`
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
	out, err := parseRecorderEntries(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// parseRecorderEntries decodes the complete lines of a cc-stub recorder file.
// A final segment with no trailing newline is a line cc-stub is still
// appending (a respawn can write its invocation line while the test reads),
// so it is skipped, not decoded: decoding it failed on a torn line under load
// (TestL2_SessionLifecycle_ResumeIDSurvivesRespawn, 2026-09-25). A malformed
// COMPLETE line is still an error.
func parseRecorderEntries(b []byte) ([]recorderEntry, error) {
	s := string(b)
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return nil, nil
	}
	var out []recorderEntry
	for _, line := range strings.Split(s[:i], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r recorderEntry
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("decode recorder line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// invocationsByWorkdir filters to invocation entries whose workdir
// contains the given substring. Order-preserving.
// isBatchInvocation reports whether a recorded cc-stub spawn is a batch
// session (nudge extraction, consolidation, foci_summary): since #1962 a batch
// is an ordinary stream-json session, and what marks it apart is that it
// always launches with --dangerously-skip-permissions, which no harness agent
// does.
func isBatchInvocation(inv recorderEntry) bool {
	for _, f := range inv.Flags {
		if f == "--dangerously-skip-permissions" {
			return true
		}
	}
	return false
}

func invocationsByWorkdir(entries []recorderEntry, workdirSubstr string) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "invocation" && strings.Contains(e.Workdir, workdirSubstr) {
			out = append(out, e)
		}
	}
	return out
}

// TestParseRecorderEntries_SkipsTornFinalLine pins the torn-read fix: a final
// segment with no newline is a line cc-stub is still writing, so it is skipped;
// a malformed complete line is still an error.
func TestParseRecorderEntries_SkipsTornFinalLine(t *testing.T) {
	torn := "{\"kind\":\"user_message\",\"session_id\":\"s1\"}\n{\"kind\":\"invocation\",\"flags\":[\"--pr"
	got, err := parseRecorderEntries([]byte(torn))
	if err != nil {
		t.Fatalf("torn final line must be skipped, got error: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "user_message" {
		t.Fatalf("got %+v, want only the complete user_message line", got)
	}
	if _, err := parseRecorderEntries([]byte("{\"kind\":\"inv\n")); err == nil {
		t.Fatal("a malformed COMPLETE line must still be an error")
	}
}
