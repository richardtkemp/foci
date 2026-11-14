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
//	CCSTUB_WRITE_TRANSCRIPT — if truthy, every user message the main loop
//	                        consumes is appended to the session transcript where
//	                        real CC keeps it (appendTranscript: under
//	                        $CLAUDE_CONFIG_DIR, else ~/.claude), as real CC does
//	                        for every turn. Off by default: tests written before
//	                        it rely on the stub keeping no transcript.
//	CCSTUB_HANG           — duration to sleep before the handshake (e.g. "5s")
//	CCSTUB_EMIT_COMPACT_BOUNDARY — if truthy, a "/compact ..." user message is
//	                        handled as a compaction turn: emit system/status
//	                        "compacting" then system/compact_boundary then a
//	                        result (no assistant text), mirroring real CC's
//	                        /compact. Drives foci's compaction start/done waits and
//	                        the #828 Part B reload-on-compact bounce.
//	CCSTUB_COMPACT_PRE_TOKENS — pre_tokens reported in compact_boundary
//	                        (default 50000); only meaningful with the above.
//	CCSTUB_RESPONSE       — assistant reply text; default echoes the user prompt
//	CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS — read as real CC reads it: when
//	                        truthy (foci sets it at every launch), each run is
//	                        bracketed by system/session_state_changed "running"
//	                        and "idle", so ccstream completes turns on idle and
//	                        adopts a run it did not open (production's path,
//	                        #3055). A per-agent backend_config.env setting it to
//	                        "0" runs the legacy complete-on-result path instead.
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
//	                          • match               — optional substring; when
//	                                                  set, the script applies
//	                                                  only to a user message
//	                                                  containing it and every
//	                                                  other turn leaves the
//	                                                  file in place.
//	                        Script is consumed one-shot — the file is claimed
//	                        (renamed away) at the moment a turn loads it, so a
//	                        script the test writes AFTER that point survives.
//	                        The claim happens before the turn's user_message
//	                        is recorded, so seeing that record means the
//	                        turn's script is already taken (#2681).
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Match binds the script to one turn: when non-empty, only a user
	// message whose text contains it loads (and consumes) the script; any
	// other turn — a keepalive, reflection, or a message the test did not
	// aim at — runs with defaults and leaves the file for its target.
	// Without it a script goes to whichever turn reaches the file first,
	// which makes "hold THIS turn open" setups order-dependent (#2079).
	Match              string                      `json:"match,omitempty"`
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

	// CrashOnUnconsumed models CC dying with a mid-turn message still
	// unconsumed (foci_todo #2050). When set, the turn this script loads
	// holds open after its assistant envelope — no result — and keeps
	// reading stdin; the first user message containing this substring is
	// recorded as kind "unconsumed_user_message" (NOT "user_message": it
	// was never folded into the conversation) and the stub exits 1 without
	// acting on it. The crash is keyed on the message's arrival, not on a
	// timer, so the write is guaranteed to have reached the process that
	// dies. A resumed process (--resume) never sees the message again unless
	// foci re-sends it.
	CrashOnUnconsumed string `json:"crash_on_unconsumed,omitempty"`

	// CrashAfterConsuming models CC dying after folding a message into its
	// conversation but before acknowledging it (foci_todo #2050). Like
	// CrashOnUnconsumed the turn holds open after its assistant envelope; the
	// first user message containing this substring is recorded as a consumed
	// "user_message", appended to the session transcript as
	// {"type":"user","uuid":<its uuid>} — the record real CC writes before the
	// API request — and the stub exits 1 WITHOUT emitting any
	// user_message_uuids ack. foci must find it in the transcript and not
	// redeliver it.
	CrashAfterConsuming string `json:"crash_after_consuming,omitempty"`

	// HoldAfterConsuming is CrashAfterConsuming without the crash: the first
	// user message containing this substring is recorded as a consumed
	// "user_message" and appended to the session transcript, with no ack, and
	// the turn then stays open (later user lines are recorded as
	// "held_user_message") until stdin closes. It models a CC that folded a
	// steer in and is still working when foci shuts down, so the next foci
	// process must find the steer in the transcript and not resend it (#3104).
	HoldAfterConsuming string `json:"hold_after_consuming,omitempty"`
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
//	Kind="argv_error" — the stub rejected its argv (an undeclared flag or an
//	  unexpected argument, #2925) and exited 1; captures flags + error.
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
	// argv_error only — why parseArgs rejected Flags.
	Error string `json:"error,omitempty"`

	// interactive_login only (#3104): the login pane's CLAUDE_CONFIG_DIR
	// and CLAUDE_SECURESTORAGE_CONFIG_DIR ("<unset>" when absent).
	ConfigDir     string `json:"config_dir,omitempty"`
	SecureStorage string `json:"secure_storage,omitempty"`

	// user_message-only
	SessionID  string `json:"session_id,omitempty"`
	TextPrefix string `json:"text_prefix,omitempty"`
	// ContentBlockTypes captures every block type observed in the user
	// envelope's content array (e.g. ["text", "image"] for a photo with
	// caption). Empty when content was a flat string. Used by attachment
	// tests to assert foci forwarded a non-text block alongside the
	// caption.
	ContentBlockTypes []string `json:"content_block_types,omitempty"`
	// Folded marks a user message that arrived while a run was open and
	// was answered inside that run, as real CC folds a steer (#3056).
	Folded bool `json:"folded,omitempty"`

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
	a, err := parseArgs(os.Args[1:])
	if err != nil {
		// Real CC rejects an argument it does not know ("error: unknown option
		// '--x'", exit 1; probed on CC 2.1.314, #2925), and so does the stub.
		// Go's flag package stops at the first unknown flag, so warning and
		// carrying on (the old behaviour) silently dropped every flag after
		// it: a44c628aa put --prompt-suggestions before --resume and
		// --settings, and L2 failed as session and nudge bugs. Record the
		// error too, so a test's recorder tail names the cause.
		fmt.Fprintf(os.Stderr, "cc-stub: %v\ncc-stub: argv: %q\ncc-stub: %s\n", err, os.Args[1:], argvErrorHint)
		recordArgvError(err)
		os.Exit(1)
	}
	if a.help {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(0)
	}
	if a.authStatus {
		// `claude auth status` — foci's startup readiness probe
		// (ccstream/readiness.go). The answer is per config folder
		// (stubAuth): signed in unless the folder's state says otherwise.
		// Like real CC, the exit code says whether it is signed in and the
		// JSON body is printed either way.
		st := loadStubAuth()
		fmt.Println(st.statusJSON())
		if !st.loggedIn() {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 1 && stdinIsTerminal() {
		// An interactive `claude` with no flags on a terminal: the
		// relogin pane foci starts in tmux (internal/relogin). Emulate
		// its /login screens for this config folder.
		interactiveLogin()
		return
	}
	resume, model, settings := a.resume, a.model, a.settings
	skipPermissions, permissionMode := a.skipPermissions, a.permissionMode

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
	// Answer it in real CC's shape, which carries no session_id: real CC
	// sends no system/init at the handshake, fresh or resumed (probed on CC
	// 2.1.314, #3056). foci's WaitReady unblocks on this response, and it
	// learns the session id from the first run's init.
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
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": reqID,
				"response":   map[string]any{"pid": os.Getpid()},
			},
		})
		// Record system prompt at handshake. Tests use this to assert
		// the bootstrap was rebuilt from disk (e.g. /reload after a
		// workspace file edit produces a different prompt sha256).
		sp, asp := extractInitSystemPrompt(first)
		recordInitSystem(sp, asp)
	}
	_ = out.Flush()
	// Real CC emits system/init at the start of every run, after "running"
	// (and again for each message folded into an open run).
	emitInit := func() {
		m := map[string]any{
			"type":       "system",
			"subtype":    "init",
			"session_id": sessionID,
			"model":      ifEmpty(model, "stub-model"),
			"tools":      []string{},
		}
		if ackUUID != "" {
			m["user_message_uuids"] = []string{ackUUID}
		}
		emit(out, m)
	}
	pump := newStdinPump(in)

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
	// envelope (foci's compaction-start wait) followed by a system/compact_boundary
	// envelope (foci's compaction-done wait), then a result to close the turn.
	// This mirrors real CC's /compact handling and lets an L2 test exercise
	// the #828 Part B reload-on-compact bounce end-to-end. pre_tokens comes
	// from CCSTUB_COMPACT_PRE_TOKENS (default 50000).
	writeTranscript := isTruthy(os.Getenv("CCSTUB_WRITE_TRANSCRIPT"))
	emitCompactBoundary := isTruthy(os.Getenv("CCSTUB_EMIT_COMPACT_BOUNDARY"))
	compactPreTokens := 50000
	if v := os.Getenv("CCSTUB_COMPACT_PRE_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			compactPreTokens = n
		}
	}
	turnCount := 0
	// inRun is true while a run stays open for a folded user message: that
	// message is answered without a fresh "running" (#3056).
	inRun := false
	// lateText is a script's late_text, sent as its own run after the idle
	// that closes the run whose script carried it.
	lateText := ""
	for {
		line, ok := pump.next()
		if !ok {
			break
		}
		var env map[string]any
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env["type"] {
		case "user":
			// Real CC acknowledges consuming a stdin message by naming its
			// uuid in user_message_uuids on the API response's records;
			// emit stamps it on this message's assistant/result envelopes.
			ackUUID, _ = env["uuid"].(string)
			userText, blockTypes := extractUserContent(env)
			// A batch session (launched with --permission-mode auto) or a
			// skip-permissions session is a foci BATCH run (nudge
			// extraction, consolidation, foci_summary — #1962); no harness
			// agent sets skip_permissions. It shares the agent's workdir, so
			// it must not consume the per-workdir script a test wrote for
			// the agent's own next turn, and it answers "[]" — a well-formed
			// empty result for nudge extraction (zero rules), ignored by the
			// other batch callers — the way a compliant model would answer
			// a structured request, rather than echoing the prompt back.
			batch := skipPermissions || permissionMode == "auto"
			isCompact := emitCompactBoundary && strings.HasPrefix(strings.TrimSpace(userText), "/compact")
			willPanic := panicOnMatch != "" && strings.Contains(userText, panicOnMatch)
			// Claim this turn's script BEFORE recording its user_message
			// (#2681). Tests treat "user_message recorded" as "this turn
			// has taken its script" and write the next turn's script at
			// once; recording first left a window where that write
			// overwrote the script this turn had not yet claimed, so the
			// turn ran the NEXT turn's script. Compaction and forced-panic
			// turns never load a script, as before.
			var script *stubScript
			if !batch && !isCompact && !willPanic {
				script = loadScript(userText)
			}
			recordUserMessage(sessionID, userText, blockTypes, inRun)
			// CCSTUB_PANIC_ON_USER_MESSAGE: simulate a crashing CC.
			// Writes a Go-style panic preamble to stderr (foci's reader
			// surfaces a tail of stderr in the error log) before exiting
			// non-zero. Match is substring so tests can target a specific
			// message without coupling to the full payload.
			if willPanic {
				fmt.Fprintf(os.Stderr, "panic: cc-stub forced crash on user message matching %q\n\ngoroutine 1 [running]:\nmain.main()\n\t/cc-stub/main.go:0 +0x0\n", panicOnMatch)
				os.Exit(2)
			}
			// CCSTUB_WRITE_TRANSCRIPT: CC appends each message it consumes
			// to the session transcript, the file foci's session index row
			// names and its restart checks read (#3104).
			if writeTranscript {
				if err := appendTranscript(sessionID, map[string]any{"type": "user", "uuid": ackUUID, "sessionId": sessionID}); err != nil {
					fmt.Fprintf(os.Stderr, "cc-stub: transcript write failed: %v\n", err)
				}
			}
			// A user message starts a run. Real CC emits "running" first,
			// before even the run's system/init (probed on CC 2.1.314, #3055).
			// A message folded into an open run gets no "running".
			if !inRun {
				emitSessionState(out, sessionID, "running")
			}
			inRun = true
			// CCSTUB_EMIT_COMPACT_BOUNDARY: a "/compact ..." user message is
			// CC's compaction trigger (foci injects it via SourceCompact). Emit
			// the same system envelopes real CC produces — status "compacting"
			// then compact_boundary — so foci's compaction waiters fire and the
			// #828 Part B reload bounce runs. No assistant text: real CC emits
			// none for /compact. The result envelope closes the turn cleanly.
			if isCompact {
				compacting := "compacting"
				emit(out, map[string]any{
					"type":       "system",
					"subtype":    "status",
					"status":     compacting,
					"session_id": sessionID,
				})
				// Real CC: status compacting, then init, then the boundary.
				emitInit()
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
				turnCount++
				if pump.takeFolded() {
					_ = out.Flush()
					continue
				}
				emitSessionState(out, sessionID, "idle")
				inRun = false
				pump.endRun()
				_ = out.Flush()
				continue
			}
			// The config folder's login is gone (stubAuth): CC answers
			// every message with its synthetic "Not logged in" reply,
			// flagged authentication_failed, and an error result (#3104).
			if !loadStubAuth().loggedIn() {
				emitInit()
				emit(out, map[string]any{
					"type":  "assistant",
					"error": "authentication_failed",
					"message": map[string]any{
						"id":      "msg_stub_auth",
						"type":    "message",
						"role":    "assistant",
						"model":   "<synthetic>",
						"content": []map[string]any{{"type": "text", "text": stubNotLoggedIn}},
					},
					"session_id": sessionID,
				})
				emit(out, map[string]any{
					"type":       "result",
					"subtype":    "success",
					"is_error":   true,
					"result":     stubNotLoggedIn,
					"session_id": sessionID,
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				})
				turnCount++
				if pump.takeFolded() {
					_ = out.Flush()
					continue
				}
				emitSessionState(out, sessionID, "idle")
				inRun = false
				pump.endRun()
				_ = out.Flush()
				continue
			}
			emitInit()
			reply := respText
			if batch && reply == "" {
				reply = "[]"
			}
			if script != nil && script.Text != "" {
				reply = script.Text
			}
			if reply == "" {
				// Default: echo back so tests can assert on round-trip
				// without needing to set the env var.
				reply = "stub-reply: " + userText
			}
			// Faithful keepalive compliance. The cache-keepalive ping
			// (shared/prompts/keepalive.md, carrying the "[KEEPALIVE]"
			// marker) instructs the agent to "Respond with `[[NO_RESPONSE]]`
			// and nothing else" — an UNCONDITIONAL silence directive (unlike
			// the proactive-injection note, which offers the agent a choice
			// and must NOT be silenced here). A real, compliant agent emits
			// exactly the silencing sentinel, which foci strips to "" on every
			// delivery path (in-turn sink AND late-delivery fallback), so a
			// keepalive never produces a user-visible reply. The stub's
			// default echo instead spills the prompt back as a NON-silent
			// reply; with the delegated backend that reply races the turn's
			// (silent) sink setup and, when it loses, leaks to the chat via
			// the late-delivery path — a contention-sensitive flake that no
			// real agent would ever cause. Mirror the real agent's compliance
			// so the stub is faithful and keepalive is deterministically
			// silent. An explicit scripted reply (script.Text) still wins, so
			// a test that deliberately scripts a keepalive-turn reply is
			// unaffected.
			if (script == nil || script.Text == "") && strings.Contains(userText, "[KEEPALIVE]") {
				reply = "[[NO_RESPONSE]]"
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
					pump.sleep(time.Duration(script.SleepMs) * time.Millisecond)
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
			if script != nil && script.CrashOnUnconsumed != "" {
				crashOnUnconsumed(pump, script.CrashOnUnconsumed)
			}
			if script != nil && script.CrashAfterConsuming != "" {
				crashAfterConsuming(pump, script.CrashAfterConsuming, sessionID)
			}
			if script != nil && script.HoldAfterConsuming != "" {
				holdAfterConsuming(pump, script.HoldAfterConsuming, sessionID)
			}
			// CCSTUB_EXIT_AFTER_ASSISTANT: exit 0 between assistant and
			// result envelopes on the first turn. Foci sees stdout EOF
			// mid-turn and reaps the subprocess; its watchdog fires the
			// "backend died mid-turn" recovery path.
			if exitAfterAssistant {
				os.Exit(0)
			}
			// Scripted permission requests come inside the run, after the
			// assistant message, as real CC asks while a tool_use waits:
			// "requires_action", the can_use_tool control_request(s), then
			// "running" once foci has answered (probed on CC 2.1.314,
			// #3056). The run stays open until every request not cancelled
			// below is answered; a request nobody answers holds the run
			// open, as it holds real CC's. Foci processes them on its
			// reader goroutine and dispatches each to permPromptFn — which
			// routes through SendInteractiveMessageWithID, so each
			// request_id becomes a registered Telegram prompt that
			// PushCallbackQuery can resolve. Multiple entries in one script
			// land as multiple concurrent pending permissions (testing the
			// queue/concurrency assertions in N1).
			if script != nil && (len(script.PermissionRequests) > 0 || len(script.ControlCancelRequests) > 0) {
				var waitFor []string
				if len(script.PermissionRequests) > 0 {
					emitSessionState(out, sessionID, "requires_action")
				}
				cancelled := map[string]bool{}
				for _, id := range script.ControlCancelRequests {
					cancelled[id] = true
				}
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
					if !cancelled[reqID] {
						waitFor = append(waitFor, reqID)
					}
				}
				_ = out.Flush()
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
				_ = out.Flush()
				if !pump.waitAnswered(waitFor) {
					// foci closed stdin with a request unanswered: the
					// process is being torn down.
					_ = out.Flush()
					return
				}
				if len(script.PermissionRequests) > 0 {
					emitSessionState(out, sessionID, "running")
					_ = out.Flush()
				}
			}
			// CCSTUB_HANG_DURING_TURN: sleep AFTER the assistant envelope
			// has been flushed but BEFORE the result envelope. Foci's
			// IsProcessing flag stays true for the duration, so concurrent
			// /reset or steer messages can race the in-flight turn. A user
			// message foci writes meanwhile is folded into this run.
			if hangDuringTurn > 0 {
				pump.sleep(hangDuringTurn)
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
			// A real CC call always reads or writes the prompt cache, and
			// since #2509 foci treats only cache tokens > 0 as evidence that
			// a session is warm (last_cache_touch). Report them, or no stub
			// session ever enters keepalive's warm window (#2834).
			resultEnv := map[string]any{
				"type":   "result",
				"result": reply,
				"usage": map[string]any{
					"input_tokens":            0,
					"output_tokens":           0,
					"cache_read_input_tokens": stubCacheReadTokens,
				},
			}
			// OmitSessionIDInResult: drop the session_id key on result
			// envelopes. Foci must tolerate (log+warn, not crash) and
			// fall back to the init envelope's session_id.
			if script == nil || !script.OmitSessionIDInResult {
				resultEnv["session_id"] = sessionID
			}
			emit(out, resultEnv)
			if script != nil && script.LateText != "" {
				lateText = script.LateText
			}
			turnCount++
			// A user message foci wrote while this run was open is answered
			// next, inside the same run: real CC sends this result, then
			// init, assistant and result for the folded message, then ONE
			// idle (probed on CC 2.1.314 with a text-only turn, #3056).
			if pump.takeFolded() {
				_ = out.Flush()
				continue
			}
			// The run ends: real CC emits "idle" right after the result; it
			// is ccstream's turn-completion signal. Late text below comes
			// after it, outside the run.
			emitSessionState(out, sessionID, "idle")
			inRun = false
			pump.endRun()
			_ = out.Flush()
			// Late-text injection: emit a SECOND assistant message AFTER
			// the run's result. Foci has already seen the result envelope
			// and torn down the per-turn sink; this assistant arrives with
			// no current sink registered and must route through the session
			// router's late-delivery fallback to reach the platform.
			// Simulates CC-harness-internal injections (task-notifications,
			// system reminders) that produce assistant text outside any
			// foci-side user message.
			//
			// With session-state events on, the late text is what real CC
			// sends for such an injection: a run of its own (running, init,
			// assistant, result, idle; init carries no user_message_uuids,
			// probed on CC 2.1.314 with a background task's notification,
			// #3056), which ccstream adopts as an autonomous turn with its
			// own sink.
			if lateText != "" {
				text := lateText
				lateText = ""
				time.Sleep(200 * time.Millisecond)
				ackUUID = ""
				emitSessionState(out, sessionID, "running")
				emitInit()
				emit(out, map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": text},
						},
					},
					"session_id": sessionID,
				})
				if sessionStateEvents {
					emit(out, map[string]any{
						"type":       "result",
						"result":     text,
						"session_id": sessionID,
						"usage": map[string]any{
							"input_tokens":            0,
							"output_tokens":           0,
							"cache_read_input_tokens": stubCacheReadTokens,
						},
					})
				}
				emitSessionState(out, sessionID, "idle")
				_ = out.Flush()
			}
			// CCSTUB_EXIT_AFTER_N_TURNS: bookend the turn loop so a
			// clean exit happens AFTER the run's idle has been
			// flushed. Foci processes the completion, persists the
			// session id, and marks the backend dead when it reaps the
			// process — DelegatedManager.Get then observes
			// IsRunning()==false on the next inbound message and
			// respawns with --resume. Anything foci wrote to stdin
			// before that (a turn begun just before the exit) is never
			// read: tests must wait for foci to observe the exit before
			// sending the next message (waitForStubExitObserved,
			// foci_todo #2044). A message folded into the run counts as a
			// turn of its own.
			if exitAfterTurns > 0 && turnCount >= exitAfterTurns {
				_ = out.Flush()
				os.Exit(0)
			}
		case "control_request":
			// Dispatch by request subtype. The stub must answer each
			// control_request foci sends or foci blocks on its response.
			reqID, _ := env["request_id"].(string)
			reqSubtype := ""
			if req, ok := env["request"].(map[string]any); ok {
				reqSubtype, _ = req["subtype"].(string)
			}
			switch reqSubtype {
			case "get_context_usage":
				// foci's refreshContextFromBackend (post-turn hook) asks for
				// the context window once per session. If the stub ignores it,
				// GetContextUsage blocks for its full 5s timeout — and because
				// a failed refresh caches nothing, that timeout repeats every
				// turn, inflating in-flight latency and starving gates that
				// fire shortly after a turn (e.g. the reflection pass). Answer
				// with a canned large window so the limit is cached after the
				// first call and subsequent turns skip the refresh entirely.
				emit(out, map[string]any{
					"type":       "control_response",
					"request_id": reqID,
					"response": map[string]any{
						"subtype":    "success",
						"request_id": reqID,
						"response": map[string]any{
							"totalTokens": 1000,
							"maxTokens":   200000,
							"percentage":  0,
							"model":       "stub",
						},
					},
				})
			default:
				// e.g. interrupt — ack with a control_response.
				if reqID != "" {
					emit(out, map[string]any{
						"type":       "control_response",
						"request_id": reqID,
						"response":   map[string]any{"subtype": "ack"},
					})
				}
			}
			_ = out.Flush()
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

