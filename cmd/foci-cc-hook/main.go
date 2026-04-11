// Command foci-cc-hook is a tiny helper that foci installs as a
// PostToolUse and PostToolUseFailure hook on Claude Code sessions.
// CC invokes the configured hook binary after each tool execution,
// pipes a JSON envelope containing the tool call + its response (or
// error) into the binary's stdin, and captures the binary's stdout
// into a system/hook_response message on its stream-json output.
//
// This binary reads that JSON envelope, extracts the fields foci
// needs for OnToolEnd correlation (hook_event_name, tool_use_id,
// tool_name, tool_response, error, agent_id), truncates large
// response payloads to keep stream-json lines under ccstream's
// scanner limit, and writes a compact JSON object to stdout.
//
// The helper always exits 0 regardless of parse errors — CC uses
// exit codes to gate tool execution (exit 2 blocks), so we must not
// accidentally interfere with the user's turn. Any parse failure on
// our side is a silent drop; foci's stream parser will log at debug
// when it sees the empty or malformed hook_response.stdout and
// graceful-skip the OnToolEnd dispatch.
package main

import (
	"encoding/json"
	"io"
	"os"
)

// installIDFlag is the argv flag foci sets when installing the hook so the
// binary can echo the install ID back in its output. Each foci backend uses
// a unique install ID in its hook command string — see
// internal/delegator/ccstream/hooks.go for the generation and filter logic.
// Without the ID round-trip, multiple backends sharing a workdir can't tell
// which hook_response events belong to which backend.
const installIDFlag = "--install"

// maxFieldBytes bounds the size of tool_response / error fields in the
// emitted JSON so each hook_response line from CC stays well under the
// ccstream reader's 1MB scanner limit (internal/delegator/ccstream/reader.go
// maxTokenSize). Without truncation, a multi-MB file read would blow the
// scanner and tear down the ccstream backend via OnReaderStopped.
const maxFieldBytes = 64 * 1024

// hookInput mirrors the JSON envelope CC writes to the hook's stdin for
// PostToolUse / PostToolUseFailure events. See claude-code
// src/entrypoints/sdk/coreSchemas.ts:436-459 for the canonical schema.
// Fields not consumed by foci are intentionally omitted.
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolResponse  json.RawMessage `json:"tool_response,omitempty"`
	Error         string          `json:"error,omitempty"`
	AgentID       string          `json:"agent_id,omitempty"`
	IsInterrupt   bool            `json:"is_interrupt,omitempty"`
	IsTimeout     bool            `json:"is_timeout,omitempty"`
}

// hookOutput is the compact JSON foci's ccstream handleHookResponse parser
// expects to find in hook_response.stdout. Keep field names aligned with
// the stable contract in internal/delegator/ccstream/hooks.go.
//
// InstallID is echoed back from argv so foci can filter hook_response
// events by the originating backend when multiple foci backends share a
// workdir and therefore share a .claude/settings.local.json file.
type hookOutput struct {
	HookEvent    string `json:"hook_event"`
	InstallID    string `json:"install_id,omitempty"`
	ToolUseID    string `json:"tool_use_id"`
	ToolName     string `json:"tool_name"`
	ToolResponse string `json:"tool_response,omitempty"`
	Error        string `json:"error,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	IsError      bool   `json:"is_error"`
}

// parseInstallID extracts the value of the --install flag from argv.
// Returns empty string when absent. Accepts both `--install X` (two args)
// and `--install=X` (one arg) forms. Silent on malformed input — foci's
// stream parser handles missing install_ids by treating them as "not ours".
func parseInstallID(args []string) string {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == installIDFlag && i+1 < len(args) {
			return args[i+1]
		}
		const eq = installIDFlag + "="
		if len(a) > len(eq) && a[:len(eq)] == eq {
			return a[len(eq):]
		}
	}
	return ""
}

func main() {
	installID := parseInstallID(os.Args)

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return // exit 0 — silent drop, don't interfere with the turn
	}
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return
	}

	out := hookOutput{
		HookEvent: in.HookEventName,
		InstallID: installID,
		ToolUseID: in.ToolUseID,
		ToolName:  in.ToolName,
		AgentID:   in.AgentID,
		IsError:   in.HookEventName == "PostToolUseFailure" || in.IsInterrupt || in.IsTimeout,
	}
	if len(in.ToolResponse) > 0 {
		out.ToolResponse = truncate(decodeToolResponse(in.ToolResponse), maxFieldBytes)
	}
	if in.Error != "" {
		out.Error = truncate(in.Error, maxFieldBytes)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

// truncate caps s at max bytes, appending a visible marker when it had to
// cut. We cut on byte boundaries, not rune boundaries — the receiving
// parser treats the field as an opaque string so we don't need to worry
// about splitting multi-byte UTF-8 sequences.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// decodeToolResponse turns the raw tool_response RawMessage into a plain
// string for downstream display. CC encodes most file/text tool results as
// JSON strings (e.g. `"contents of the file"`), and forwarding the raw
// bytes verbatim would leave the surrounding double quotes in the user-
// visible "Show full" expansion. We try to unmarshal as a string first;
// when that fails (objects, arrays, numbers — Bash structured output for
// example), we fall back to the raw bytes so the tracker still gets
// something legible.
func decodeToolResponse(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
