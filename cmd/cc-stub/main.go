// Command cc-stub is a fake `claude` binary for foci integration tests.
//
// foci-gw spawns a real CC subprocess for every CC-backed agent and
// communicates with it via the stream-json NDJSON protocol. Real CC is
// slow, non-deterministic, and burns mana on every invocation — none of
// which is acceptable on a PR-blocking test layer. cc-stub plays the
// other end of the protocol well enough for integration tests to exercise
// foci's wiring (agent dispatch, session routing, tool execution, lifecycle).
//
// What it does:
//   - Accepts the same flag surface foci passes (--print, --resume, etc.)
//   - Reads NDJSON control requests + user messages on stdin
//   - Emits NDJSON system/init + assistant + result lines on stdout
//   - Records every invocation to a JSONL file (path via CCSTUB_RECORDER env)
//   - Supports failure-mode env vars for lifecycle/error-path tests
//
// What it doesn't do:
//   - Real model inference; canned responses only
//   - Tool execution (it emits tool_use blocks if scripted; foci handles the rest)
//   - Compaction, MCP elicitations, partial-message streaming
//
// Foci is pointed at this binary by setting [cc_backend].claude_binary in
// the test foci.toml. The binary must be on $PATH or referenced absolutely.
//
// Env vars (all optional):
//   CCSTUB_RECORDER       — path to a JSONL file; each invocation appends one line
//   CCSTUB_EXIT_CODE      — exit with this code before any handshake (lifecycle tests)
//   CCSTUB_FAIL_ON_RESUME — if "1"/"true" and --resume is set, exit 1 (simulates missing JSONL)
//   CCSTUB_HANG           — duration to sleep before the handshake (e.g. "5s")
//   CCSTUB_RESPONSE       — assistant reply text; default echoes the user prompt
//   CCSTUB_SCRIPT_DIR     — directory holding per-workdir scripts; the file
//                           named after the basename of $CWD (e.g. "fotini.json")
//                           is read on the next user message and its three
//                           sections applied:
//                             • text                — assistant reply text
//                             • tool_uses[]         — tool_use blocks emitted
//                                                     inside the assistant
//                                                     message (Bash blocks
//                                                     are literally executed
//                                                     so the exec bridge
//                                                     fires).
//                             • permission_requests[] — can_use_tool
//                                                     control_request
//                                                     envelopes emitted
//                                                     AFTER the result, with
//                                                     stable test-supplied
//                                                     request_ids so the
//                                                     test can construct
//                                                     "im:<reqID>:<idx>"
//                                                     callback strings ahead
//                                                     of time.
//                           Script is consumed one-shot — the file is removed
//                           after the next user message processes.
//
// Usage:
//   cc-stub --print --input-format stream-json --output-format stream-json \
//           [--resume <session-id>] [--model <m>] [--allowedTools <rules>] \
//           [--settings <json>] [--permission-prompt-tool stdio] \
//           [--include-partial-messages] [--include-hook-events] [--verbose]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// scriptedToolUse describes one tool_use block a scripted cc-stub should
// emit in response to a user message. Tests author these via JSON files.
type scriptedToolUse struct {
	Name  string         `json:"name"`  // "Bash", "Read", ...
	Input map[string]any `json:"input"` // tool-specific input shape
	ID    string         `json:"id"`    // optional; auto-generated if empty
}

// scriptedPermSuggestion is one of CC's "Always: <prefix>" hint chips.
// Tests use it to verify foci's Choices() emits the third button.
type scriptedPermSuggestion struct {
	Prefix string `json:"prefix"`
	Scope  string `json:"scope,omitempty"` // "session" | etc.; empty = stub default
}