// crashOnUnconsumed implements stubScript.CrashOnUnconsumed: with the turn
// held open, read stdin until a user message containing marker arrives,
// record it as unconsumed, and exit 1. Other user lines are recorded as
// "held_user_message" and otherwise ignored — the turn is deliberately stuck,
// as a real CC is mid-tool when a steer lands. If stdin
// closes first the stub exits 0, as it would at the end of the main loop.
func crashOnUnconsumed(p *stdinPump, marker string) {
	for {
		line, ok := p.raw()
		if !ok {
			break
		}
		var env map[string]any
		if err := json.Unmarshal(line, &env); err != nil || env["type"] != "user" {
			continue
		}
		text, _ := extractUserContent(env)
		wd, _ := os.Getwd()
		if !strings.Contains(text, marker) {
			// Read but held: the turn is stuck, so CC never folds it.
			writeRecorder(recorderEntry{
				Kind:       "held_user_message",
				Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
				Workdir:    wd,
				TextPrefix: text,
			})
			continue
		}
		writeRecorder(recorderEntry{
			Kind:       "unconsumed_user_message",
			Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
			Workdir:    wd,
			TextPrefix: text,
		})
		fmt.Fprintf(os.Stderr, "cc-stub: dying with unconsumed user message matching %q\n", marker)
		os.Exit(1)
	}
	os.Exit(0)
}

