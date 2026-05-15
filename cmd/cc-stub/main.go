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
	"strconv"
	"strings"
	"time"
)

// invocation is one line written to the recorder file. Tests read this
// file to assert on what foci handed to the (stubbed) CC subprocess —
// crucially the working directory, which is the regression net for the
// cross-agent session-key bug.
type invocation struct {
	Timestamp string   `json:"ts"`
	Workdir   string   `json:"workdir"`
	ResumeID  string   `json:"resume_id"`
	Model     string   `json:"model"`
	Flags     []string `json:"flags"`
	PID       int      `json:"pid"`
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
	// followed by a result message to close the turn.
	respText := os.Getenv("CCSTUB_RESPONSE")
	for in.Scan() {
		var env map[string]any
		if err := json.Unmarshal(in.Bytes(), &env); err != nil {
			continue
		}
		switch env["type"] {
		case "user":
			userText := extractUserText(env)
			reply := respText
			if reply == "" {
				// Default: echo back so tests can assert on round-trip
				// without needing to set the env var.
				reply = "stub-reply: " + userText
			}
			emit(out, map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": reply},
					},
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
// Foci sends a `message.contentString` field; fall back to other shapes
// gracefully so unrecognised inputs don't break the stub.
func extractUserText(env map[string]any) string {
	msg, ok := env["message"].(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := msg["contentString"].(string); ok {
		return s
	}
	if blocks, ok := msg["content"].([]any); ok {
		var sb strings.Builder
		for _, b := range blocks {
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

// recordInvocation appends one JSONL line to CCSTUB_RECORDER (if set).
// Tests assert on this file to verify foci handed work to the correct
// workdir / resume id / agent. Silent no-op if the env var is unset or
// the file can't be opened — recorder failures must not break the stub.
func recordInvocation(resume, model string) {
	path := os.Getenv("CCSTUB_RECORDER")
	if path == "" {
		return
	}
	wd, _ := os.Getwd()
	inv := invocation{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:   wd,
		ResumeID:  resume,
		Model:     model,
		Flags:     os.Args[1:],
		PID:       os.Getpid(),
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(inv)
	_, _ = f.Write(append(b, '\n'))
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