// scriptedPermissionRequest describes one can_use_tool control_request
// cc-stub should emit on the next user message — this is the half of the
// CC permission protocol that real CC does for any tool call not on its
// own allowlist, and that foci routes to Telegram for approval.
//
// Field semantics:
//   - ToolName / Input mirror CC's PermissionRequestPayload exactly so
//     foci's auto-approve matcher and prompt-text builders see normal data.
//   - RequestID — caller-supplied so tests can construct stable
//     "im:<reqID>:<idx>" callback strings ahead of time. Empty = stub
//     auto-generates a "perm_stub_<nano>_<i>" id, surfaced via the
//     recorder so polling tests can still discover it.
//   - ToolUseID — CC's identifier for the tool block being authorised;
//     foci echoes it back in the PermissionAllow response. Empty = auto.
//   - Suggestions — populates permission_suggestions so foci.Choices()
//     emits the third "Always: <prefix>" button.
type scriptedPermissionRequest struct {
	ToolName    string                   `json:"tool_name"`
	Input       map[string]any           `json:"input"`
	RequestID   string                   `json:"request_id,omitempty"`
	ToolUseID   string                   `json:"tool_use_id,omitempty"`
	Suggestions []scriptedPermSuggestion `json:"permission_suggestions,omitempty"`
}

// stubScript is the on-disk format for $CCSTUB_SCRIPT_DIR/<workdir>.json.
// Text is the assistant text; ToolUses are emitted in the same assistant
// message after the text block; PermissionRequests are emitted as
// separate control_request envelopes after the assistant message.
type stubScript struct {
	Text               string                      `json:"text"`
	ToolUses           []scriptedToolUse           `json:"tool_uses"`
	PermissionRequests []scriptedPermissionRequest `json:"permission_requests"`
}

// recorderEntry is one line written to the recorder file. Two event
// kinds are emitted:
//
//   Kind="invocation" — at process spawn; captures workdir / resume / flags.
//   Kind="user_message" — for each user message processed during the
//     lifetime of the long-lived CC process; captures session_id +
//     workdir + a short prefix of the text. This is the regression net
//     for the cross-agent send_to_session bug: a turn that targets
//     clutch's session MUST land in clutch's workdir.
//   Kind="permission_request" — for each can_use_tool control_request the
//     stub emitted on a scripted user turn; captures the request_id so
//     tests that didn't pre-specify one can still discover it.
//   Kind="control_response" — for each inbound control_response from foci
//     (the answer to a can_use_tool request); captures the full inner
//     payload (behavior / message / decisionClassification / updatedPermissions)
//     so tests can assert on whatever they need.
//
// Tests read the JSONL file, group by kind, and assert structurally.
type recorderEntry struct {
	Kind      string   `json:"kind"`
	Timestamp string   `json:"ts"`
	Workdir   string   `json:"workdir"`

	// invocation-only
	ResumeID string   `json:"resume_id,omitempty"`
	Model    string   `json:"model,omitempty"`
	Flags    []string `json:"flags,omitempty"`
	PID      int      `json:"pid,omitempty"`

	// user_message-only
	SessionID  string `json:"session_id,omitempty"`
	TextPrefix string `json:"text_prefix,omitempty"`

	// permission_request / control_response shared
	ControlRequestID string          `json:"control_request_id,omitempty"`
	// permission_request only — the outbound shape the stub emitted, so a
	// test can also assert "what cc-stub asked foci to approve".
	OutboundToolName string          `json:"outbound_tool_name,omitempty"`
	// control_response only — the inner foci payload (e.g. {"behavior":"allow",...}).
	// Stored as raw JSON so tests can pick fields without coupling to the stub's
	// view of the schema.
	ControlResponse  json.RawMessage `json:"control_response,omitempty"`
}