// crashAfterConsuming implements stubScript.CrashAfterConsuming: with the turn
// held open, read stdin until a user message containing marker arrives, record
// it as consumed, write its transcript record, and exit 1 before any ack.
func crashAfterConsuming(p *stdinPump, marker, sessionID string) {
	if consumeMarked(p, marker, sessionID) {
		fmt.Fprintf(os.Stderr, "cc-stub: dying after consuming user message matching %q (no ack)\n", marker)
		os.Exit(1)
	}
	os.Exit(0)
}

// holdAfterConsuming implements stubScript.HoldAfterConsuming: consume the
// marked message as crashAfterConsuming does, then keep the turn open, reading
// (and holding) user lines until stdin closes.
func holdAfterConsuming(p *stdinPump, marker, sessionID string) {
	if consumeMarked(p, marker, sessionID) {
		fmt.Fprintf(os.Stderr, "cc-stub: consumed user message matching %q (no ack); holding the turn open\n", marker)
		for {
			line, ok := p.raw()
			if !ok {
				break
			}
			var env map[string]any
			if err := json.Unmarshal(line, &env); err != nil || env["type"] != "user" {
				continue
			}
			text, _ := extractUserContent(env)
			wd, _ := os.Getwd()
			writeRecorder(recorderEntry{
				Kind:       "held_user_message",
				Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
				Workdir:    wd,
				TextPrefix: text,
			})
		}
	}
	os.Exit(0)
}

