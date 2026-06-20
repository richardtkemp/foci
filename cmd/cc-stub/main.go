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
//
//	CCSTUB_RECORDER       — path to a JSONL file; each invocation appends one line
//	CCSTUB_EXIT_CODE      — exit with this code before any handshake (lifecycle tests)
//	CCSTUB_EXIT_CODE_ONCE_MARKER — one-shot gate for CCSTUB_EXIT_CODE: first
//	                               spawn exits, subsequent spawns proceed normally
//	CCSTUB_FAIL_ON_RESUME — if "1"/"true" and --resume is set, exit 1 (simulates missing JSONL)
//	CCSTUB_HANG           — duration to sleep before the handshake (e.g. "5s")
//	CCSTUB_EMIT_COMPACT_BOUNDARY — if truthy, a "/compact ..." user message is
//	                        handled as a compaction turn: emit system/status
//	                        "compacting" then system/compact_boundary then a
//	                        result (no assistant text), mirroring real CC's
//	                        /compact. Drives foci's onCompactionStart/Done and
//	                        the #828 Part B reload-on-compact bounce.
//	CCSTUB_COMPACT_PRE_TOKENS — pre_tokens reported in compact_boundary
//	                        (default 50000); only meaningful with the above.
//	CCSTUB_RESPONSE       — assistant reply text; default echoes the user prompt
//	CCSTUB_SCRIPT_DIR     — directory holding per-workdir scripts; the file
//	                        named after the basename of $CWD (e.g. "fotini.json")
//	                        is read on the next user message and its three
//	                        sections applied:
//	                          • text                — assistant reply text
//	                          • tool_uses[]         — tool_use blocks emitted
//	                                                  inside the assistant
//	                                                  message (Bash blocks
//	                                                  are literally executed
//	                                                  so the exec bridge
//	                                                  fires).
//	                          • permission_requests[] — can_use_tool
//	                                                  control_request
//	                                                  envelopes emitted
//	                                                  AFTER the result, with
//	                                                  stable test-supplied
//	                                                  request_ids so the
//	                                                  test can construct
//	                                                  "im:<reqID>:<idx>"
//	                                                  callback strings ahead
//	                                                  of time.
//	                        Script is consumed one-shot — the file is removed
//	                        after the next user message processes.
//
// Usage:
//
//	cc-stub --print --input-format stream-json --output-format stream-json \
//	        [--resume <session-id>] [--model <m>] [--allowedTools <rules>] \
//	        [--settings <json>] [--permission-prompt-tool stdio] \
//	        [--include-partial-messages] [--include-hook-events] [--verbose]
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	// IntermediateTexts are emitted as SEPARATE assistant messages BEFORE
	// the main Text-bearing assistant message, all within the same turn
	// (before the result envelope). Each entry becomes its own
	// `{"type":"assistant","message":{"content":[{"type":"text","text":...}]}}`
	// envelope. Simulates the 2026-05-18 22:33 shape where CC produced
	// multiple text blocks within a single turn — e.g. agent emits
	// [[NO_RESPONSE]] then a real reply — to exercise turn-sink state
	// across multiple intermediate text events.
	IntermediateTexts []string `json:"intermediate_texts,omitempty"`
	// LateText is emitted as a SECOND assistant message AFTER the result
	// envelope (with a brief delay to let foci process turn completion).
	// Simulates the CC-harness scenario where task-notifications or other
	// internal injections produce assistant text outside any foci-side
	// user message — exercising the session router's late-delivery path.
	LateText string `json:"late_text,omitempty"`

	// StreamDeltas, when non-empty, are emitted as `stream_event` envelopes
	// (each a verbatim Anthropic content_block_delta / text_delta) BEFORE
	// the main assistant envelope, simulating CC's token-level streaming.
	// They drive foci's OnStreamEvent → OnTextDelta → StreamWriter path,
	// so tests can exercise the live-streaming delivery route (incremental
	// message edits) rather than only whole-block assistant text. The
	// concatenation of the deltas should equal Text so the final committed
	// stream edit matches what a real CC turn would produce.
	StreamDeltas []string `json:"stream_deltas,omitempty"`

	// RawLinesBeforeAssistant are literal NDJSON lines written verbatim
	// to stdout BEFORE the assistant envelope, after the user message is
	// consumed. Use to inject malformed lines, unparseable bytes, or
	// hand-crafted envelopes that don't fit the structured ExtraEnvelopes
	// shape (e.g. truncated JSON like `{not json` for malformed-stream
	// tests). No newline is appended — entries should already include
	// `\n` if they're meant to terminate a line.
	RawLinesBeforeAssistant []string `json:"raw_lines_before_assistant,omitempty"`

	// ExtraEnvelopes are arbitrary JSON envelopes emitted between any
	// pre-assistant scripted output and the main assistant envelope. Each
	// is marshalled and written as one NDJSON line. Use to test foci's
	// tolerance for unknown envelope types: e.g.
	// {"type":"unknown_future_type","payload":{"k":"v"}}.
	ExtraEnvelopes []map[string]any `json:"extra_envelopes,omitempty"`

	// OmitContent, when true, suppresses the content array on the
	// assistant envelope (no text, no tool_uses). Foci should treat
	// the missing/empty content as a no-op assistant message and still
	// finalize the turn cleanly via the result envelope. Used to test
	// the empty-content tolerance path.
	OmitContent bool `json:"omit_content,omitempty"`

	// OmitSessionIDInResult, when true, drops the session_id field from
	// the result envelope. Foci should tolerate (warn, not crash) and
	// fall back to the session_id from the init envelope. Used to test
	// session-id resilience on the result path.
	OmitSessionIDInResult bool `json:"omit_session_id_in_result,omitempty"`

	// SleepMs, when > 0, sleeps the cc-stub for that many ms between
	// the user-message consumption and the first assistant envelope.
	// Distinct from CCSTUB_HANG_DURING_TURN (which sleeps AFTER the
	// assistant envelope): SleepMs delays the FIRST output of a turn,
	// matching the stall-detector test premise (init succeeded, no
	// progress past init).
	SleepMs int `json:"sleep_ms,omitempty"`

	// ControlCancelRequests is a list of permission request_ids the stub
	// should cancel after emitting permission_requests + result envelope.
	// Each entry produces one
	//   {"type":"control_cancel_request","request_id":"<id>"}
	// envelope, which foci's reader routes via OnControlCancelRequest →
	// CancelInteractiveMessage (disables the Telegram inline keyboard
	// for the matching prompt). Tests assert: a follow-up callback_query
	// on the cancelled prompt is a no-op (the keyboard reference was
	// cleared). The id should match one of the PermissionRequests above
	// — if it doesn't, foci logs a "no listener" debug and drops it.
	ControlCancelRequests []string `json:"control_cancel_requests,omitempty"`
}