func main() {
	// Parse the flag surface foci passes. We ignore most values — the
	// stub doesn't care about content, only that flags are accepted.
	var (
		printMode        bool
		inputFormat      string
		outputFormat     string
		permTool         string
		includePartial   bool
		includeHook      bool
		verbose          bool
		model            string
		resume           string
		allowedTools     string
		settings         string
		appendSysPrompt  string
		helpFlag         bool
	)
	fs := flag.NewFlagSet("cc-stub", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.BoolVar(&printMode, "print", false, "output-only mode (matches CC)")
	fs.StringVar(&inputFormat, "input-format", "", "stream-json | text")
	fs.StringVar(&outputFormat, "output-format", "", "stream-json | text")
	fs.StringVar(&permTool, "permission-prompt-tool", "", "stdio")
	fs.BoolVar(&includePartial, "include-partial-messages", false, "")
	fs.BoolVar(&includeHook, "include-hook-events", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	fs.StringVar(&model, "model", "", "model identifier")
	fs.StringVar(&resume, "resume", "", "resume session id (empty = fresh)")
	fs.StringVar(&allowedTools, "allowedTools", "", "permission rules")
	fs.StringVar(&settings, "settings", "", "hook settings JSON")
	fs.StringVar(&appendSysPrompt, "append-system-prompt", "", "")
	fs.BoolVar(&helpFlag, "h", false, "show usage")
	fs.BoolVar(&helpFlag, "help", false, "show usage")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// CC ignores unknown flags by default; mimic that by warning to stderr
		// rather than failing. Some future foci flag we don't model shouldn't
		// blow up tests.
		fmt.Fprintln(os.Stderr, "cc-stub: flag parse warning:", err)
	}
	if helpFlag {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(0)
	}

	// Failure modes — checked before any work so lifecycle tests can
	// observe a clean exit-without-output.
	if code := os.Getenv("CCSTUB_EXIT_CODE"); code != "" {
		if n, err := strconv.Atoi(code); err == nil {
			os.Exit(n)
		}
	}
	if isTruthy(os.Getenv("CCSTUB_FAIL_ON_RESUME")) && resume != "" {
		// Simulates a CC that received --resume but couldn't find the
		// referenced session — exits non-zero, foci's delegated wrapper
		// retries without --resume.
		fmt.Fprintln(os.Stderr, "cc-stub: --resume id not found, exiting non-zero")
		os.Exit(1)
	}
	if hang := os.Getenv("CCSTUB_HANG"); hang != "" {
		if d, err := time.ParseDuration(hang); err == nil {
			time.Sleep(d)
		}
	}

	recordInvocation(resume, model)

	// Generate or echo session_id. Fresh runs make a new one; resumes
	// echo back the same id so foci's b.sessionID stays consistent.
	sessionID := resume
	if sessionID == "" {
		sessionID = fmt.Sprintf("stub-%d", time.Now().UnixNano())
	}

	// stdin reader (NDJSON, one envelope per line)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// Process the first envelope — should be control_request initialize.
	// Respond with control_response immediately so foci's readyCh closes,
	// then emit system/init so foci stores our session_id. (Real CC emits
	// these in the opposite order for resumes; foci tolerates either.)
	if !in.Scan() {
		// Stdin closed before we got the init handshake — exit cleanly.
		// This is what foci sees when it kills the subprocess at teardown.
		return
	}
	var first map[string]any
	_ = json.Unmarshal(in.Bytes(), &first)

	// Best-effort: send a control_response only if we saw an init request.
	if reqID, _ := extractInitRequestID(first); reqID != "" {
		emit(out, map[string]any{
			"type":        "control_response",
			"request_id":  reqID,
			"response": map[string]any{
				"subtype":    "initialize_success",
				"session_id": sessionID,
			},
		})
	}
	emit(out, map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
		"model":      ifEmpty(model, "stub-model"),
		"tools":      []string{},
	})
	out.Flush()

	// Now loop on user messages. For each, emit one assistant text block
	// (plus any scripted tool_use blocks), followed by a result message
	// to close the turn.
	//
	// Script is loaded PER user message — not once at startup — because
	// cc-stub is long-lived (survives across turns) and the test may
	// write a script after the stub spawned (e.g. fotini's first turn
	// is onboarding, the second turn is the scripted send_to_session).
	respText := os.Getenv("CCSTUB_RESPONSE")
	for in.Scan() {
		var env map[string]any
		if err := json.Unmarshal(in.Bytes(), &env); err != nil {
			continue
		}
		switch env["type"] {
		case "user":
			userText := extractUserText(env)
			recordUserMessage(sessionID, userText)
			script := loadScript()
			reply := respText
			if script != nil && script.Text != "" {
				reply = script.Text
			}
			if reply == "" {
				// Default: echo back so tests can assert on round-trip
				// without needing to set the env var.
				reply = "stub-reply: " + userText
			}
			content := []map[string]any{
				{"type": "text", "text": reply},
			}
			if script != nil {
				for _, tu := range script.ToolUses {
					id := tu.ID
					if id == "" {
						id = fmt.Sprintf("toolu_stub_%d", time.Now().UnixNano())
					}
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  tu.Name,
						"input": tu.Input,
					})
					// Real CC runs the tool internally. For test fidelity
					// with the exec bridge (foci_* shell functions live
					// in BASH_ENV), the stub literally runs Bash tool_use
					// commands itself. Other tool types are emitted but
					// not executed — tests can extend this when needed.
					if tu.Name == "Bash" {
						runBashToolUse(tu.Input)
					}
				}
			}
			emit(out, map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"session_id": sessionID,
			})
			emit(out, map[string]any{
				"type":       "result",
				"result":     reply,
				"session_id": sessionID,
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			})
			out.Flush()
			// Emit any scripted can_use_tool control_requests AFTER the
			// assistant + result envelopes. Foci processes them on its
			// reader goroutine and dispatches each to permPromptFn —
			// which routes through SendInteractiveMessageWithID, so
			// each request_id becomes a registered Telegram prompt that
			// PushCallbackQuery can resolve. Multiple entries in one
			// script land as multiple concurrent pending permissions
			// (testing the queue/concurrency assertions in N1).
			if script != nil {
				for i, pr := range script.PermissionRequests {
					reqID := pr.RequestID
					if reqID == "" {
						reqID = fmt.Sprintf("perm_stub_%d_%d", time.Now().UnixNano(), i)
					}
					toolUseID := pr.ToolUseID
					if toolUseID == "" {
						toolUseID = fmt.Sprintf("toolu_stub_%d_%d", time.Now().UnixNano(), i)
					}
					payload := map[string]any{
						"subtype":     "can_use_tool",
						"tool_name":   pr.ToolName,
						"input":       pr.Input,
						"tool_use_id": toolUseID,
					}
					if len(pr.Suggestions) > 0 {
						payload["permission_suggestions"] = pr.Suggestions
					}
					emit(out, map[string]any{
						"type":       "control_request",
						"request_id": reqID,
						"request":    payload,
					})
					recordPermissionRequest(reqID, pr.ToolName)
				}
				if len(script.PermissionRequests) > 0 {
					out.Flush()
				}
			}
			// One-shot: delete the script file after applying so the
			// next user message in this long-lived process uses
			// defaults. Critical for tests that trigger send_to_session
			// — the SESSION RESPONSE injection comes back as a user
			// message and would re-trigger the script in an infinite
			// loop. Tests that need multi-turn scripted behaviour
			// re-write the file between turns.
			if script != nil {
				dir := os.Getenv("CCSTUB_SCRIPT_DIR")
				if dir != "" {
					if wd, err := os.Getwd(); err == nil {
						_ = os.Remove(filepath.Join(dir, filepath.Base(wd)+".json"))
					}
				}
			}
		case "control_request":
			// e.g. interrupt — ack with a control_response.
			if reqID, ok := env["request_id"].(string); ok {
				emit(out, map[string]any{
					"type":       "control_response",
					"request_id": reqID,
					"response":   map[string]any{"subtype": "ack"},
				})
				out.Flush()
			}
		case "control_response":
			// Inbound from foci — typically the answer to a can_use_tool
			// permission request the stub emitted on a previous turn.
			// Record the full inner payload so tests can assert on
			// behavior / message / updatedPermissions / decisionClassification
			// without coupling to the protocol's nesting shape.
			recordControlResponse(env)
		default:
			// Ignore anything else (e.g. partial-message events foci doesn't expect).
		}
	}
}