// consumeMarked reads stdin until a user message containing marker arrives,
// records it as a consumed user_message and appends its transcript record (the
// one real CC writes before the API request). False when stdin closes first.
func consumeMarked(p *stdinPump, marker, sessionID string) bool {
	for {
		line, ok := p.raw()
		if !ok {
			return false
		}
		var env map[string]any
		if err := json.Unmarshal(line, &env); err != nil || env["type"] != "user" {
			continue
		}
		text, blockTypes := extractUserContent(env)
		if !strings.Contains(text, marker) {
			continue
		}
		recordUserMessage(sessionID, text, blockTypes, false)
		id, _ := env["uuid"].(string)
		if err := appendTranscript(sessionID, map[string]any{"type": "user", "uuid": id, "sessionId": sessionID}); err != nil {
			fmt.Fprintf(os.Stderr, "cc-stub: transcript write failed: %v\n", err)
		}
		return true
	}
}

// stubConfigDir is the config folder real CC uses: $CLAUDE_CONFIG_DIR, else
// ~/.claude.
func stubConfigDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// appendTranscript appends rec to the session transcript at the path real CC
// uses — <config dir>/projects/<workdir, every non-alphanumeric → "-">/<sessionID>.jsonl,
// the config dir being $CLAUDE_CONFIG_DIR, else ~/.claude — which foci reads to
// tell a folded message from a lost one (#2050).
func appendTranscript(sessionID string, rec map[string]any) error {
	cfgDir, err := stubConfigDir()
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := filepath.Join(cfgDir, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(wd, "-"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionID+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	b, _ := json.Marshal(rec)
	_, err = f.Write(append(b, '\n'))
	return err
}

// stubCacheReadTokens is the cache_read_input_tokens every normal turn's
// result reports. Constant, so a keepalive fork never reads less than its
// source and trips the #2509 fork-shortfall warning.
const stubCacheReadTokens = 1000

// ackUUID is the uuid of the user message the stub is answering; emit stamps
// it as user_message_uuids on assistant and result envelopes, as real CC does
// on the records of the API response that consumed it (#2050).
var ackUUID string

// sessionStateEvents mirrors real CC's opt-in: CC emits
// system/session_state_changed only when CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS
// is truthy (a bool env in CC 2.1.314, same truthy set as isTruthy).
var sessionStateEvents = isTruthy(os.Getenv("CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"))

// emitSessionState emits CC's run-boundary event when the opt-in is on. Shape
// copied from a live CC 2.1.314 stream (#3055):
// {"type":"system","subtype":"session_state_changed","state":"running","uuid":…,"session_id":…}.
// Real CC also emits "requires_action" while a permission prompt waits; the
// stub's permission requests come after the run's idle, so it never does.
func emitSessionState(w *bufio.Writer, sessionID, state string) {
	if !sessionStateEvents {
		return
	}
	emit(w, map[string]any{
		"type":       "system",
		"subtype":    "session_state_changed",
		"state":      state,
		"uuid":       fmt.Sprintf("stub-state-%d", time.Now().UnixNano()),
		"session_id": sessionID,
	})
}

// emit writes one NDJSON line. Errors are silent because the stub is
// best-effort — if foci has already closed stdout, there's nowhere to log.
func emit(w *bufio.Writer, v any) {
	if m, ok := v.(map[string]any); ok && ackUUID != "" && (m["type"] == "assistant" || m["type"] == "result") {
		m["user_message_uuids"] = []string{ackUUID}
	}
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

// argvErrorHint ends the stderr report of a rejected argv.
const argvErrorHint = "a new foci flag must be declared in parseArgs (cmd/cc-stub/main.go)"

// authStatusJSON is the stub's answer to `claude auth status` for a folder
// with no stub auth state.
const authStatusJSON = `{"loggedIn":true,"authMethod":"stub"}`

// stubNotLoggedIn is CC's reply to a message when its login is gone.
const stubNotLoggedIn = "Not logged in · Please run /login"

// stubAuthFile is the file in a config folder that holds the stub's login
// state for that folder (#3104): a test writes {"loggedIn":false} to log one
// account out, and an "email" and "loginExpiresAt" (RFC 3339) for what
// `claude auth status` reports. No file: signed in, as authStatusJSON. The
// interactive /login emulation sets loggedIn back to true.
const stubAuthFile = ".ccstub-auth.json"

// stubAuth is one config folder's login state.
type stubAuth struct {
	LoggedIn       *bool  `json:"loggedIn,omitempty"`
	Email          string `json:"email,omitempty"`
	LoginExpiresAt string `json:"loginExpiresAt,omitempty"`
	path           string
}

func (a stubAuth) loggedIn() bool { return a.LoggedIn == nil || *a.LoggedIn }

// statusJSON is the `claude auth status` body for this state.
func (a stubAuth) statusJSON() string {
	if a.LoggedIn == nil && a.Email == "" && a.LoginExpiresAt == "" {
		return authStatusJSON
	}
	out := map[string]any{"loggedIn": a.loggedIn(), "authMethod": "stub"}
	if a.Email != "" {
		out["email"] = a.Email
	}
	if a.LoginExpiresAt != "" {
		out["loginExpiresAt"] = a.LoginExpiresAt
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// loadStubAuth reads the config folder's login state. A missing or bad file
// means signed in.
func loadStubAuth() stubAuth {
	dir, err := stubConfigDir()
	if err != nil {
		return stubAuth{}
	}
	a := stubAuth{path: filepath.Join(dir, stubAuthFile)}
	if data, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(data, &a)
	}
	return a
}

// stdinIsTerminal reports whether stdin is a terminal (a tmux pane), not
// foci's stream-json pipe.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// interactiveLoginTimeout ends an interactive session nobody drives, so a
// gateway killed mid-login leaves no stub (and so no tmux server) behind.
const interactiveLoginTimeout = 3 * time.Minute

// interactiveLogin emulates the screens foci's relogin driver reads
// (internal/relogin/extract.go): after "/login" and a confirming Enter, the
// sign-in anchor, a URL naming this folder, and the paste anchor; after a
// code line, the folder is signed in and the success anchor is shown. The
// session then idles until killed.
func interactiveLogin() {
	time.AfterFunc(interactiveLoginTimeout, func() { os.Exit(0) })
	wd, _ := os.Getwd()
	secure, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if !ok {
		secure = "<unset>"
	}
	writeRecorder(recorderEntry{
		Kind:          "interactive_login",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:       wd,
		PID:           os.Getpid(),
		Binary:        os.Args[0],
		ConfigDir:     os.Getenv("CLAUDE_CONFIG_DIR"),
		SecureStorage: secure,
	})
	fmt.Println("cc-stub interactive (login emulation)")
	in := bufio.NewScanner(os.Stdin)
	state := "idle"
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		switch {
		case state == "idle" && line == "/login":
			state = "menu"
			fmt.Println("Select login method: 1. Claude account")
		case state == "menu":
			state = "code"
			dir, _ := stubConfigDir()
			fmt.Println("Browser didn't open? Use the url below to sign in")
			fmt.Println()
			fmt.Println("https://claude.ai/oauth/authorize?stub=1&dir=" + url.QueryEscape(dir))
			fmt.Println()
			fmt.Println("Paste code here if prompted >")
		case state == "code" && line != "":
			a := loadStubAuth()
			t := true
			a.LoggedIn = &t
			if data, err := json.Marshal(a); err == nil && a.path != "" {
				_ = os.MkdirAll(filepath.Dir(a.path), 0o755)
				_ = os.WriteFile(a.path, data, 0o600)
			}
			state = "done"
			fmt.Println("Login successful. Press Enter to continue")
		}
	}
	select {}
}

// stubArgs is the part of foci's argv the stub acts on.
type stubArgs struct {
	help            bool
	authStatus      bool // `claude auth status`: the readiness probe
	resume          string
	model           string
	settings        string
	skipPermissions bool
	permissionMode  string
}

// parseArgs parses the argv foci passes to claude. Every flag ccstream can
// pass must be declared here, even the ones the stub ignores: an undeclared
// flag, or an argument the stub does not model, is an error (as in real CC),
// never a warning. With Go's flag package a warning would be worse than
// nothing, because Parse stops at the first unknown flag and every flag after
// it (--resume, --settings) is silently lost (#2925). A value given to a
// boolean flag (`--verbose true`) is caught the same way: the value becomes a
// positional argument, which is an error too.
//
// The only positional form accepted is `auth status`, foci's startup
// readiness probe.
func parseArgs(args []string) (stubArgs, error) {
	if len(args) == 2 && args[0] == "auth" && args[1] == "status" {
		return stubArgs{authStatus: true}, nil
	}
	var a stubArgs
	fs := flag.NewFlagSet("cc-stub", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // main reports the error, once
	fs.Bool("print", false, "output-only mode (matches CC)")
	fs.String("input-format", "", "stream-json | text")
	fs.String("output-format", "", "stream-json | text")
	fs.String("permission-prompt-tool", "", "stdio")
	fs.Bool("include-partial-messages", false, "")
	fs.Bool("include-hook-events", false, "")
	fs.Bool("verbose", false, "")
	fs.StringVar(&a.model, "model", "", "model identifier")
	fs.StringVar(&a.resume, "resume", "", "resume session id (empty = fresh)")
	fs.String("allowedTools", "", "permission rules")
	fs.StringVar(&a.settings, "settings", "", "hook settings JSON")
	fs.String("append-system-prompt", "", "")
	fs.BoolVar(&a.skipPermissions, "dangerously-skip-permissions", false, "")
	fs.StringVar(&a.permissionMode, "permission-mode", "", "CC permission mode (foci batch sessions: auto)")
	fs.String("prompt-suggestions", "", "ignored")
	fs.String("disallowedTools", "", "ignored (foci: Agent,Task in every session, #2934)")
	fs.String("effort", "", "ignored (launch effort level, #840)")
	// A restricted batch (ccstream restrictedBatchArgs, #2702).
	fs.Bool("restricted", false, "ignored")
	fs.String("tools", "", "ignored")
	fs.Bool("strict-mcp-config", false, "ignored")
	fs.BoolVar(&a.help, "h", false, "show usage")
	fs.BoolVar(&a.help, "help", false, "show usage")
	if err := fs.Parse(args); err != nil {
		return stubArgs{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return stubArgs{}, fmt.Errorf("unexpected argument %q (a value given to a boolean flag, or a flag after a positional?)", rest[0])
	}
	return a, nil
}

// recordArgvError appends a kind="argv_error" line when parseArgs rejects
// the argv, so the failure shows in the recorder tail L2 tests print.
func recordArgvError(err error) {
	wd, _ := os.Getwd()
	writeRecorder(recorderEntry{
		Kind:      "argv_error",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Workdir:   wd,
		Flags:     os.Args[1:],
		Error:     err.Error(),
		PID:       os.Getpid(),
		Binary:    os.Args[0],
	})
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
func recordUserMessage(sessionID, text string, blockTypes []string, folded bool) {
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
		Folded:            folded,
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

// loadScript reads the per-workdir script JSON from $CCSTUB_SCRIPT_DIR
// for the user message userText, consuming it one-shot. Returns nil if the
// env var is unset, the file doesn't exist, the content is unparseable, or
// the script's Match does not appear in userText — in any of those cases
// the stub falls back to its echo-default behaviour.
//
// One-shot consumption is a CLAIM at load time: the file is renamed to a
// private name before it is parsed, so this turn owns exactly the bytes it
// applies, and the next user message in this long-lived process (e.g. the
// SESSION RESPONSE injection a send_to_session triggers) finds no file
// rather than re-running the script in a loop. It used to be deleted after
// the turn finished instead, which also deleted any script the test wrote
// for its NEXT turn while this one was still running — tests arm the next
// script as soon as they see this turn's user_message (#2079). The caller
// claims BEFORE recording that user_message, so a test that arms the next
// script on seeing it can never overwrite this turn's unclaimed script
// (#2681).
func loadScript(userText string) *stubScript {
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
	// Peek first so a turn the script is not aimed at never touches the file.
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub [error] loadScript: no file at %s (wd=%s)\n", path, wd)
		return nil
	}
	if s, ok := parseScript(path, b); !ok || !scriptMatches(s, userText) {
		if ok {
			fmt.Fprintf(os.Stderr, "cc-stub [error] script %s left in place: match %q not in this user message\n", path, s.Match)
		}
		return nil
	}
	claimed := fmt.Sprintf("%s.claimed-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, claimed); err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub [error] loadScript: claim %s: %v\n", path, err)
		return nil
	}
	// Re-read what was actually claimed: the test may have replaced the
	// file between the peek and the rename.
	b, err = os.ReadFile(claimed)
	if err != nil {
		_ = os.Remove(claimed)
		fmt.Fprintf(os.Stderr, "cc-stub [error] loadScript: read claimed %s: %v\n", claimed, err)
		return nil
	}
	s, ok := parseScript(path, b)
	if ok && !scriptMatches(s, userText) {
		// A newer script aimed at another turn replaced the peeked one
		// in between. Put it back — Link refuses to clobber, so a script
		// written after the claim wins.
		if err := os.Link(claimed, path); err != nil {
			fmt.Fprintf(os.Stderr, "cc-stub [error] loadScript: restore %s: %v\n", path, err)
		}
		ok = false
	}
	_ = os.Remove(claimed)
	if !ok {
		return nil
	}
	// Log script size, not contents — tests use multi-MB Text payloads
	// to exercise foci's stdout scanner cap, and printing them here
	// wedges the stub on its stderr pipe before stdout ever gets the
	// payload. Length + tool-use count is the diagnostic value.
	fmt.Fprintf(os.Stderr, "cc-stub [error] loaded script %s (text_len=%d tool_uses=%d)\n", path, len(s.Text), len(s.ToolUses))
	return s
}

func parseScript(path string, b []byte) (*stubScript, bool) {
	var s stubScript
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintf(os.Stderr, "cc-stub [error] failed to parse script %s: %v\n", path, err)
		return nil, false
	}
	return &s, true
}

func scriptMatches(s *stubScript, userText string) bool {
	return s.Match == "" || strings.Contains(userText, s.Match)
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
  CCSTUB_WRITE_TRANSCRIPT if truthy, append each consumed user message to
                        the session transcript (under $CLAUDE_CONFIG_DIR)
  CCSTUB_HANG           sleep this duration before handshake (e.g. "5s")
  CCSTUB_EMIT_COMPACT_BOUNDARY  if truthy, "/compact" emits status+compact_boundary
  CCSTUB_COMPACT_PRE_TOKENS     pre_tokens in compact_boundary (default 50000)
  CCSTUB_RESPONSE       assistant reply text (default echoes user prompt)
  CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS  if truthy (foci sets it), bracket
                        each run with session_state_changed running/idle

Flags accepted (most ignored — stub only cares about --resume, --model):
  --print --input-format --output-format --permission-prompt-tool
  --include-partial-messages --include-hook-events --verbose
  --model --resume --allowedTools --settings --append-system-prompt
  --dangerously-skip-permissions --permission-mode (either marks a batch run)
  --prompt-suggestions --disallowedTools --effort
  --restricted --tools --strict-mcp-config
  -h / --help
Any other flag or argument exits 1, as in real CC (an undeclared flag
would otherwise drop every flag after it). "auth status" prints a
logged-in status and exits 0, unless the config folder's .ccstub-auth.json
says otherwise ({"loggedIn":false,"email":...,"loginExpiresAt":...}); a
logged-out folder answers every message with CC's "Not logged in" reply.
With no arguments on a terminal (foci's relogin pane) it emulates the
/login screens and signs the folder in when a code is typed.

Foci points at this binary by setting [cc_backend].claude_binary in
foci.toml. See cmd/cc-stub/main.go for the full protocol.`