// recorderEntry is one line written to the recorder file. Two event
// kinds are emitted:
//
//	Kind="invocation" — at process spawn; captures workdir / resume / flags.
//	Kind="user_message" — for each user message processed during the
//	  lifetime of the long-lived CC process; captures session_id +
//	  workdir + a short prefix of the text. This is the regression net
//	  for the cross-agent send_to_session bug: a turn that targets
//	  clutch's session MUST land in clutch's workdir.
//	Kind="permission_request" — for each can_use_tool control_request the
//	  stub emitted on a scripted user turn; captures the request_id so
//	  tests that didn't pre-specify one can still discover it.
//	Kind="control_response" — for each inbound control_response from foci
//	  (the answer to a can_use_tool request); captures the full inner
//	  payload (behavior / message / decisionClassification / updatedPermissions)
//	  so tests can assert on whatever they need.
//	Kind="init_system" — captures the system_prompt + appendSystemPrompt
//	  foci sent on the initialize control_request, recorded once per
//	  subprocess at handshake. Tests can assert system-prompt rebuilds
//	  after /reload by comparing PromptLen + PromptSHA256 across spawns.
//	Kind="bash_tool_use" — captures the Bash command + output + is_error
//	  for every Bash tool_use cc-stub executed inline. Tests use this to
//	  observe what `foci_*` shell functions returned (e.g. memory_search
//	  results, secret-template resolved values) without depending on
//	  foci's tool_result re-injection (which is CC-internal and not
//	  observable in foci's stdout).
//
// Tests read the JSONL file, group by kind, and assert structurally.
type recorderEntry struct {
	Kind      string `json:"kind"`
	Timestamp string `json:"ts"`
	Workdir   string `json:"workdir"`

	// invocation-only
	ResumeID string   `json:"resume_id,omitempty"`
	Model    string   `json:"model,omitempty"`
	Flags    []string `json:"flags,omitempty"`
	PID      int      `json:"pid,omitempty"`
	// Binary captures os.Args[0] — the path foci invoked cc-stub at.
	// Per-agent claude_binary override tests use this to distinguish a
	// spawn that landed at the global path from one that landed at the
	// per-agent override path. Empty in older recordings.
	Binary string `json:"binary,omitempty"`

	// user_message-only
	SessionID  string `json:"session_id,omitempty"`
	TextPrefix string `json:"text_prefix,omitempty"`
	// ContentBlockTypes captures every block type observed in the user
	// envelope's content array (e.g. ["text", "image"] for a photo with
	// caption). Empty when content was a flat string. Used by attachment
	// tests to assert foci forwarded a non-text block alongside the
	// caption.
	ContentBlockTypes []string `json:"content_block_types,omitempty"`

	// permission_request / control_response shared
	ControlRequestID string `json:"control_request_id,omitempty"`
	// permission_request only — the outbound shape the stub emitted, so a
	// test can also assert "what cc-stub asked foci to approve".
	OutboundToolName string `json:"outbound_tool_name,omitempty"`
	// control_response only — the inner foci payload (e.g. {"behavior":"allow",...}).
	// Stored as raw JSON so tests can pick fields without coupling to the stub's
	// view of the schema.
	ControlResponse json.RawMessage `json:"control_response,omitempty"`

	// init_system only — system prompt observed on the initialize
	// control_request. Stored as length + sha256-hex so tests can detect
	// changes without persisting the (possibly large) prompt verbatim.
	// Also keep a short head prefix (first 256 chars) for human-readable
	// diffing in failure messages.
	PromptLen    int    `json:"prompt_len,omitempty"`
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	PromptHead   string `json:"prompt_head,omitempty"`
	AppendLen    int    `json:"append_len,omitempty"`
	AppendSHA256 string `json:"append_sha256,omitempty"`

	// bash_tool_use only — Bash tool_use observability. ToolUseID,
	// BashCommand, and BashOutput let tests assert on what foci's exec
	// bridge returned (memory_search hits, secret template resolution,
	// etc.) without relying on foci's tool_result re-injection (which
	// happens internally in real CC and is not observable in stdout).
	// IsError mirrors the cc-stub's is_error decision (bash non-zero
	// exit).
	ToolUseID   string `json:"tool_use_id,omitempty"`
	BashCommand string `json:"bash_command,omitempty"`
	BashOutput  string `json:"bash_output,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
}

func main() {
	// Parse the flag surface foci passes. We ignore most values — the
	// stub doesn't care about content, only that flags are accepted.
	var (
		printMode       bool
		inputFormat     string
		outputFormat    string
		permTool        string
		includePartial  bool
		includeHook     bool
		verbose         bool
		model           string
		resume          string
		allowedTools    string
		settings        string
		appendSysPrompt string
		helpFlag        bool
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
			// CCSTUB_EXIT_CODE_ONCE_MARKER mirrors the HANG_ONCE_MARKER
			// pattern: if the path is set AND already exists, skip the
			// exit (subsequent spawns proceed normally); if absent,
			// touch it and exit with the configured code. Lets tests
			// script "first spawn dies before handshake; second spawn
			// recovers" without per-spawn env injection.
			marker := os.Getenv("CCSTUB_EXIT_CODE_ONCE_MARKER")
			skip := false
			if marker != "" {
				if _, err := os.Stat(marker); err == nil {
					skip = true
				} else {
					_ = os.WriteFile(marker, []byte("1"), 0o600)
				}
			}
			if !skip {
				os.Exit(n)
			}
		}
	}
	// recordInvocation BEFORE the FAIL_ON_RESUME exit so tests can
	// observe that a respawn with --resume <id> was attempted (rather
	// than infer it from the absence of a fresh invocation entry).
	recordInvocation(resume, model)

	if isTruthy(os.Getenv("CCSTUB_FAIL_ON_RESUME")) && resume != "" {
		// Simulates a CC that received --resume but couldn't find the
		// referenced session — exits non-zero, foci's delegated wrapper
		// retries without --resume.
		fmt.Fprintln(os.Stderr, "cc-stub: --resume id not found, exiting non-zero")
		os.Exit(1)
	}
	if hang := os.Getenv("CCSTUB_HANG"); hang != "" {
		if d, err := time.ParseDuration(hang); err == nil {
			// CCSTUB_HANG_ONCE_MARKER points at a file path. If the path
			// is set AND already exists, skip the hang — subsequent
			// spawns proceed normally. If the path is set and absent,
			// touch it and proceed with the hang. Lets tests script
			// "first spawn hangs past init deadline; second spawn does
			// not" without per-spawn env injection.
			marker := os.Getenv("CCSTUB_HANG_ONCE_MARKER")
			skip := false
			if marker != "" {
				if _, err := os.Stat(marker); err == nil {
					skip = true
				} else {
					_ = os.WriteFile(marker, []byte("1"), 0o600)
				}
			}
			if !skip {
				time.Sleep(d)
			}
		}
	}

	// Extract the install_id foci's prepareHooks wrote into --settings.
	// Empty when foci didn't install hooks (binary missing / dev build),
	// which is the natural opt-out for tests that need a no-hook path.
	hookInstallID := installIDFromSettings(settings)

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
	defer func() { _ = out.Flush() }()

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
			"type":       "control_response",
			"request_id": reqID,
			"response": map[string]any{
				"subtype":    "initialize_success",
				"session_id": sessionID,
			},
		})
		// Record system prompt at handshake. Tests use this to assert
		// the bootstrap was rebuilt from disk (e.g. /reload after a
		// workspace file edit produces a different prompt sha256).
		sp, asp := extractInitSystemPrompt(first)
		recordInitSystem(sp, asp)
	}
	emit(out, map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
		"model":      ifEmpty(model, "stub-model"),
		"tools":      []string{},
	})
	_ = out.Flush()

	// Now loop on user messages. For each, emit one assistant text block
	// (plus any scripted tool_use blocks), followed by a result message
	// to close the turn.
	//
	// Script is loaded PER user message — not once at startup — because
	// cc-stub is long-lived (survives across turns) and the test may
	// write a script after the stub spawned (e.g. fotini's first turn
	// is onboarding, the second turn is the scripted send_to_session).
	respText := os.Getenv("CCSTUB_RESPONSE")

	// Lifecycle env vars (all optional, all read once at the top of the
	// loop so a partial flag flip mid-process doesn't desync):
	//
	//   CCSTUB_EXIT_AFTER_ASSISTANT — non-empty → exit 0 between the
	//       assistant envelope and the result envelope on the first
	//       user message. Only fires on a fresh spawn (--resume empty),
	//       so foci's respawn-with-resume after the crash succeeds
	//       normally — that's the recovery path the lifecycle tests
	//       assert on. Without the --resume guard the respawn would
	//       crash identically and the test would loop.
	//   CCSTUB_EXIT_AFTER_N_TURNS=N — after N user_message envelopes
	//       have been fully processed, exit 0 cleanly between turns
	//       so foci respawns with --resume on the next inbound
	//       message. Used by lifecycle tests that need the per-session
	//       resume path to actually fire mid-test.
	//   CCSTUB_HANG_DURING_TURN=duration — sleep this long AFTER the
	//       assistant envelope and BEFORE the result envelope. Lets
	//       a test trigger /reset or send a message while a turn is
	//       intentionally in-flight (IsProcessing == true).
	//   CCSTUB_PANIC_ON_USER_MESSAGE=substring — if the user text
	//       contains the substring, write a Go-style "panic:"
	//       traceback to stderr and exit with code 2. Mimics a real
	//       CC subprocess crash.
	exitAfterAssistant := os.Getenv("CCSTUB_EXIT_AFTER_ASSISTANT") != "" && resume == ""
	exitAfterTurns := 0
	if v := os.Getenv("CCSTUB_EXIT_AFTER_N_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			exitAfterTurns = n
		}
	}
	var hangDuringTurn time.Duration
	if v := os.Getenv("CCSTUB_HANG_DURING_TURN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			hangDuringTurn = d
		}
	}
	// Panic-on-message also gates on resume so the respawn after the
	// crash recovers cleanly — same pattern as CCSTUB_EXIT_AFTER_ASSISTANT.
	panicOnMatch := ""
	if resume == "" {
		panicOnMatch = os.Getenv("CCSTUB_PANIC_ON_USER_MESSAGE")
	}
	// CCSTUB_EMIT_COMPACT_BOUNDARY: when truthy, a user message whose text
	// begins with "/compact" is handled as a compaction turn instead of a
	// normal assistant turn — the stub emits a system/status "compacting"
	// envelope (foci's onCompactionStart) followed by a system/compact_boundary
	// envelope (foci's onCompactionDone), then a result to close the turn.
	// This mirrors real CC's /compact handling and lets an L2 test exercise
	// the #828 Part B reload-on-compact bounce end-to-end. pre_tokens comes
	// from CCSTUB_COMPACT_PRE_TOKENS (default 50000).
	emitCompactBoundary := isTruthy(os.Getenv("CCSTUB_EMIT_COMPACT_BOUNDARY"))
	compactPreTokens := 50000
	if v := os.Getenv("CCSTUB_COMPACT_PRE_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			compactPreTokens = n
		}
	}
	turnCount := 0
	for in.Scan() {
		var env map[string]any
		if err := json.Unmarshal(in.Bytes(), &env); err != nil {
			continue
		}
		switch env["type"] {
		case "user":
			userText, blockTypes := extractUserContent(env)
			recordUserMessage(sessionID, userText, blockTypes)
			// CCSTUB_PANIC_ON_USER_MESSAGE: simulate a crashing CC.
			// Writes a Go-style panic preamble to stderr (foci's reader
			// surfaces a tail of stderr in the error log) before exiting
			// non-zero. Match is substring so tests can target a specific
			// message without coupling to the full payload.
			if panicOnMatch != "" && strings.Contains(userText, panicOnMatch) {
				fmt.Fprintf(os.Stderr, "panic: cc-stub forced crash on user message matching %q\n\ngoroutine 1 [running]:\nmain.main()\n\t/cc-stub/main.go:0 +0x0\n", panicOnMatch)
				os.Exit(2)
			}
			// CCSTUB_EMIT_COMPACT_BOUNDARY: a "/compact ..." user message is
			// CC's compaction trigger (foci injects it via SourceCompact). Emit
			// the same system envelopes real CC produces — status "compacting"
			// then compact_boundary — so foci's compaction waiters fire and the
			// #828 Part B reload bounce runs. No assistant text: real CC emits
			// none for /compact. The result envelope closes the turn cleanly.
			if emitCompactBoundary && strings.HasPrefix(strings.TrimSpace(userText), "/compact") {
				compacting := "compacting"
				emit(out, map[string]any{
					"type":       "system",
					"subtype":    "status",
					"status":     compacting,
					"session_id": sessionID,
				})
				_ = out.Flush()
				emit(out, map[string]any{
					"type":    "system",
					"subtype": "compact_boundary",
					"compact_metadata": map[string]any{
						"trigger":    "manual",
						"pre_tokens": compactPreTokens,
					},
					"session_id": sessionID,
				})
				emit(out, map[string]any{
					"type":       "result",
					"result":     "",
					"session_id": sessionID,
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				})
				_ = out.Flush()
				turnCount++
				continue
			}
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
			// Scripted raw-lines / extra envelopes / sleep land BEFORE
			// any other pre-assistant emission. RawLinesBeforeAssistant is
			// written verbatim (use for malformed JSON); ExtraEnvelopes is
			// marshalled per-line (use for unknown type fields). SleepMs
			// gates progress past init for stall-detector tests.
			if script != nil {
				for _, raw := range script.RawLinesBeforeAssistant {
					_, _ = out.WriteString(raw)
				}
				if len(script.RawLinesBeforeAssistant) > 0 {
					_ = out.Flush()
				}
				for _, env := range script.ExtraEnvelopes {
					emit(out, env)
				}
				if len(script.ExtraEnvelopes) > 0 {
					_ = out.Flush()
				}
				if script.SleepMs > 0 {
					time.Sleep(time.Duration(script.SleepMs) * time.Millisecond)
				}
			}
			// Pre-emit token-level stream_event deltas. Each becomes a
			// content_block_delta/text_delta envelope, driving foci's
			// OnStreamEvent → OnTextDelta → StreamWriter (the live-streaming
			// delivery path). Emitted before the assistant envelope, matching
			// real CC's ordering (deltas stream, then the final assistant
			// block carrying the complete text arrives).
			if script != nil {
				for _, d := range script.StreamDeltas {
					emit(out, map[string]any{
						"type": "stream_event",
						"event": map[string]any{
							"type": "content_block_delta",
							"delta": map[string]any{
								"type": "text_delta",
								"text": d,
							},
						},
						"session_id": sessionID,
					})
				}
				if len(script.StreamDeltas) > 0 {
					_ = out.Flush()
				}
			}
			// Pre-emit intermediate-text assistant messages. Each entry
			// becomes its own assistant envelope before the main one, all
			// within the same turn (no result envelope between them).
			// Matches the 2026-05-18 22:33 shape: multiple text blocks
			// within a single turn, observed in production as N separate
			// `ccstream OnAssistant: text_blocks=1` events.
			if script != nil {
				for _, t := range script.IntermediateTexts {
					emit(out, map[string]any{
						"type": "assistant",
						"message": map[string]any{
							"role": "assistant",
							"content": []map[string]any{
								{"type": "text", "text": t},
							},
						},
						"session_id": sessionID,
					})
				}
				if len(script.IntermediateTexts) > 0 {
					_ = out.Flush()
				}
			}
			content := []map[string]any{
				{"type": "text", "text": reply},
			}
			// Bash tool_uses are executed inline so the exec bridge fires
			// before the assistant envelope reaches foci. We buffer each
			// run's outcome so a system/hook_response can be emitted AFTER
			// the assistant message and BEFORE the result envelope — that
			// ordering matches real CC's per-tool hook dispatch and lets
			// ccstream.handleHookResponse fire OnToolEnd + PostToolNudgeFunc
			// while the turn is still open.
			type bashRun struct {
				toolUseID string
				toolName  string
				toolInput map[string]any
				result    bashResult
			}
			var bashRuns []bashRun
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
						res := runBashToolUse(tu.Input)
						bashRuns = append(bashRuns, bashRun{
							toolUseID: id,
							toolName:  tu.Name,
							toolInput: tu.Input,
							result:    res,
						})
						// Record so tests can assert on what foci's exec
						// bridge returned (memory_search hits, secret
						// resolution, etc.). Pull the literal command string
						// out of tu.Input for the recorder entry.
						cmd, _ := tu.Input["command"].(string)
						recordBashToolUse(id, cmd, res.Output, res.IsError)
					}
				}
			}
			assistantMsg := map[string]any{
				"role":    "assistant",
				"content": content,
			}
			// OmitContent: drop the content key entirely (foci should
			// treat as empty rather than panicking on a missing/null
			// content array).
			if script != nil && script.OmitContent {
				delete(assistantMsg, "content")
			}
			emit(out, map[string]any{
				"type":       "assistant",
				"message":    assistantMsg,
				"session_id": sessionID,
			})
			_ = out.Flush()
			// CCSTUB_EXIT_AFTER_ASSISTANT: exit 0 between assistant and
			// result envelopes on the first turn. Foci sees stdout EOF
			// mid-turn and reaps the subprocess; its watchdog fires the
			// "backend died mid-turn" recovery path.
			if exitAfterAssistant {
				os.Exit(0)
			}
			// CCSTUB_HANG_DURING_TURN: sleep AFTER the assistant envelope
			// has been flushed but BEFORE the result envelope. Foci's
			// IsProcessing flag stays true for the duration, so concurrent
			// /reset or steer messages can race the in-flight turn.
			if hangDuringTurn > 0 {
				time.Sleep(hangDuringTurn)
			}
			// Per-tool hook_response — one envelope per Bash run, carrying
			// the install_id foci embedded in --settings so the backend's
			// install-id filter dispatches OnToolEnd / PostToolNudgeFunc.
			// Skipped when foci installed no hooks (install_id empty).
			if hookInstallID != "" {
				for _, br := range bashRuns {
					emitHookResponse(out, hookInstallID, br.toolUseID, br.toolName, br.toolInput, br.result.Output, br.result.IsError)
				}
				if len(bashRuns) > 0 {
					_ = out.Flush()
				}
			}
			resultEnv := map[string]any{
				"type":   "result",
				"result": reply,
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			}
			// OmitSessionIDInResult: drop the session_id key on result
			// envelopes. Foci must tolerate (log+warn, not crash) and
			// fall back to the init envelope's session_id.
			if script == nil || !script.OmitSessionIDInResult {
				resultEnv["session_id"] = sessionID
			}
			emit(out, resultEnv)
			_ = out.Flush()
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
					_ = out.Flush()
				}
				// Emit any scripted control_cancel_requests. Foci's reader
				// routes each to OnControlCancelRequest →
				// CancelInteractiveMessage, which clears the inline
				// keyboard for the matching prompt. A subsequent
				// callback_query against that prompt is then a no-op
				// (HandleInteractiveCallback returns false because the
				// listener was unregistered).
				for _, cancelID := range script.ControlCancelRequests {
					emit(out, map[string]any{
						"type":       "control_cancel_request",
						"request_id": cancelID,
					})
					recordPermissionCancel(cancelID)
				}
				if len(script.ControlCancelRequests) > 0 {
					_ = out.Flush()
				}
			}
			// Late-text injection: emit a SECOND assistant message AFTER
			// result + permissions. Foci has already seen the result
			// envelope and torn down the per-turn sink; this assistant
			// arrives with no current sink registered and must route
			// through the session router's late-delivery fallback to
			// reach the platform. Simulates CC-harness-internal injections
			// (task-notifications, system reminders) that produce
			// assistant text outside any foci-side user message.
			if script != nil && script.LateText != "" {
				time.Sleep(200 * time.Millisecond)
				emit(out, map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": script.LateText},
						},
					},
					"session_id": sessionID,
				})
				_ = out.Flush()
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
			// CCSTUB_EXIT_AFTER_N_TURNS: bookend the turn loop so a
			// clean exit happens AFTER the result envelope has been
			// flushed. Foci processes the completion, persists the
			// session id, then sees stdin EOF when the next user msg
			// can't reach the dead process — DelegatedManager.Get
			// observes IsRunning()==false on the next inbound message
			// and respawns with --resume.
			turnCount++
			if exitAfterTurns > 0 && turnCount >= exitAfterTurns {
				_ = out.Flush()
				os.Exit(0)
			}
		case "control_request":
			// e.g. interrupt — ack with a control_response.
			if reqID, ok := env["request_id"].(string); ok {
				emit(out, map[string]any{
					"type":       "control_response",
					"request_id": reqID,
					"response":   map[string]any{"subtype": "ack"},
				})
				_ = out.Flush()
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

// extractInitSystemPrompt pulls the (systemPrompt, appendSystemPrompt)
// strings out of an initialize control_request envelope. Returns empty
// strings if the envelope isn't an initialize or the fields are absent.
func extractInitSystemPrompt(env map[string]any) (string, string) {
	if env["type"] != "control_request" {
		return "", ""
	}
	req, ok := env["request"].(map[string]any)
	if !ok {
		return "", ""
	}
	if req["subtype"] != "initialize" {
		return "", ""
	}
	sp, _ := req["systemPrompt"].(string)
	asp, _ := req["appendSystemPrompt"].(string)
	return sp, asp
}

// extractUserContent returns both the flattened text and the list of
// content block types observed. For flat-string content, blockTypes is
// nil. For structured content, every block's "type" field is captured.
// Tests use blockTypes to assert attachment presence (e.g. "image" for
// a photo, "document" for a PDF).
func extractUserContent(env map[string]any) (string, []string) {
	msg, ok := env["message"].(map[string]any)
	if !ok {
		return "", nil
	}
	switch v := msg["content"].(type) {
	case string:
		return v, nil
	case []any:
		var sb strings.Builder
		var types []string
		for _, b := range v {
			m, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, ok := m["type"].(string); ok {
				types = append(types, bt)
			}
			if t, ok := m["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String(), types
	}
	return "", nil
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
		Binary:    os.Args[0],
	})
}

// recordBashToolUse appends one JSONL line tagged kind="bash_tool_use"
// for every Bash command cc-stub ran inline. Captures the command and
// the combined-stdout-and-stderr output so tests can assert on what
// foci's exec bridge returned. Output is truncated to 4 KiB so the
// recorder file stays manageable when foci_memory_search returns a
// large hit list.
func recordBashToolUse(toolUseID, command, output string, isError bool) {
	wd, _ := os.Getwd()
	out := output
	if len(out) > 4096 {
		out = out[:4096]
	}
	cmd := command
	if len(cmd) > 1024 {
		cmd = cmd[:1024]
	}
	writeRecorder(recorderEntry{
		Kind:        "bash_tool_use",
		Timestamp:   time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:     wd,
		ToolUseID:   toolUseID,
		BashCommand: cmd,
		BashOutput:  out,
		IsError:     isError,
	})
}

// recordInitSystem appends one JSONL line tagged kind="init_system"
// capturing the system prompts foci sent on the initialize control_request.
// Prompts can be large (multi-KB character files); store length + sha256
// + a 256-char head for human-readable diffing.
func recordInitSystem(systemPrompt, appendSystemPrompt string) {
	wd, _ := os.Getwd()
	head := systemPrompt
	if len(head) > 256 {
		head = head[:256]
	}
	hSys := sha256.Sum256([]byte(systemPrompt))
	hApp := sha256.Sum256([]byte(appendSystemPrompt))
	writeRecorder(recorderEntry{
		Kind:         "init_system",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:      wd,
		PromptLen:    len(systemPrompt),
		PromptSHA256: hex.EncodeToString(hSys[:]),
		PromptHead:   head,
		AppendLen:    len(appendSystemPrompt),
		AppendSHA256: hex.EncodeToString(hApp[:]),
	})
}

// recordUserMessage appends one JSONL line tagged kind="user_message" so
// tests can assert which session+workdir handled the message. This is
// the per-turn signal the cross-agent regression test asserts on — the
// invocation-only recorder couldn't distinguish between turns inside a
// long-lived CC process.
//
// blockTypes is the list of content-block types observed in the user
// envelope (e.g. ["text", "image"] for a photo + caption). Empty for
// flat-string content; populated only when the user payload is structured.
func recordUserMessage(sessionID, text string, blockTypes []string) {
	wd, _ := os.Getwd()
	prefix := text
	// send_to_session prepends a ~600-char SYSTEM INJECTION context
	// note before the actual user payload; cap at 2k so tests can
	// match markers that live past that header.
	if len(prefix) > 2000 {
		prefix = prefix[:2000]
	}
	writeRecorder(recorderEntry{
		Kind:              "user_message",
		Timestamp:         time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:           wd,
		SessionID:         sessionID,
		TextPrefix:        prefix,
		ContentBlockTypes: blockTypes,
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

// recordPermissionCancel appends one JSONL line tagged
// kind="permission_cancel" so tests can pair each scripted cancel with
// the preceding permission_request entry and assert ordering. The id
// is the request_id of the cancelled permission.
func recordPermissionCancel(reqID string) {
	wd, _ := os.Getwd()
	writeRecorder(recorderEntry{
		Kind:             "permission_cancel",
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:          wd,
		ControlRequestID: reqID,
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
}

// bashResult is the captured outcome of one runBashToolUse invocation.
// Output is the combined stdout+stderr produced by the bash subshell so
// emitHookResponse can feed a faithful tool_response back to foci. The
// stub still tees output to its own stderr (via io.MultiWriter) so
// test-harness diagnostics keep working.
type bashResult struct {
	Output  string
	IsError bool
}

// runBashToolUse runs the "command" field of a Bash tool_use input as
// a non-interactive bash subshell, inheriting the stub's environment so
// BASH_ENV / FOCI_SOCK (set by foci-gw) take effect — that's how
// foci_send_to_session and the other shell-exported foci tools reach
// the exec bridge.
//
// Output is captured (combined stdout+stderr) and returned so the caller
// can synthesise a system/hook_response envelope matching what real CC
// emits after its internal tool execution (foci's PostToolNudgeFunc /
// OnToolEnd both consume that). Output is ALSO teed to the stub's
// stderr so existing tests that fished diagnostics out of the gateway
// stderr stream keep working.
//
// 10-second wall clock cap — enough for any exec-bridge round-trip,
// short enough that runaway scripts fail loud rather than hanging tests.
func runBashToolUse(input map[string]any) bashResult {
	cmd, _ := input["command"].(string)
	if cmd == "" {
		fmt.Fprintln(os.Stderr, "cc-stub: Bash tool_use with empty command — skipped")
		return bashResult{Output: "(no command)", IsError: true}
	}
	// cc-stub is a test double for Claude Code; it legitimately runs the
	// model-requested shell command directly. procx.Spawn is the production
	// secrets-dropping wrapper and must not wrap this test binary.
	c := exec.Command("bash", "-c", cmd) //nolint:forbidigo // test stub emulating CC's Bash tool
	var buf bytes.Buffer
	tee := io.MultiWriter(&buf, os.Stderr)
	c.Stdout = tee
	c.Stderr = tee
	c.Env = os.Environ()
	// Wall clock guard.
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub: Bash start failed: %v\n", err)
		return bashResult{Output: err.Error(), IsError: true}
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(os.Stderr, "cc-stub: Bash command exited: %v\n", err)
			return bashResult{Output: buf.String(), IsError: true}
		}
		return bashResult{Output: buf.String(), IsError: false}
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		fmt.Fprintln(os.Stderr, "cc-stub: Bash command timed out after 10s, killed")
		return bashResult{Output: buf.String() + "\n(timed out after 10s)", IsError: true}
	}
}

// installIDFromSettings parses the --settings JSON foci passes via argv
// to extract the install_id its prepareHooks generated. The settings
// blob's shape is:
//
//	{"hooks":{"PostToolUse":[{"matcher":"*","hooks":[
//	  {"type":"command","command":"\"<hookbin>\" --install <id>", ...}
//	]}], "PostToolUseFailure":[...]}}
//
// cc-stub needs the install_id to attach to every system/hook_response
// it synthesises — ccstream.handleHookResponse drops events whose
// install_id doesn't match the backend's recorded ID, so without this
// echo no nudge / OnToolEnd dispatch ever fires.
//
// Returns "" if settings is empty, unparseable, or carries no --install
// token. In that case the stub does NOT emit hook_response envelopes —
// tests that need foci's no-hook path get clean stdout, and tests that
// install hooks get fully-functional PostToolUse delivery.
func installIDFromSettings(settings string) string {
	if settings == "" {
		return ""
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		return ""
	}
	const marker = "--install "
	for _, matchers := range s.Hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				idx := strings.Index(h.Command, marker)
				if idx < 0 {
					continue
				}
				rest := strings.TrimSpace(h.Command[idx+len(marker):])
				if j := strings.IndexAny(rest, " \t"); j >= 0 {
					rest = rest[:j]
				}
				if rest != "" {
					return rest
				}
			}
		}
	}
	return ""
}

// emitHookResponse writes one system/hook_response NDJSON line matching
// what real CC emits after running a tool. Outer envelope carries
// hook_event + stdout (the hook script's verbatim stdout bytes) +
// exit_code + outcome; the stdout field carries a JSON-encoded inner
// hookScriptOutput that must include install_id (so the backend's
// install-id filter accepts it) and tool_use_id (so per-tool tracking
// dispatches correctly).
//
// installID — must equal what foci passed via --settings; if empty,
// caller skips this emission.
// isError — selects hook_event: PostToolUse vs PostToolUseFailure.
// Foci's handler routes both to OnToolEnd, but failures additionally
// populate the inner `error` field so the agent transcript shows the
// failure body, not the (often empty) tool_response.
func emitHookResponse(w *bufio.Writer, installID, toolUseID, toolName string, toolInput map[string]any, output string, isError bool) {
	hookEvent := "PostToolUse"
	if isError {
		hookEvent = "PostToolUseFailure"
	}
	inputJSON, _ := json.Marshal(toolInput)
	inner := map[string]any{
		"hook_event":  hookEvent,
		"install_id":  installID,
		"tool_use_id": toolUseID,
		"tool_name":   toolName,
		"tool_input":  string(inputJSON),
		"is_error":    isError,
	}
	if isError {
		inner["error"] = output
	} else {
		inner["tool_response"] = output
	}
	innerJSON, _ := json.Marshal(inner)
	emit(w, map[string]any{
		"type":       "system",
		"subtype":    "hook_response",
		"hook_event": hookEvent,
		"stdout":     string(innerJSON),
		"exit_code":  0,
		"outcome":    "ok",
	})
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
  CCSTUB_EMIT_COMPACT_BOUNDARY  if truthy, "/compact" emits status+compact_boundary
  CCSTUB_COMPACT_PRE_TOKENS     pre_tokens in compact_boundary (default 50000)
  CCSTUB_RESPONSE       assistant reply text (default echoes user prompt)

Flags accepted (most ignored — stub only cares about --resume, --model):
  --print --input-format --output-format --permission-prompt-tool
  --include-partial-messages --include-hook-events --verbose
  --model --resume --allowedTools --settings --append-system-prompt
  -h / --help

Foci points at this binary by setting [cc_backend].claude_binary in
foci.toml. See cmd/cc-stub/main.go for the full protocol.`