// emit writes one NDJSON line. Errors are silent because the stub is
// best-effort — if foci has already closed stdout, there's nowhere to log.
func emit(w *bufio.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
	_, _ = w.WriteString("\n")
}

// extractInitRequestID pulls the request_id from a parsed control_request
// envelope whose subtype is "initialize". Returns "" for anything else.
func extractInitRequestID(env map[string]any) (string, bool) {
	if env["type"] != "control_request" {
		return "", false
	}
	req, ok := env["request"].(map[string]any)
	if !ok {
		return "", false
	}
	if req["subtype"] != "initialize" {
		return "", false
	}
	reqID, _ := env["request_id"].(string)
	return reqID, true
}

// extractUserText pulls the text body out of a `{"type":"user", ...}` envelope.
// Foci's UserPayload.MarshalJSON emits `{"role":"user","content":"<string>"}`
// for the common case and `{"role":"user","content":[<blocks>]}` for
// structured content. The stub handles both shapes.
func extractUserText(env map[string]any) string {
	msg, ok := env["message"].(map[string]any)
	if !ok {
		return ""
	}
	switch v := msg["content"].(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, b := range v {
			if m, ok := b.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// recordInvocation appends one JSONL line tagged kind="invocation" to
// CCSTUB_RECORDER (if set). Silent no-op if the env var is unset or the
// file can't be opened — recorder failures must not break the stub.
func recordInvocation(resume, model string) {
	wd, _ := os.Getwd()
	writeRecorder(recorderEntry{
		Kind:      "invocation",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:   wd,
		ResumeID:  resume,
		Model:     model,
		Flags:     os.Args[1:],
		PID:       os.Getpid(),
	})
}

// recordUserMessage appends one JSONL line tagged kind="user_message" so
// tests can assert which session+workdir handled the message. This is
// the per-turn signal the cross-agent regression test asserts on — the
// invocation-only recorder couldn't distinguish between turns inside a
// long-lived CC process.
func recordUserMessage(sessionID, text string) {
	wd, _ := os.Getwd()
	prefix := text
	// send_to_session prepends a ~600-char SYSTEM INJECTION context
	// note before the actual user payload; cap at 2k so tests can
	// match markers that live past that header.
	if len(prefix) > 2000 {
		prefix = prefix[:2000]
	}
	writeRecorder(recorderEntry{
		Kind:       "user_message",
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:    wd,
		SessionID:  sessionID,
		TextPrefix: prefix,
	})
}

// recordPermissionRequest appends one JSONL line tagged
// kind="permission_request" so tests that don't pre-specify a request_id
// can still observe what cc-stub emitted (and assert on tool_name).
func recordPermissionRequest(reqID, toolName string) {
	wd, _ := os.Getwd()
	writeRecorder(recorderEntry{
		Kind:             "permission_request",
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:          wd,
		ControlRequestID: reqID,
		OutboundToolName: toolName,
	})
}

// recordControlResponse appends one JSONL line tagged kind="control_response"
// capturing the inner foci payload (the actual answer body, e.g.
// {"behavior":"allow",...}). The inbound envelope is shaped:
//
//	{"type":"control_response","response":{"subtype":"success","request_id":"...","response":{...}}}
//
// We drop the outer subtype-success wrapping and store only the innermost
// object since that's what tests assert on.
func recordControlResponse(env map[string]any) {
	wd, _ := os.Getwd()
	respObj, _ := env["response"].(map[string]any)
	reqID, _ := respObj["request_id"].(string)
	inner, _ := json.Marshal(respObj["response"])
	writeRecorder(recorderEntry{
		Kind:             "control_response",
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:          wd,
		ControlRequestID: reqID,
		ControlResponse:  inner,
	})
}

// writeRecorder appends one JSONL line to CCSTUB_RECORDER (if set).
func writeRecorder(e recorderEntry) {
	path := os.Getenv("CCSTUB_RECORDER")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
}

// runBashToolUse runs the "command" field of a Bash tool_use input as
// a non-interactive bash subshell, inheriting the stub's environment so
// BASH_ENV / FOCI_SOCK (set by foci-gw) take effect — that's how
// foci_send_to_session and the other shell-exported foci tools reach
// the exec bridge. Output is forwarded to stderr for debugging; the
// stub does not feed it back to foci as a tool_result (real CC handles
// that internally; foci's reader doesn't currently consume tool_result
// blocks, so emitting one would be silent at best).
//
// 10-second wall clock cap — enough for any exec-bridge round-trip,
// short enough that runaway scripts fail loud rather than hanging tests.
func runBashToolUse(input map[string]any) {
	cmd, _ := input["command"].(string)
	if cmd == "" {
		fmt.Fprintln(os.Stderr, "cc-stub: Bash tool_use with empty command — skipped")
		return
	}
	c := exec.Command("bash", "-c", cmd)
	c.Stdout = os.Stderr // tee both to stderr so the test harness can fish them out
	c.Stderr = os.Stderr
	c.Env = os.Environ()
	// Wall clock guard.
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub: Bash start failed: %v\n", err)
		return
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(os.Stderr, "cc-stub: Bash command exited: %v\n", err)
		}
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		fmt.Fprintln(os.Stderr, "cc-stub: Bash command timed out after 10s, killed")
	}
}

// loadScript reads the per-workdir script JSON from $CCSTUB_SCRIPT_DIR.
// Returns nil if the env var is unset, the file doesn't exist, or the
// content is unparseable — in any of those cases the stub falls back to
// its echo-default behaviour.
func loadScript() *stubScript {
	dir := os.Getenv("CCSTUB_SCRIPT_DIR")
	if dir == "" {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub: loadScript getwd failed: %v\n", err)
		return nil
	}
	path := filepath.Join(dir, filepath.Base(wd)+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub [error] loadScript: no file at %s (wd=%s)\n", path, wd)
		return nil
	}
	var s stubScript
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub [error] failed to parse script %s: %v\n", path, err)
		return nil
	}
	// Log script size, not contents — tests use multi-MB Text payloads
	// to exercise foci's stdout scanner cap, and printing them here
	// wedges the stub on its stderr pipe before stdout ever gets the
	// payload. Length + tool-use count is the diagnostic value.
	fmt.Fprintf(os.Stderr, "cc-stub [error] loaded script %s (text_len=%d tool_uses=%d)\n", path, len(s.Text), len(s.ToolUses))
	return &s
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func isTruthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

const usage = `cc-stub — fake claude binary for foci integration tests.

Reads stream-json NDJSON on stdin, emits stream-json NDJSON on stdout,
records each invocation to $CCSTUB_RECORDER (JSONL).

Env vars (all optional):
  CCSTUB_RECORDER       path to JSONL file; one line appended per invocation
  CCSTUB_EXIT_CODE      exit with this code before handshake (lifecycle tests)
  CCSTUB_FAIL_ON_RESUME if truthy and --resume is set, exit 1
  CCSTUB_HANG           sleep this duration before handshake (e.g. "5s")
  CCSTUB_RESPONSE       assistant reply text (default echoes user prompt)

Flags accepted (most ignored — stub only cares about --resume, --model):
  --print --input-format --output-format --permission-prompt-tool
  --include-partial-messages --include-hook-events --verbose
  --model --resume --allowedTools --settings --append-system-prompt
  -h / --help

Foci points at this binary by setting [cc_backend].claude_binary in
foci.toml. See cmd/cc-stub/main.go for the full protocol.`
