package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/peercred"
	"foci/internal/tempdir"
)

// bridgeCounter provides unique socket paths across concurrent exec calls.
var bridgeCounter atomic.Int64

// Accept-error backoff for the bridge accept loop. The values mirror
// net/http.Server.Serve's accept backoff, which handles the same EMFILE-class
// errors (EMFILE, ENFILE, ENOBUFS, ENOMEM) for the same reason: without a wait
// the loop spins at 100% CPU and floods the log while fds are exhausted, and a
// longer cap delays every queued call after the fds come back (#1122).
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = 1 * time.Second
)

// acceptBackoff is the wait range between retries after an Accept error. The
// wait starts at min, doubles on each consecutive error, and is capped at max.
type acceptBackoff struct {
	min, max time.Duration
}

var defaultAcceptBackoff = acceptBackoff{min: acceptBackoffMin, max: acceptBackoffMax}

// ExecBridge creates a per-exec unix socket that exposes ExecExport tools
// as shell functions inside subprocess commands.
type ExecBridge struct {
	sockPath  string
	funcsPath string
	listener  net.Listener
	registry  *Registry
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewExecBridge creates a unix socket and shell functions file. The bridge
// accepts connections until Close is called. ctx carries the session key
// from the calling agent (used by tools that need session identity).
// Socket paths are PID-based and ephemeral — suitable for per-command bridges.
func NewExecBridge(registry *Registry, ctx context.Context) (*ExecBridge, error) {
	n := bridgeCounter.Add(1)
	sockPath := fmt.Sprintf("%s/exec-%d-%d.sock", tempdir.Dir(), os.Getpid(), n)
	funcsPath := fmt.Sprintf("%s/exec-%d-%d-funcs.sh", tempdir.Dir(), os.Getpid(), n)
	return newExecBridge(registry, ctx, sockPath, funcsPath)
}

// NewSessionExecBridge creates an exec bridge for a delegated backend session.
// The socket path is UNIQUE PER BACKEND INSTANCE — it embeds the session key
// (for log/debug correlation), the gateway pid (to isolate separate gateway
// processes sharing one FOCI_TMPDIR, e.g. concurrent tests), and a
// process-local counter (the actual per-instance discriminator):
//
//	exec-<session-key>-<gw-pid>-<n>.sock
//
// Per-instance is deliberate. Two backends can transiently exist for the same
// session key: on /reset the dying session's backend is remapped onto a branch
// key to finish memory formation in the background while a fresh backend takes
// over the original key (see Agent.BranchStrategyFor's session-end case). If
// both derived their bridge path from the session key alone they would share
// one socket, and the dying backend's teardown would close the fresh session's
// bridge out from under it (the #1120 outage). A unique path per instance makes
// that impossible: Close on one bridge can never touch another's socket. The
// path is not reused, so there is no stale socket to remove before listening.
func NewSessionExecBridge(registry *Registry, ctx context.Context, sessionKey string) (*ExecBridge, error) {
	// Sanitize: session keys may contain slashes (e.g. "clutch/c123/b456").
	safe := strings.ReplaceAll(sessionKey, "/", "-")
	n := bridgeCounter.Add(1)
	sockPath := fmt.Sprintf("%s/exec-%s-%d-%d.sock", tempdir.Dir(), safe, os.Getpid(), n)
	funcsPath := fmt.Sprintf("%s/exec-%s-%d-%d-funcs.sh", tempdir.Dir(), safe, os.Getpid(), n)
	return newExecBridge(registry, ctx, sockPath, funcsPath)
}

func newExecBridge(registry *Registry, ctx context.Context, sockPath, funcsPath string) (*ExecBridge, error) {
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("exec bridge listen: %w", err)
	}
	return startExecBridge(registry, ctx, listener, sockPath, funcsPath, defaultAcceptBackoff)
}

// startExecBridge finishes bridge setup on a listener already bound to
// sockPath and starts its accept loop. It takes ownership of listener: on error
// it closes the listener and removes sockPath. Tests use it directly to serve a
// listener that injects Accept errors.
func startExecBridge(registry *Registry, ctx context.Context, listener net.Listener, sockPath, funcsPath string, backoff acceptBackoff) (*ExecBridge, error) {
	// Restrict socket access
	if err := os.Chmod(sockPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("exec bridge chmod: %w", err)
	}

	bridgeCtx, cancel := context.WithCancel(ctx)
	b := &ExecBridge{
		sockPath:  sockPath,
		funcsPath: funcsPath,
		listener:  listener,
		registry:  registry,
		ctx:       bridgeCtx,
		cancel:    cancel,
	}

	// Write shell functions file
	if err := b.writeShellFuncs(); err != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		cancel()
		return nil, fmt.Errorf("exec bridge write funcs: %w", err)
	}

	// Start accept loop
	b.wg.Add(1)
	go b.acceptLoop(listener, backoff)

	execbridgeLog.Debugf("session=%s started sock=%s tools=%d", SessionKeyFromContext(ctx), sockPath, b.exportedToolCount())
	return b, nil
}

// SockPath returns the unix socket path for FOCI_SOCK env var.
func (b *ExecBridge) SockPath() string { return b.sockPath }

// FuncsPath returns the shell functions file path.
func (b *ExecBridge) FuncsPath() string { return b.funcsPath }

// Close stops the listener, waits for in-flight connections, and removes files.
func (b *ExecBridge) Close() {
	b.cancel()
	_ = b.listener.Close()
	b.wg.Wait()
	_ = os.Remove(b.sockPath)
	_ = os.Remove(b.funcsPath)
	execbridgeLog.Debugf("closed sock=%s", b.sockPath)
}

// acceptLoop serves ln until Close. It returns only when ln is closed or the
// bridge ctx ends during a retry wait. Any other Accept error (EMFILE and
// similar) is retried on the same listener after a backoff: returning would
// leave the socket bound with nobody accepting, so every foci-call would hang
// with no log line (#1122, mode M1).
func (b *ExecBridge) acceptLoop(ln net.Listener, backoff acceptBackoff) {
	defer b.wg.Done()
	var wait time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if wait == 0 {
				// Log once per error run; the run ends at the next good Accept.
				execbridgeLog.Warnf("exec bridge accept error on %s: %v; retrying", b.sockPath, err)
				wait = backoff.min
			} else {
				wait = min(wait*2, backoff.max)
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-b.ctx.Done():
				// Close cancels ctx first, so a loop in its wait does not hold
				// Close for up to the cap.
				timer.Stop()
				return
			}
			continue
		}
		wait = 0
		b.wg.Add(1)
		go b.handleConn(conn)
	}
}

func (b *ExecBridge) handleConn(conn net.Conn) {
	defer b.wg.Done()
	defer func() { _ = conn.Close() }()

	// Same-user authentication (defence in depth alongside the 0600 socket
	// mode): the exec bridge runs tools in-process at gateway privilege, so a
	// connection from any other user must be refused before we look at the
	// request — never let a different-UID peer drive these tools.
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		writeError(conn, "exec bridge: non-unix connection rejected")
		return
	}
	if match, err := peercred.MatchesSelf(uc); err != nil {
		execbridgeLog.Warnf("peer credential check failed: %v", err)
		writeError(conn, "peer credential check failed")
		return
	} else if !match {
		execbridgeLog.Warnf("peer UID mismatch, rejecting connection")
		writeError(conn, "peer UID mismatch")
		return
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return
	}

	var req struct {
		Tool   string          `json:"tool"`
		Params json.RawMessage `json:"params"`
		// Hints are set by foci-call from the generated wrapper's stdout-piped
		// detection and --format (#2048); absent from older foci-call builds
		// and from hand-written requests, which then get the zero value.
		Hints OutputHints `json:"hints"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}

	tool := b.registry.Get(req.Tool)
	if tool == nil {
		writeError(conn, fmt.Sprintf("unknown tool: %s", req.Tool))
		return
	}
	if !tool.ExecExport {
		writeError(conn, fmt.Sprintf("tool %s not exported for exec", req.Tool))
		return
	}

	execbridgeLog.Debugf("session=%s call tool=%s", SessionKeyFromContext(b.ctx), req.Tool)
	ctx := b.ctx
	if req.Hints != (OutputHints{}) {
		ctx = WithOutputHints(ctx, req.Hints)
	}
	result, err := tool.Execute(ctx, req.Params)
	if err != nil {
		// Convergence-point logging: every exec-bridge tool error surfaces
		// here so it appears in service logs (individual tools return errors
		// via fmt.Errorf; without this log the error would only reach the
		// calling agent's tool_result envelope). Logged at INFO because the
		// dominant case is benign input/validation errors (e.g. a missing
		// required arg) — agent mistakes, not system faults — and a flood of
		// WARNs for those drowns out genuine problems.
		execbridgeLog.Infof("session=%s tool=%s error: %v", SessionKeyFromContext(b.ctx), req.Tool, err)
		writeError(conn, err.Error())
		return
	}

	if req.Hints.Format == OutputFormatJSON && !result.JSON {
		if result, err = wrapResultAsJSON(result); err != nil {
			writeError(conn, err.Error())
			return
		}
	}

	// When the tool spilled the full result to disk (large http body, etc.),
	// pass the file pointer through instead of inlining megabytes onto the
	// socket. foci-call streams the file straight to stdout, so a pipe
	// (`foci_http_request url | jq`) gets the complete body and CC applies its
	// own output truncation to the final result. text remains the inline
	// preview for the non-spilled case / fallback.
	writeBridgeResponse(conn, bridgeResponse{
		Result:     result.Text,
		ResultFile: result.ResultFile,
		ResultSize: result.ResultSize,
	})
}

// genericJSONOutput is the --json shape of every exported tool that does not
// render its own (Tool.JSONOutput empty); wrapResultAsJSON produces it.
const genericJSONOutput = `{"result": "<the text output>"} — plus "result_file" (path) and "result_size" (bytes) when the text was too large to inline and the full output is on disk.`

// wrapResultAsJSON gives a --json call a JSON document from a tool that only
// renders text (#1215), so every foci_* function honours --json. A spilled
// result keeps its file as a named field rather than being streamed raw, since
// streaming would put non-JSON on stdout.
func wrapResultAsJSON(r ToolResult) (ToolResult, error) {
	out, err := JSONResult(struct {
		Result     string `json:"result"`
		ResultFile string `json:"result_file,omitempty"`
		ResultSize int64  `json:"result_size,omitempty"`
	}{r.Text, r.ResultFile, r.ResultSize})
	if err != nil {
		return ToolResult{}, fmt.Errorf("render --json output: %w", err)
	}
	return out, nil
}

// bridgeResponse is the JSON envelope written back to foci-call.
type bridgeResponse struct {
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	ResultFile string `json:"result_file,omitempty"` // full result on disk; foci-call streams it
	ResultSize int64  `json:"result_size,omitempty"` // total bytes of the full result
}

// writeError sends an error-only response envelope to foci-call.
func writeError(conn net.Conn, errMsg string) {
	writeBridgeResponse(conn, bridgeResponse{Error: errMsg})
}

func writeBridgeResponse(conn net.Conn, resp bridgeResponse) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_, _ = conn.Write(data)
}

func (b *ExecBridge) exportedToolCount() int {
	count := 0
	for _, t := range b.registry.All() {
		if t.ExecExport {
			count++
		}
	}
	return count
}

// jsonPassthroughHelper is a bash helper emitted at the top of the shell
// functions file. Each generated function calls it as its first line:
//
//	local __foci_json_rc=0
//	foci__json "tool" "key1 key2 key3" "$@" && return $__foci_json_rc
//
// The guard fires only when ALL three conditions are met:
//  1. Exactly one argument provided
//  2. The whole argument is exactly one JSON object (slurped, stderr-
//     silenced jq): a parse error anywhere or concatenated values ('{}{}')
//     are not a passthrough (#2300)
//  3. Every key in the parsed object is a valid parameter name for the
//     tool, compared as a whole string inside the jq call — no shell word
//     splitting or pathname expansion of keys (#2300)
//
// On a match the helper calls foci-call exactly once and stores that call's
// exit status into the caller's __foci_json_rc local (bash dynamic scoping:
// the generated function declares it local, so the value dies with the call
// and cannot leak), then returns 0 — which is what fires the guard's
// `&& return`, handing foci-call's status — whatever it is — back to the
// caller. Before #2226 the helper returned foci-call's status directly, so a
// failing passthrough did not fire the `&&`, fell through to the normal body
// and ran foci-call a SECOND time. A return of 1 still means "not a
// passthrough" (__foci_json_rc untouched): the guard does not fire and the
// function's normal body runs.
//
// This prevents false positives when a single positional arg happens to
// look like JSON (e.g. searching for a JSON string).
//
// The same const ships foci__json_type, the shared "exactly one JSON
// value" check (#2300): it prints the type of its argument when the WHOLE
// text is one JSON value and nothing otherwise (empty, unparseable, or
// several values). foci__json_arg types a flag's value with it, and the
// wrappers that need one JSON object (ask) test its output for "object" —
// one filter, so the slurp semantics cannot drift between callers.
//
// Note: helpers use foci__ prefix (not _foci_) because Claude Code's shell
// snapshot mechanism filters out underscore-prefixed functions.
const jsonPassthroughHelper = `# Trace helper: logs to stderr when FOCI_TRACE is set.
foci__trace() { [ -n "${FOCI_TRACE:-}" ] && echo "FOCI_TRACE[$1]: ${*:2}" >&2; return 0; }
export -f foci__trace

# JSON passthrough: if the sole arg is exactly one JSON object with valid
# param keys, use it directly. The check slurps the WHOLE argument (#2300):
# jq parses the text into one array, so concatenated values ('{}{}') reach
# the filter as length 2 and the length==1 test rejects them, while a parse
# error anywhere kills jq before the filter runs (empty output) — either
# way it is not a passthrough. Each key is compared inside jq as a whole
# string (no word splitting, no pathname expansion).
foci__json() {
  local tool="$1" valid_keys="$2"; shift 2
  [ $# -eq 1 ] || return 1
  [ "${1:0:1}" = "{" ] || return 1
  [ "$(printf '%s' "$1" | jq -sr --arg ks "$valid_keys" \
    'if length==1 and (.[0]|type=="object") and all(.[0]|keys[]; . as $k | ($ks|split(" ")|map(select(length>0))|index($k)) != null) then "ok" else "no" end' 2>/dev/null)" = "ok" ] || return 1
  foci-call "$(jq -nc --argjson p "$1" '{"tool":"'"$tool"'","params":$p}')"
  __foci_json_rc=$?
  return 0
}
export -f foci__json

# Print the JSON type of $1 when it is EXACTLY one JSON value, and nothing
# in every other case: empty input, a parse error anywhere, or several
# values — a JSON text is exactly one value (#2300). The one shared
# whole-value check: foci__json_arg types a flag's value with it, and the
# wrappers that need one JSON object (ask) test its output for "object".
foci__json_type() {
  printf '%s' "$1" | jq -sr 'if length==1 then .[0]|type else empty end' 2>/dev/null
}
export -f foci__json_type

# Validate a JSON-typed flag value where the flag is given, so a bad value names
# the flag. Without this the caller sees jq's "invalid JSON text passed to
# --argjson", which names neither the flag nor the value, and which prints TWICE
# because the first failure leaves $params empty and the next jq rejects that
# too (#1811). foci__json_type supplies the whole-value check (#2300): anything
# that is not exactly one JSON value — empty input, a parse error anywhere,
# several values, a valid prefix with trailing text ('1 x') — yields no type,
# so the not-JSON branch below rejects it.
foci__json_arg() {
  local flag="$1" want="$2" val="$3" got
  got="$(foci__json_type "$val")"
  if [ -z "$got" ]; then
    echo "error: $flag expects a JSON $want, but this value is not JSON: $val" >&2
    case "$want" in
      object) echo "       e.g. $flag '{\"Authorization\":\"Bearer TOKEN\"}'" >&2 ;;
      array)  echo "       e.g. $flag '[\"one\",\"two\"]'" >&2 ;;
      number) echo "       e.g. $flag 30" >&2 ;;
    esac
    return 1
  fi
  if [ "$want" != any ] && [ "$got" != "$want" ]; then
    echo "error: $flag expects a JSON $want, got $got: $val" >&2
    return 1
  fi
  return 0
}
export -f foci__json_arg

`

// shellStdoutPipedDetect opens every generated foci_* function (#2048). It
// tells the tool whether the caller piped this call's stdout, so a tool can
// switch to a machine-readable form for `foci_todo list | head`/`| jq`.
//
// isatty cannot answer that: under an agent's Bash tool stdout is never a TTY.
// Instead compare this function's own fd 1 (/proc/$BASHPID) with the calling
// shell's ($$). -ef stats both magic links, so it compares the underlying open
// files (a pipe's inode) with no fork. Measured in bash 5.2 (see
// TestShellFuncStdoutPipedDetection):
//
//	foci_x                  same fd        → not piped
//	foci_x | head           forked, pipe   → piped
//	foci_x 2>&1 | tee f     forked, pipe   → piped
//	x=$(foci_x)             forked, pipe   → piped (output feeds a program)
//	( foci_x )              forked, same fd → not piped
//	foci_x > file           main shell redirects its own fd 1 → not piped
//	( foci_x > file )       forked, fd differs from $$ → piped
//
// "Piped" is relative to the NEAREST enclosing bash: `bash -c 'foci_x' | head`
// reads as not piped, because the inner bash's own stdout is the pipe. Without
// /proc (macOS) the -e test fails and every call reads as not piped, i.e.
// today's behaviour. Both variables are re-declared local on every call so an
// exported value in the caller's environment never leaks into a call; the
// todo wrapper sets FOCI_OUTPUT_FORMAT from --format. foci-call forwards both
// to the gateway as the request's "hints" (OutputHints).
const shellStdoutPipedDetect = `  local -x FOCI_STDOUT_PIPED=0 FOCI_OUTPUT_FORMAT=; [ -e /proc/$$/fd/1 ] && ! [ /proc/${BASHPID:-x}/fd/1 -ef /proc/$$/fd/1 ] && FOCI_STDOUT_PIPED=1`

// shellJSONFlagStrip gives every generated function the shell-only --json
// OUTPUT flag (#1215): it removes each exact "--json" argument, wherever it
// appears, and sets FOCI_OUTPUT_FORMAT=json, which foci-call forwards as the
// request's hints.format. Stripping it here, once, rather than adding an arm to
// each of the hand-written flag parsers means no parser can miss it, and the
// JSON passthrough guard (next line) still sees a lone '{...}' argument. The
// cost is that a literal "--json" can never be passed as a flag VALUE.
const shellJSONFlagStrip = `  local __foci_a __foci_j=; local -a __foci_rest=(); for __foci_a in "$@"; do if [ "$__foci_a" = --json ]; then __foci_j=1; else __foci_rest+=("$__foci_a"); fi; done; if [ -n "$__foci_j" ]; then FOCI_OUTPUT_FORMAT=json; set -- "${__foci_rest[@]}"; fi`

// shellJSONInputTools already use --json as an INPUT flag, so they do not get
// the --json output flag: foci_ask --json '<questions>' predates #1215, and its
// output is a one-line "question posted" acknowledgement anyway.
var shellJSONInputTools = map[string]bool{"ask": true}

// hasJSONOutputFlag reports whether a tool's shell function accepts --json as
// the output flag (see shellJSONFlagStrip).
func hasJSONOutputFlag(t *Tool) bool { return !shellJSONInputTools[t.Name] }

// shellJSONGuardRc declares, in every generated function, the local slot the
// JSON passthrough guard returns foci-call's status through (#2226). It must
// precede the guard line: the foci__json helper stores the one passthrough
// foci-call's exit status into this caller-local (bash dynamic scoping) and
// returns 0, so the guard's `&& return $__foci_json_rc` fires with the real
// status instead of falling through to the normal body — which would call
// foci-call a second time. `local` also keeps the helper's assignment from
// leaking into the caller's shell.
const shellJSONGuardRc = "  local __foci_json_rc=0"

// shellFuncPrologue returns the lines every generated function runs after its
// --help check: the stdout-piped detection, the --json output flag, then the
// JSON passthrough guard (which calls foci-call itself, so it must come after
// both). The guard declares __foci_json_rc local first, so a passthrough
// returns foci-call's own exit status after exactly one call, and a
// non-passthrough (helper return 1) leaves the normal body to run.
func shellFuncPrologue(t *Tool, validKeys string) string {
	lines := []string{shellStdoutPipedDetect}
	if hasJSONOutputFlag(t) {
		lines = append(lines, shellJSONFlagStrip)
	}
	lines = append(lines, shellJSONGuardRc)
	lines = append(lines, fmt.Sprintf("  foci__json %q %q \"$@\" && return $__foci_json_rc", t.Name, validKeys))
	return strings.Join(lines, "\n")
}

// writeShellFuncs generates a bash file defining foci_<toolname>() for each
// exported tool. Functions use jq for safe JSON construction and foci-call
// for socket communication.
//
// Every generated function is validated for help/body parity before write —
// any ExecExport tool whose body lacks a case arm for a flag advertised in
// --help (the bug in TODO #723 for foci_remind) returns an error here, so
// the failure surfaces at production startup rather than at runtime.
func (b *ExecBridge) writeShellFuncs() error {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Auto-generated by foci exec bridge — do not edit\n\n")
	sb.WriteString(jsonPassthroughHelper)

	for _, t := range b.registry.All() {
		if !t.ExecExport {
			continue
		}
		if err := validateShellFuncSchemaParity(t); err != nil {
			return fmt.Errorf("exec bridge: %w", err)
		}
		fn := generateShellFunc(t)
		if fn != "" {
			sb.WriteString(fn)
			sb.WriteString(fmt.Sprintf("export -f foci_%s\n\n", t.Name))
		}
	}

	// O_EXCL rather than os.WriteFile (O_TRUNC, no O_EXCL): funcsPath is
	// built from a predictable pattern (session key + gw pid + counter, all
	// discoverable by a local user), and the temp root it lives under can be
	// group- or world-writable depending on deployment (see
	// internal/tempdir's rootMode). os.WriteFile follows an existing symlink
	// at that path and truncates whatever it points at; O_EXCL makes an
	// existing file/symlink at the path a hard error instead of a silent
	// follow — an attacker who pre-plants the path gets a failed bridge
	// startup, never a write into an arbitrary target (#1501). This is safe
	// to require: the path is never reused (see NewSessionExecBridge's
	// doc comment) and writeShellFuncs runs exactly once per bridge.
	f, err := os.OpenFile(b.funcsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create funcs file: %w", err)
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("write funcs file: %w", err)
	}
	return f.Close()
}

// validateShellFuncSchemaParity ensures every non-positional parameter in a
// tool's JSON schema has a corresponding flag-handler case arm in its
// generated shell function body. The check is structural: it walks the
// schema (the source of truth) and looks for `--<flag>)` or `--<flag>=` in
// the body. Catches both directions of drift —
//   - schema gains a param the body silently ignores (TODO #723: foci_remind
//     advertised --text but the JSON-blob body rejected it)
//   - body claims to handle a flag the schema doesn't define (less common,
//     but the help text would also be missing it)
//
// Runs on every NewExecBridge call, so any test that constructs a bridge
// with real production tools enforces parity automatically — no
// hand-maintained tool list required.
func validateShellFuncSchemaParity(t *Tool) error {
	body := generateShellFunc(t)
	if body == "" {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(t.Parameters, &schema); err != nil || len(schema.Properties) == 0 {
		// Empty or unparseable schemas have no schema params to wire into
		// flags; nothing to validate.
		return nil
	}
	posSet := make(map[string]bool)
	if pos, ok := positionalParamsForTool(t); ok {
		for _, p := range pos {
			posSet[p] = true
		}
	}
	var missing []string
	for param := range schema.Properties {
		if posSet[param] {
			continue
		}
		flag := "--" + strings.ReplaceAll(param, "_", "-")
		if !strings.Contains(body, flag+")") && !strings.Contains(body, flag+"=") {
			missing = append(missing, param)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("tool %q: schema params not wired into shell func body: %s", t.Name, strings.Join(missing, ", "))
	}
	return nil
}

// toolParamKeys extracts the property names from a tool's JSON schema Parameters.
// Returns a space-separated string suitable for the foci__json bash helper.
func toolParamKeys(t *Tool) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(t.Parameters, &schema) != nil || len(schema.Properties) == 0 {
		return ""
	}
	keys := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// toolFlagList returns every schema property (and alias) of a tool as a
// sorted "--kebab-case" flag list, for the "valid flags:" line of a
// hand-written shell function. Hand-maintained copies of that line drifted
// from the schema (#1817: --include-headers was handled and listed there but
// absent from the schema, so --help never mentioned it); deriving it keeps the
// rejection message, --help and the parity check all reading the same source.
func toolFlagList(t *Tool, extra ...string) string {
	flags := append([]string(nil), extra...)
	for _, k := range strings.Fields(toolParamKeys(t)) {
		flags = append(flags, "--"+strings.ReplaceAll(k, "_", "-"))
		for _, alias := range t.Aliases[k] {
			flags = append(flags, "--"+strings.ReplaceAll(alias, "_", "-"))
		}
	}
	sort.Strings(flags)
	return strings.Join(flags, " ")
}

// positionalParamsForTool returns a tool's positional schema params (declared on
// the Tool.Positional struct field — the single source of truth). The bool
// reports whether any are declared.
func positionalParamsForTool(t *Tool) ([]string, bool) {
	return t.Positional, len(t.Positional) > 0
}

// generateHelpText builds a help string for a tool from its description and JSON schema.
func generateHelpText(t *Tool) string {
	var b strings.Builder
	b.WriteString(t.Description)

	// Extract parameter info from JSON schema up front — both the Arguments
	// (positional) and Flags sections draw descriptions from it.
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(t.Parameters, &schema)

	// Build set of positional params to exclude from flags list.
	posSet := make(map[string]bool)
	if pos, ok := positionalParamsForTool(t); ok {
		for _, p := range pos {
			posSet[p] = true
		}
		// Show usage line with positional args.
		fmt.Fprintf(&b, "\n\nUsage: foci_%s", t.Name)
		for _, p := range pos {
			fmt.Fprintf(&b, " <%s>", p)
		}
		b.WriteString(" [flags...]")

		// Arguments section: positionals carry descriptions in the schema
		// (e.g. session_key accepts a bare agent name), but they're excluded
		// from Flags, so surface them here or the affordance is invisible.
		b.WriteString("\n\nArguments:")
		for _, p := range pos {
			desc := schema.Properties[p].Description
			if desc != "" {
				fmt.Fprintf(&b, "\n  %-22s %s", p, desc)
			} else {
				fmt.Fprintf(&b, "\n  %s", p)
			}
		}
	}

	if len(schema.Properties) > 0 {
		// Collect non-positional params as flags.
		keys := make([]string, 0, len(schema.Properties))
		for k := range schema.Properties {
			if !posSet[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			b.WriteString("\n\nFlags:")
			reqSet := make(map[string]bool)
			for _, r := range schema.Required {
				reqSet[r] = true
			}
			for _, k := range keys {
				p := schema.Properties[k]
				req := ""
				if reqSet[k] {
					req = " (required)"
				}
				desc := p.Description
				if len(p.Enum) > 0 {
					desc += " [" + strings.Join(p.Enum, "|") + "]"
				}
				flag := "--" + strings.ReplaceAll(k, "_", "-")
				// #1786: state which actions accept this flag from the SAME data the
				// shell gate enforces, so the table cannot contradict the subcommand
				// lines below it or the rejection the user actually gets.
				// #2244: foci_todo's alone ("todo" is the Name NewTodoTool
				// sets) — it is the only sub-actioned tool, and the lookup is
				// keyed by flag name, so other tools with a same-named
				// parameter (remind --text, http_request --body) have no
				// actions and must not inherit the annotation.
				if t.Name == "todo" {
					if acts := todoFlagActions(flag); len(acts) > 0 {
						desc = strings.TrimSpace(desc) + " [actions: " + strings.Join(acts, ", ") + "]"
					}
				}
				// Append aliases so --help shows them alongside the canonical name.
				for _, alias := range t.Aliases[k] {
					flag += "|--" + strings.ReplaceAll(alias, "_", "-")
				}
				if p.Type == "boolean" {
					flag += " (flag)"
				}
				if desc != "" {
					fmt.Fprintf(&b, "\n  %-22s %s%s", flag, desc, req)
				} else {
					fmt.Fprintf(&b, "\n  %s%s", flag, req)
				}
			}
		}
	}
	b.WriteString(jsonOutputHelp(t))
	return b.String()
}

// jsonOutputHelp is the --help section documenting the --json output flag and
// the shape it prints (#1215); "" for a tool without the flag.
func jsonOutputHelp(t *Tool) string {
	if !hasJSONOutputFlag(t) {
		return ""
	}
	shape := t.JSONOutput
	if shape == "" {
		shape = genericJSONOutput
	}
	return "\n\nOutput: text by default. --json (flag, any position) prints JSON instead:\n  " + shape +
		"\n  Errors are unchanged: a message on stderr and a non-zero exit."
}

// todoActionAliases maps user-friendly aliases to canonical action names.
// Both the shell layer (foci_todo create) and the Go tool layer (action:
// "create" in JSON params from CC tool calls) consult this map to normalize
// to the canonical name before dispatch. New aliases added here propagate
// automatically to both surfaces.
//
// The schema enum in NewTodoTool intentionally lists only canonical names —
// aliases are a convenience layer, not part of the canonical surface (same
// convention as `list-all`, which is shell-only and not in the schema).
var todoActionAliases = map[string]string{
	"create": "add",
	"update": "edit",
	// #1427: the user-facing /todo command accepts "show" (aliases: info,
	// detail) and, for a bare id, renders identical full-detail output to
	// its "get" subcommand — but the foci_todo shell tool only recognized
	// "get", so `foci_todo show <id>` rejected with "unknown action: show"
	// while `foci_todo get <id>` worked. Alias it here so both surfaces
	// agree.
	"show": "get",
}

// resolveTodoAction returns the canonical action for an input action,
// applying todoActionAliases if applicable. Unknown actions pass through
// unchanged so the downstream switch can produce its usage-style error.
func resolveTodoAction(a string) string {
	if canonical, ok := todoActionAliases[a]; ok {
		return canonical
	}
	return a
}

// todoActionAliasesBashCase emits the body of a `case "$action" in ... esac`
// block that rewrites alias action names to their canonical form. Emitted
// near the top of the foci_todo shell function so every downstream lookup
// (action_usage, positional dispatch, main dispatch) sees the canonical name.
func todoActionAliasesBashCase() string {
	// Sort for deterministic output across builds.
	aliases := make([]string, 0, len(todoActionAliases))
	for a := range todoActionAliases {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	var b strings.Builder
	for _, a := range aliases {
		fmt.Fprintf(&b, "    %s) action='%s' ;;\n", a, todoActionAliases[a])
	}
	return b.String()
}

// todoActions defines per-subcommand usage and flag lists for foci_todo, the
// only sub-actioned shell tool today. Single source of truth for both
//
//   - the "Subcommands" block appended to top-level `foci_todo --help`, and
//   - the per-action `foci_todo <action> --help` intercept, and
//   - the per-action "valid flags" list shown when an unknown flag is rejected
//     while a known action is in scope.
//
// Closes the recovery loop documented in TODO #729: previously,
// `foci_todo complete --help` was rejected as "unrecognized flag" and
// `foci_todo complete --note ...` listed every foci_todo flag instead of
// scoping to complete's actual three flags.
//
// Order is preserved (slice not map) so help output is stable.
var todoActions = []struct {
	Name  string
	Usage string // single-line: e.g. "complete <id> [--reason TEXT]"
	Flags string // space-separated --flag list valid for this action; empty = no flags
}{
	{"add", "add <text> | --text TEXT | [<title>] --body TEXT  [--title TEXT] [--priority high|medium|low] [--tag TAGS]   (alias: create; with --body, bare words are the title; the title is prepended in bold)", "--text --body --title --priority --tag"},
	{"list", "list [--tag T] [--status open|done|dropped|all] [--priority P] [--sort F] [--reverse] [--limit N] [--truncate N] [--format jsonl|md]", "--tag --status --priority --sort --reverse --limit --truncate --format"},
	{"list-all", "list-all [--tag T] [--priority P] [--sort F] [--reverse] [--limit N] [--truncate N] [--format jsonl|md]", "--tag --priority --sort --reverse --limit --truncate --format"},
	{"search", "search <query> [--sort F] [--reverse] [--limit N] [--truncate N] [--format jsonl|md]   (query may also be given as --query TEXT; --truncate: chars per item, default 300 unpiped / none piped, 0 = none)", "--query --sort --reverse --limit --truncate --format"},
	{"get", "get <id> [--format jsonl|md]   (alias: show; or --id N)", "--id --format"},
	{"complete", "complete <id> [--reason|--notes|--note|--text TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason --notes --note --text"},
	{"drop", "drop <id> [--reason|--notes|--note|--text TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason --notes --note --text"},
	{"reopen", "reopen <id>   (status→open, clears completed_at/close_reason; or --id N / --ids 1,2,3)", "--id --ids"},
	{"start", "start <id>   (status→started; or --id N / --ids 1,2,3)", "--id --ids"},
	{"edit", "edit <id> [--text TEXT] [--title TEXT] [--append-text|--note|--add TEXT] [--append] [--priority P] [--tag T | --add-tag T --remove-tag T]   (alias: update; or --id N / --ids 1,2,3; --tag replaces the whole tag set, --add-tag/--remove-tag change one tag and keep the rest)", "--id --ids --text --title --append --append-text --add --note --notes --priority --tag --add-tag --remove-tag"},
	{"remove", "remove --id N   (or --ids 1,2,3)", "--id --ids"},
}

// todoFlagActions returns the actions whose allowlist accepts flag, in
// todoActions order. Empty means no action accepts it — a positional-only
// parameter such as --query, which `search` takes as an argument.
//
// #1786: this is the single source for the "which actions is this flag for?"
// annotation in `foci_todo --help`. That annotation used to be hand-written
// prose inside each schema description, so it drifted from the allowlist that
// actually enforces validity: the table advertised `search --status`, the
// subcommand line below it did not, and the shell rejected it.
func todoFlagActions(flag string) []string {
	var out []string
	for _, a := range todoActions {
		for _, f := range strings.Fields(a.Flags) {
			if f == flag {
				out = append(out, a.Name)
				break
			}
		}
	}
	return out
}

// todoActionsBashCase emits the inner body of a `case "$action" in ... esac`
// block that populates `action_usage` and `action_flags` for known actions.
// Unknown actions leave both empty, which the surrounding bash treats as
// "no action context" — falling back to the master usage line.
func todoActionsBashCase() string {
	var b strings.Builder
	for _, a := range todoActions {
		// Single-quote in bash is a literal — none of the usage strings
		// contain ' so no escaping is required. If that ever changes,
		// switch to the standard '\\'' bash escape.
		fmt.Fprintf(&b, "    %s)\n      action_usage='%s'\n      action_flags='%s' ;;\n", a.Name, a.Usage, a.Flags)
	}
	return b.String()
}

// todoSubcommandsHelpBlock returns the "Subcommands:" section appended to
// the top-level `foci_todo --help` output.
func todoSubcommandsHelpBlock() string {
	var b strings.Builder
	b.WriteString("\n\nSubcommands:")
	for _, a := range todoActions {
		fmt.Fprintf(&b, "\n  foci_todo %s", a.Usage)
	}
	b.WriteString("\n\nOutput: list, list-all, search and get print markdown, but when their stdout is piped" +
		"\n(foci_todo list | head, | grep, | jq, or captured by $(...)) they print JSONL instead: one JSON" +
		"\nobject per item per line (id, status, priority, tags, title, created_at, updated_at, body excerpt;" +
		"\nget gives the full body). A capped list ends with a {\"truncated\":true,...} line." +
		"\n--format jsonl|md forces either form regardless of piping; --json is the same as --format jsonl.")
	b.WriteString("\n\nRun 'foci_todo <subcommand> --help' for subcommand-specific usage.")
	return b.String()
}

// generateShellFunc returns a bash function definition for a tool.
// Each tool gets a function named foci_<toolname> with appropriate argument handling.
// Every function starts with a help flag check, then a JSON passthrough guard:
// if the sole argument is a valid JSON object whose keys are all valid parameter
// names for this tool, it is sent directly to foci-call as tool params.
func generateShellFunc(t *Tool) string {
	name := "foci_" + t.Name
	validKeys := toolParamKeys(t)
	helpText := generateHelpText(t)
	// Escape single quotes for embedding in bash single-quoted heredoc.
	escapedHelp := strings.ReplaceAll(helpText, "'", "'\\''")
	helpCheck := fmt.Sprintf("  if [ \"${1:-}\" = \"-h\" ] || [ \"${1:-}\" = \"--help\" ]; then\n    echo '%s'\n    return 0\n  fi", escapedHelp)
	guard := shellFuncPrologue(t, validKeys)

	switch t.Name {
	case "http_request":
		// URL as first arg, flags for method, headers, body, save_to, etc.
		// Hand-written for the repeatable --header 'K: V' accumulator, which
		// has no schema property; everything else (including the "valid
		// flags:" list) comes from the schema like the generic generator.
		return fmt.Sprintf(`%s() {
%s
%s
  local url="" method="GET" body="" body_file="" save_to="" save_json_path="" headers="{}" query="{}" include_headers=false background=false timeout="" max_bytes="" files="[]" form_fields="{}" basic_auth=""
  local __foci_url_via=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --url) if [ "$__foci_url_via" = pos ]; then echo "error: url was already given positionally; use --url OR the positional form, not both" >&2; return 1; fi; __foci_url_via=flag; url="$2"; shift 2 ;;
      --method) method="$2"; shift 2 ;;
      --body) body="$2"; shift 2 ;;
      --body-file) body_file="$2"; shift 2 ;;
      --basic-auth) basic_auth="$2"; shift 2 ;;
      --header) headers="$(echo "$headers" | jq --arg k "${2%%%%:*}" --arg v "${2#*: }" '. + {($k): $v}')"; shift 2 ;;
      --headers) foci__json_arg --headers object "$2" || return 1; headers="$2"; shift 2 ;;
      --query) foci__json_arg --query object "$2" || return 1; query="$2"; shift 2 ;;
      --save-to) save_to="$2"; shift 2 ;;
      --save-from-json-path) save_json_path="$2"; shift 2 ;;
      --timeout) foci__json_arg --timeout number "$2" || return 1; timeout="$2"; shift 2 ;;
      --max-response-bytes) foci__json_arg --max-response-bytes number "$2" || return 1; max_bytes="$2"; shift 2 ;;
      --files) foci__json_arg --files array "$2" || return 1; files="$2"; shift 2 ;;
      --form-fields) foci__json_arg --form-fields object "$2" || return 1; form_fields="$2"; shift 2 ;;
      --background) background=true; shift ;;
      --include-headers) include_headers=true; shift ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid flags: %s" >&2
        echo "note: foci_http_request <url> [flags...] — <url> may also be given as a bare argument" >&2
        return 1 ;;
      *)
        if [ "$__foci_url_via" = flag ]; then echo "error: url was already given as --url; use --url OR the positional form, not both" >&2; return 1; fi
        __foci_url_via=pos; url="$1"; shift ;;
    esac
  done
  if [ -z "$url" ]; then
    echo "usage: %s <url> [--method METHOD] [--header 'K: V'] [--body BODY] [--save-to PATH] [--timeout SECS] ..." >&2
    return 1
  fi
  local params
  params="$(jq -nc --arg u "$url" --arg m "$method" --argjson h "$headers" '{"url":$u,"method":$m,"headers":$h}')"
  [ -n "$body" ] && params="$(echo "$params" | jq --arg b "$body" '. + {body: $b}')"
  [ -n "$body_file" ] && params="$(echo "$params" | jq --arg b "$body_file" '. + {body_file: $b}')"
  [ -n "$basic_auth" ] && params="$(echo "$params" | jq --arg a "$basic_auth" '. + {basic_auth: $a}')"
  [ -n "$save_to" ] && params="$(echo "$params" | jq --arg s "$save_to" '. + {save_to: $s}')"
  [ -n "$save_json_path" ] && params="$(echo "$params" | jq --arg s "$save_json_path" '. + {save_from_json_path: $s}')"
  [ -n "$timeout" ] && params="$(echo "$params" | jq --argjson t "$timeout" '. + {timeout: $t}')"
  [ -n "$max_bytes" ] && params="$(echo "$params" | jq --argjson m "$max_bytes" '. + {max_response_bytes: $m}')"
  [ "$background" = true ] && params="$(echo "$params" | jq '. + {background: true}')"
  [ "$include_headers" = true ] && params="$(echo "$params" | jq '. + {include_headers: true}')"
  [ "$query" != "{}" ] && params="$(echo "$params" | jq --argjson q "$query" '. + {query: $q}')"
  [ "$files" != "[]" ] && params="$(echo "$params" | jq --argjson f "$files" '. + {files: $f}')"
  [ "$form_fields" != "{}" ] && params="$(echo "$params" | jq --argjson f "$form_fields" '. + {form_fields: $f}')"
  foci-call "$(jq -nc --argjson p "$params" '{"tool":"http_request","params":$p}')"
}
`, name, helpCheck, guard, toolFlagList(t, "--header"), name)

	case "todo":
		// action as first arg, rest varies by action. helpCheck above is the
		// generic schema-driven help; override it here so top-level
		// `foci_todo --help` also lists subcommands. Per-action --help is
		// handled inline after the action is parsed below.
		todoFullHelp := helpText + todoSubcommandsHelpBlock()
		todoEscapedHelp := strings.ReplaceAll(todoFullHelp, "'", "'\\''")
		todoHelpCheck := fmt.Sprintf("  if [ \"${1:-}\" = \"-h\" ] || [ \"${1:-}\" = \"--help\" ]; then\n    echo '%s'\n    return 0\n  fi", todoEscapedHelp)
		return fmt.Sprintf(`%s() {
%s
%s
  local action="$1"; shift 2>/dev/null || true
  # Normalize action aliases (e.g. "create" → "add"). Single source of truth:
  # todoActionAliases in internal/tools/execbridge.go.
  case "$action" in
%s  esac
  # Per-action usage and flag scope for --help and unknown-flag errors.
  # See todoActions in internal/tools/execbridge.go for the source of truth.
  local action_usage="" action_flags=""
  case "$action" in
%s  esac
  # #1901: validate the action against todoActions BEFORE any flag parsing
  # (including the --help intercept just below). A lookup miss for a
  # non-empty action means the action itself is unrecognized — report that
  # directly instead of letting a later unrecognized-flag or missing-help
  # branch misattribute the failure to the flag ("'frobnicate' takes no
  # flags") or the specific flag ("unrecognized flag: --bar") when the real
  # problem is the action name.
  if [ -n "$action" ] && [ -z "$action_usage" ]; then
    echo "error: unknown action '$action'" >&2
    echo "usage: foci_todo <add|list|list-all|search|get|complete|drop|reopen|start|edit|remove> [args...]" >&2
    return 1
  fi
  if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    if [ -n "$action_usage" ]; then
      echo "usage: foci_todo $action_usage"
    else
      echo "usage: foci_todo <add|list|list-all|search|get|complete|drop|reopen|start|edit|remove> [args...]"
      echo "Run 'foci_todo --help' for full help."
    fi
    return 0
  fi
  local text="" priority="" tag="" query="" status="" id="" ids="" reason="" sort="" reverse="" limit="" truncate="" append="" append_text="" body="" title="" add_words="" add_tag="" remove_tag=""
  while [ $# -gt 0 ]; do
    # #1218: reject flags that are globally-known but not valid for THIS action
    # (e.g. edit --status done — --status is a list/search filter that edit's
    # builder silently ignores, so the close looked like it worked but was a
    # no-op). action_flags is the per-action allowlist from todoActions. Only
    # enforced for known actions (non-empty allowlist); an unknown or flagless
    # action falls through to the master unrecognized-flag handler below.
    if [ -n "$action_flags" ]; then
      case "$1" in
        --*)
          case " $action_flags " in
            *" $1 "*) : ;;
            *)
              echo "error: flag $1 is not valid for 'foci_todo $action'" >&2
              echo "valid flags for '$action': $action_flags" >&2
              return 1 ;;
          esac ;;
      esac
    fi
    case "$1" in
      --text) text="$2"; shift 2 ;;
      --body) body="$2"; shift 2 ;;
      --title) title="$2"; shift 2 ;;
      --priority) priority="$2"; shift 2 ;;
      # #1794: a REPEATED --tag accumulates rather than overwriting. Building a
      # list by repeating a flag is the usual CLI convention (and --ids here
      # already takes several values), so the old last-one-wins silently dropped
      # every tag but the last and left the item unfindable under the others.
      # Joining with "," lands on the comma form the tool already accepts.
      --tag) if [ -n "$tag" ]; then tag="$tag,$2"; else tag="$2"; fi; shift 2 ;;
      # #2198: change one tag against the item's current set (edit only),
      # accumulating like --tag.
      --add-tag) if [ -n "$add_tag" ]; then add_tag="$add_tag,$2"; else add_tag="$2"; fi; shift 2 ;;
      --remove-tag) if [ -n "$remove_tag" ]; then remove_tag="$remove_tag,$2"; else remove_tag="$2"; fi; shift 2 ;;
      --query) query="$2"; shift 2 ;;
      --status) status="$2"; shift 2 ;;
      --id) foci__json_arg --id number "$2" || return 1; id="$2"; shift 2 ;;
      --ids) ids="$2"; shift 2 ;;
      --reason) reason="$2"; shift 2 ;;
      --notes) reason="$2"; shift 2 ;;
      --note) reason="$2"; shift 2 ;;
      --append-text) append_text="$2"; shift 2 ;;
      --add) append_text="$2"; shift 2 ;;
      --append) append=true; shift ;;
      --sort) sort="$2"; shift 2 ;;
      --limit) foci__json_arg --limit number "$2" || return 1; limit="$2"; shift 2 ;;
      --truncate) foci__json_arg --truncate number "$2" || return 1; truncate="$2"; shift 2 ;;
      --reverse) reverse=true; shift ;;
      # #2048: explicit output form, overriding the stdout-piped detection in
      # the prologue. Carried to the tool as a hint, not a schema param, so the
      # API tool surface is unchanged.
      --format)
        case "${2:-}" in
          jsonl|md) FOCI_OUTPUT_FORMAT="$2" ;;
          *) echo "error: --format expects jsonl|md, got: ${2:-}" >&2; return 1 ;;
        esac
        shift 2 ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        if [ -n "$action_flags" ]; then
          echo "valid flags for '$action': $action_flags" >&2
        elif [ -n "$action" ]; then
          echo "'$action' takes no flags" >&2
        else
          echo "valid flags: --text --priority --tag --query --status --id --ids --reason --notes --note --append --append-text --add --sort --reverse --limit --truncate --format" >&2
        fi
        return 1 ;;
      *) # positional: first positional is text/query/id depending on action
        case "$action" in
          add) add_words="$add_words $1" ;;
          search) query="$query $1" ;;
          get|complete|drop|reopen|start|remove) id="$1" ;;
          edit)
            # A numeric positional is the item id (so "update 6 ..." works like
            # complete/drop); anything else falls back to text for back-compat.
            case "$1" in
              ''|*[!0-9]*) text="$text $1" ;;
              *) if [ -z "$id" ]; then id="$1"; else text="$text $1"; fi ;;
            esac ;;
        esac
        shift ;;
    esac
  done
  text="${text# }"
  query="${query# }"
  # #941: --body is an alias for the todo text. #2075: with --body, bare words
  # are the TITLE (add "<title>" --body "<text>"); without it they are the text.
  # Every combination that would drop one value is refused. The tool composes
  # the bold title line, so a direct tool call gets the same result.
  if [ "$action" = add ]; then
    add_words="${add_words# }"
    if [ -n "$body" ]; then
      if [ -n "$text" ]; then
        echo "error: add: --text and --body are the same field; use --text OR --body, not both" >&2
        return 1
      fi
      if [ -n "$add_words" ]; then
        if [ -n "$title" ]; then
          echo "error: add: with --body, bare words are the title, and the title was already given as --title" >&2
          return 1
        fi
        title="$add_words"
      fi
      text="$body"
    elif [ -n "$add_words" ]; then
      if [ -n "$text" ]; then
        echo "error: add: the text was given twice; use --text OR bare words, not both" >&2
        return 1
      fi
      text="$add_words"
    fi
  fi
  # On complete/drop, --text aliases --reason (writes to close_reason).
  # --notes and --note are parsed directly into reason above. If both --text
  # and --reason are supplied, --reason wins (explicit beats implicit).
  case "$action" in
    complete|drop)
      if [ -z "$reason" ] && [ -n "$text" ]; then
        reason="$text"
        text=""
      fi
      ;;
    edit)
      # Append content arrives via --append-text/--add directly, or via
      # --note/--notes (parsed into reason above) as an ergonomic alias on edit.
      # --append is the bare boolean form, paired with --text.
      if [ -z "$append_text" ] && [ -n "$reason" ]; then
        append_text="$reason"
        reason=""
      fi
      if [ -n "$append_text" ]; then
        if [ -n "$text" ]; then
          echo "error: edit: use --text (replace) OR --append-text/--note/--add (append), not both" >&2
          return 1
        fi
        text="$append_text"
        append=true
      fi
      ;;
  esac
  # Accept comma-separated form for --ids alongside JSON array form (TODO #751).
  # Help text and other foci tooling show "1,2,3" but jq --argjson rejects bare
  # comma form. Normalise here so callers don't need to remember to wrap in [].
  # Strip whitespace, then wrap if not already a JSON array.
  if [ -n "$ids" ]; then
    case "$ids" in
      \[*\]) ;;  # already JSON array — pass through
      *) ids="[$(echo "$ids" | tr -d ' ')]" ;;
    esac
    foci__json_arg --ids array "$ids" || return 1
  fi
  case "$action" in
    add)
      local params='{"action":"add"}'
      [ -n "$text" ] && params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
      [ -n "$title" ] && params="$(echo "$params" | jq --arg t "$title" '. + {title: $t}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    list)
      local params='{"action":"list"}'
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      [ -n "$status" ] && params="$(echo "$params" | jq --arg s "$status" '. + {status: $s}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"
      [ -n "$reverse" ] && params="$(echo "$params" | jq '. + {reverse: true}')"
      [ -n "$limit" ] && params="$(echo "$params" | jq --argjson l "$limit" '. + {limit: $l}')"
      [ -n "$truncate" ] && params="$(echo "$params" | jq --argjson n "$truncate" '. + {truncate: $n}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    list-all)
      local params='{"action":"list","status":"all"}'
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"
      [ -n "$reverse" ] && params="$(echo "$params" | jq '. + {reverse: true}')"
      [ -n "$limit" ] && params="$(echo "$params" | jq --argjson l "$limit" '. + {limit: $l}')"
      [ -n "$truncate" ] && params="$(echo "$params" | jq --argjson n "$truncate" '. + {truncate: $n}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    search)
      local params='{"action":"search"}'
      [ -n "$query" ] && params="$(echo "$params" | jq --arg q "$query" '. + {query: $q}')"
      [ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"
      [ -n "$reverse" ] && params="$(echo "$params" | jq '. + {reverse: true}')"
      [ -n "$limit" ] && params="$(echo "$params" | jq --argjson l "$limit" '. + {limit: $l}')"
      [ -n "$truncate" ] && params="$(echo "$params" | jq --argjson n "$truncate" '. + {truncate: $n}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    get)
      foci-call "$(jq -nc --argjson id "$id" '{"tool":"todo","params":{"action":"get","id":$id}}')"
      ;;
    complete)
      local params='{"action":"complete"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      [ -n "$reason" ] && params="$(echo "$params" | jq --arg r "$reason" '. + {reason: $r}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    drop)
      local params='{"action":"drop"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      [ -n "$reason" ] && params="$(echo "$params" | jq --arg r "$reason" '. + {reason: $r}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    reopen)
      local params='{"action":"reopen"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    start)
      local params='{"action":"start"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    edit)
      local params='{"action":"edit"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      [ -n "$text" ] && params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
      [ -n "$title" ] && params="$(echo "$params" | jq --arg ti "$title" '. + {title: $ti}')"
      [ -n "$append" ] && params="$(echo "$params" | jq '. + {append: true}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      [ -n "$add_tag" ] && params="$(echo "$params" | jq --arg g "$add_tag" '. + {add_tag: $g}')"
      [ -n "$remove_tag" ] && params="$(echo "$params" | jq --arg g "$remove_tag" '. + {remove_tag: $g}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    remove)
      local params='{"action":"remove"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    *)
      echo "usage: %s <add|list|list-all|search|get|complete|drop|reopen|start|edit|remove> [args...]" >&2
      return 1
      ;;
  esac
}
`, name, todoHelpCheck, guard, todoActionAliasesBashCase(), todoActionsBashCase(), name)

	case "summary":
		// Prompt as argument; content from --file or stdin
		return fmt.Sprintf(`%s() {
%s
%s
  local prompt="" file=""
  local __foci_prompt_via="" __foci_pos_file=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --prompt) if [ "$__foci_prompt_via" = pos ]; then echo "error: prompt was already given positionally; use --prompt OR the positional form, not both" >&2; return 1; fi; __foci_prompt_via=flag; prompt="$2"; shift 2 ;;
      --file) file="$2"; shift 2 ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid flags: --prompt --file" >&2
        echo "note: foci_summary <prompt> [flags...] — <prompt> may also be given as a bare argument" >&2
        return 1 ;;
      *)
        if [ "$__foci_prompt_via" = flag ]; then echo "error: prompt was already given as --prompt; use --prompt OR the positional form, not both" >&2; return 1; fi
        # Remembered only to explain a failure below: a path given positionally
        # joins the prompt, it is never read (#2150).
        if [ -z "$__foci_pos_file" ] && [ -f "$1" ]; then __foci_pos_file="$1"; fi
        __foci_prompt_via=pos; prompt="$prompt $1"; shift ;;
    esac
  done
  prompt="${prompt# }"
  if [ -z "$prompt" ]; then
    echo "usage: %s <prompt> [--file PATH]" >&2
    echo "  or: cat file | %s \"prompt\"" >&2
    return 1
  fi
  if [ -z "$file" ] && [ ! -t 0 ]; then
    mkdir -p /tmp/foci
    file="$(mktemp /tmp/foci/summary-XXXXXX)"
    cat > "$file"
    trap "rm -f '$file'" EXIT
    # Empty stdin is no input. Sending it on fails server-side with "file is
    # empty: /tmp/foci/summary-XXXXXX", naming a temp file the caller never
    # chose and hiding the real mistake (#2150).
    if [ ! -s "$file" ]; then rm -f "$file"; file=""; fi
  fi
  if [ -z "$file" ]; then
    echo "error: no input: pass --file <path> or pipe content on stdin" >&2
    if [ -n "$__foci_pos_file" ]; then
      echo "hint: '$__foci_pos_file' is a file, but a bare argument is part of the prompt and is not read; use: %s --file '$__foci_pos_file' \"<prompt>\"" >&2
    fi
    return 1
  fi
  foci-call "$(jq -nc --arg f "$file" --arg p "$prompt" '{"tool":"summary","params":{"file":$f,"prompt":$p}}')"
}
`, name, helpCheck, guard, name, name, name)

	case "tmux":
		// Subcommand-style dispatch (same pattern as todo)
		return fmt.Sprintf(`%s() {
%s
%s
  local op="$1"; shift 2>/dev/null || true
  local name="" command="" workdir="" watch="" keys="" enter="" lines="" window="" threshold_seconds="" raw=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --name) name="$2"; shift 2 ;;
      --command) command="$2"; shift 2 ;;
      --workdir) workdir="$2"; shift 2 ;;
      --watch) foci__json_arg --watch any "$2" || return 1; watch="$2"; shift 2 ;;
      --keys) keys="$2"; shift 2 ;;
      --enter) foci__json_arg --enter any "$2" || return 1; enter="$2"; shift 2 ;;
      --lines) foci__json_arg --lines number "$2" || return 1; lines="$2"; shift 2 ;;
      --window) foci__json_arg --window number "$2" || return 1; window="$2"; shift 2 ;;
      --threshold-seconds) foci__json_arg --threshold-seconds number "$2" || return 1; threshold_seconds="$2"; shift 2 ;;
      --raw) raw=true; shift ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid flags: --name --command --workdir --watch --keys --enter --lines --window --threshold-seconds --raw" >&2
        return 1 ;;
      *)
        echo "error: unexpected positional argument: $1" >&2
        return 1 ;;
    esac
  done
  case "$op" in
    start)
      local params='{"operation":"start"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      [ -n "$command" ] && params="$(echo "$params" | jq --arg c "$command" '. + {command: $c}')"
      [ -n "$workdir" ] && params="$(echo "$params" | jq --arg w "$workdir" '. + {workdir: $w}')"
      [ -n "$watch" ] && params="$(echo "$params" | jq --argjson w "$watch" '. + {watch: $w}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    send)
      local params='{"operation":"send"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      if [ -n "$keys" ]; then
        params="$(echo "$params" | jq --arg k "$keys" '. + {keys: $k}')"
      elif [ ! -t 0 ]; then
        keys="$(cat)"
        params="$(echo "$params" | jq --arg k "$keys" '. + {keys: $k}')"
      fi
      [ -n "$enter" ] && params="$(echo "$params" | jq --argjson e "$enter" '. + {enter: $e}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    read)
      local params='{"operation":"read"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      [ -n "$lines" ] && params="$(echo "$params" | jq --argjson l "$lines" '. + {lines: $l}')"
      [ -n "$raw" ] && params="$(echo "$params" | jq '. + {raw: true}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    list)
      foci-call '{"tool":"tmux","params":{"operation":"list"}}'
      ;;
    kill)
      local params='{"operation":"kill"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    watch)
      local params='{"operation":"watch"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      [ -n "$window" ] && params="$(echo "$params" | jq --argjson w "$window" '. + {window: $w}')"
      [ -n "$threshold_seconds" ] && params="$(echo "$params" | jq --argjson t "$threshold_seconds" '. + {threshold_seconds: $t}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    unwatch)
      local params='{"operation":"unwatch"}'
      [ -n "$name" ] && params="$(echo "$params" | jq --arg n "$name" '. + {name: $n}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"tmux","params":$p}')"
      ;;
    *)
      echo "usage: %s <start|send|read|list|kill|watch|unwatch> [args...]" >&2
      return 1
      ;;
  esac
}
`, name, helpCheck, guard, name)

	case "ask":
		// Primarily JSON-only input (no flat per-field flags for questions, per
		// design): accept the questions object as a positional arg (also caught
		// by the foci__json passthrough guard), via --json, or piped on stdin —
		// from exactly ONE of those sources: a second positional or a repeated
		// --json is an error, not a silent overwrite of what was already given
		// (the dropped-argument bug class #2271 fixed for zero-property tools).
		// The stdin read shares the generic generator's FOCI_STDIN_WAIT bound
		// (#1552), so a never-closing pipe cannot hang the wrapper. Whatever
		// supplied it, $json is verified by foci__json_type to be exactly one
		// JSON object before jq sees it (#2300): anything else gets the usage
		// error, not jq's --argjson internals. The optional grader params may
		// live INSIDE that JSON object, or be supplied as flags (merged in
		// below) for CLI convenience. Async tool — returns immediately after
		// posting the first question.
		return fmt.Sprintf(`%s() {
%s
%s
  local json="" grader="" grader_args="" grader_timeout="" grader_on_error=""
  local __foci_json_via=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --json)
        if [ "$__foci_json_via" = pos ]; then echo "error: the questions JSON was already given positionally; use --json OR the positional form, not both" >&2; return 1; fi
        if [ "$__foci_json_via" = flag ]; then echo "error: the questions JSON was already given as --json; pass the questions JSON exactly once" >&2; return 1; fi
        __foci_json_via=flag
        foci__json_arg --json any "$2" || return 1
        json="$2"; shift 2 ;;
      --grader) grader="$2"; shift 2 ;;
      --grader-args) foci__json_arg --grader-args array "$2" || return 1; grader_args="$2"; shift 2 ;;
      --grader-timeout-seconds) foci__json_arg --grader-timeout-seconds number "$2" || return 1; grader_timeout="$2"; shift 2 ;;
      --grader-on-error) grader_on_error="$2"; shift 2 ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid: --json --grader --grader-args --grader-timeout-seconds --grader-on-error (or pass JSON positionally, or pipe it on stdin)" >&2
        return 1 ;;
      *)
        if [ "$__foci_json_via" = flag ]; then echo "error: the questions JSON was already given as --json; use --json OR the positional form, not both" >&2; return 1; fi
        if [ "$__foci_json_via" = pos ]; then echo "error: the questions JSON was already given; expected ONE questions JSON object, not several arguments" >&2; return 1; fi
        __foci_json_via=pos; json="$1"; shift ;;
    esac
  done
%s
  if [ -z "$json" ] || [ "$(foci__json_type "$json")" != object ]; then
    if [ -n "$json" ]; then
      echo "error: %s expects one JSON object, got: $json" >&2
    fi
    echo "usage: %s '{\"questions\":[{\"question\":\"...\",\"options\":[{\"label\":\"...\"}]}]}'" >&2
    echo "  or: %s --json '<json>'   or:  echo '<json>' | %s" >&2
    return 1
  fi
  if [ -n "$grader" ]; then json="$(echo "$json" | jq --arg g "$grader" '. + {grader:$g}')"; fi
  if [ -n "$grader_args" ]; then json="$(echo "$json" | jq --argjson a "$grader_args" '. + {grader_args:$a}')"; fi
  if [ -n "$grader_timeout" ]; then json="$(echo "$json" | jq --argjson t "$grader_timeout" '. + {grader_timeout_seconds:$t}')"; fi
  if [ -n "$grader_on_error" ]; then json="$(echo "$json" | jq --arg e "$grader_on_error" '. + {grader_on_error:$e}')"; fi
  foci-call "$(jq -nc --argjson p "$json" '{"tool":"ask","params":$p}')"
}
`, name, helpCheck, guard, stdinReadSnippet("json", "", "json"), name, name, name, name)

	default:
		// Schema-driven generic: emits a flag-parsing function whose
		// accepted flags are exactly those advertised by generateHelpText.
		// This is the default path for any tool that doesn't have
		// hand-rolled UX (stdin reading, accumulator flags, subcommand
		// dispatch). See generateGenericShellFunc for behavior details.
		return generateGenericShellFunc(t)
	}
}

// blobFociCallTail is the shared closing of both empty-schema fallback
// bodies in generateGenericShellFunc: send "$1" — the params blob, defaulted
// to {} by the branch that emitted it — to the gateway and close the
// function. One emitter so the request shape cannot drift between the two
// bodies; TestGenerateGenericShellFuncEmptyFallback pins the exact line.
func blobFociCallTail(toolName string) string {
	return fmt.Sprintf("  foci-call \"$(jq -nc --argjson p \"$1\" '{\"tool\":\"%s\",\"params\":$p}')\"\n}\n", toolName)
}

// stdinReadSnippet emits the "no value given, so stdin IS the value" read
// shared by the generic generator and the ask wrapper (#1552). Reading is
// the job here — but blocking is legitimate only until the stream ends,
// and two stdin shapes never end:
//
//	B1  nothing ever arrives — an fd a supervisor left open (systemd
//	    StandardInput, an inherited `exec 3< <(cmd)`), with no --flag to
//	    fall back on.
//	B2  data arrives but the stream does not close — a `tail -f` or a
//	    `curl -N` piped in. Reads happily, returns never.
//
// Both present as SILENCE rather than an error, which is the actual harm: a
// cron job simply stops, and you find out when the message never arrives.
// So the read is bounded at FOCI_STDIN_WAIT (default 30s), then WARN and
// proceed with whatever was read. The warning is the load-bearing half: a
// bound alone would make B2 truncate a slow-but-finite upstream and send
// the fragment looking like a success — corruption traded for a hang,
// which is a bad trade. Announced on stderr, it is a diagnosis. (Note the
// common slow-pipe case degrades safely on its own: an upstream that
// buffers and flushes at the end delivers nothing by 30s, so the value is
// empty and the caller's own required-value check below rejects the call
// outright rather than sending a fragment.)
//
// FOCI_STDIN_WAIT overrides the bound, and exists so the regression test
// can assert this in a second instead of thirty. flag names the argument
// the warning suggests instead of the pipe; extraGuard appends a condition
// (the generic generator passes one when a `--file -` param already
// consumed stdin, which is single-use).
func stdinReadSnippet(param, extraGuard, flag string) string {
	return fmt.Sprintf(
		"  if [ -z \"$%[1]s\" ] && [ ! -t 0 ]%[2]s; then\n"+
			"    %[1]s=\"$(timeout \"${FOCI_STDIN_WAIT:-30}\" cat)\"\n"+
			"    if [ $? -eq 124 ]; then\n"+
			"      echo \"warning: stdin was still open after ${FOCI_STDIN_WAIT:-30}s and has been cut off there;"+
			" using the ${#%[1]s} bytes that had arrived (if that is 0 this call fails below)."+
			" A never-ending stream (tail -f, curl -N) or an inherited pipe nobody writes to always lands here —"+
			" pass the body with --%[3]s instead.\" >&2\n"+
			"    fi\n"+
			"  fi\n",
		param, extraGuard, flag)
}

// shellFuncUsageLine renders the "usage: foci_<name>[ [--json]][ <args>]"
// line the fallback bodies print on a rejected call. " [--json]" appears
// only when the tool has the --json OUTPUT flag (hasJSONOutputFlag): a tool
// like ask, whose --json is an input flag that takes a value, must not
// advertise a bare [--json].
func shellFuncUsageLine(t *Tool, args string) string {
	usage := "usage: foci_" + t.Name
	if hasJSONOutputFlag(t) {
		usage += " [--json]"
	}
	if args != "" {
		usage += " " + args
	}
	return usage
}

// generateGenericShellFunc emits a flag-parsing bash function for a tool from
// its JSON schema. Both --help text (via generateHelpText) and the body
// emitted here derive from the same schema, so the two cannot drift.
//
// Prior to this generator the default branch took $1 as a raw JSON object —
// flags advertised in --help were silently ignored, which is the bug fixed by
// TODO #723 (foci_remind --text rejected even though --help advertised it).
//
// Conventions:
//   - Snake_case schema keys become kebab-case flags: date_from -> --date-from
//   - String/integer/number/object/array params consume two args: --flag VALUE
//   - Boolean params are presence-only: --flag (sets variable to "true")
//   - Positional params (per Tool.Positional) accept bare args, joined
//     with a space when multiple arrive (matches existing query/text UX)
//   - Required params (per schema.Required) trigger a usage line on missing
//   - JSON-typed params (object/array/integer/number) are checked by
//     foci__json_arg before jq --argjson sees them, so a bad value names the
//     flag instead of printing jq internals twice (#1811)
//
// A schema with no properties to drive a flag parser gets a fallback body
// instead, split by WHY it is empty (#2271). A schema that parses with zero
// properties means the tool takes no arguments at all: any argument left
// after the prologue is a usage error naming the tool — the body used to
// hand $1 to jq --argjson unchecked, so `foci_whoami foo` printed jq's
// internals and `'' foo` silently dropped foo. A bare call still passes {}
// — without that default, jq --argjson dies on the empty string, so a
// zero-property tool (whoami, #1135) could not be called with no arguments
// at all. Only an unparseable schema keeps the legacy JSON-blob contract —
// no argument sends {}, one JSON object with any keys is sent as the params
// (the foci__json passthrough still works for callers that hand-construct
// it), and anything else is an error, the blob verified — slurped whole, so
// trailing garbage and concatenated objects fail too — before jq --argjson
// sees it.
func generateGenericShellFunc(t *Tool) string {
	name := "foci_" + t.Name
	helpText := generateHelpText(t)
	escapedHelp := strings.ReplaceAll(helpText, "'", "'\\''")
	helpCheck := fmt.Sprintf("  if [ \"${1:-}\" = \"-h\" ] || [ \"${1:-}\" = \"--help\" ]; then\n    echo '%s'\n    return 0\n  fi", escapedHelp)
	validKeys := toolParamKeys(t)
	guard := shellFuncPrologue(t, validKeys)

	var schema struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Format string `json:"format"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	schemaParses := json.Unmarshal(t.Parameters, &schema) == nil
	if !schemaParses || len(schema.Properties) == 0 {
		// No properties to drive a flag parser. The two emptiness causes
		// promise callers different things (#2271), so they get different
		// bodies over the same foci-call tail.
		if schemaParses {
			// Zero-property schema: the tool takes no arguments at all.
			// Reject whatever survived the prologue (--json was stripped,
			// the foci__json guard did not take it) BEFORE jq sees it: the
			// old body handed $1 to --argjson unchecked, printing jq
			// internals, and `set -- '{}'` on an empty $1 replaced ALL
			// arguments, so `'' foo` silently dropped foo. '' is an error
			// on purpose — "any argument is an error", like a schema
			// tool's unexpected positional; special-casing it would bring
			// the dropped-argument bug back. The bare call still
			// defaults to {} (#1135).
			return fmt.Sprintf(`%s() {
%s
%s
  if [ $# -gt 0 ]; then
    echo "error: %s takes no arguments, got '$1'" >&2
    echo "%s" >&2
    return 1
  fi
  set -- '{}'
%s`, name, helpCheck, guard, name, shellFuncUsageLine(t, ""), blobFociCallTail(t.Name))
		}
		// Unparseable schema: legacy JSON-blob contract — no argument sends
		// {}, exactly one JSON object (any keys; there is no schema to check
		// them against) is the params. Anything else is rejected before jq
		// --argjson sees it (#2271). The check slurps the WHOLE argument and
		// compares jq's stderr-silenced output (the foci__json_arg idiom):
		// slurping makes a parse error anywhere kill jq before the filter
		// runs — empty output — so trailing garbage (`{"a":1} x`) cannot
		// pass as its valid prefix, while every input that is not exactly
		// one value (empty string, concatenated objects) prints "many".
		// Comparing output rather than exit status is deliberate: jq exits
		// 0 on empty input, and a parse error already yields empty output,
		// so the one comparison covers both.
		usage := shellFuncUsageLine(t, "'<json-object>'")
		return fmt.Sprintf(`%s() {
%s
%s
  if [ $# -eq 0 ]; then
    set -- '{}'
  elif [ "$(printf '%%s' "$1" | jq -sr 'if length==1 then .[0]|type else "many" end' 2>/dev/null)" != object ]; then
    echo "error: %s takes a single JSON object argument, got '$1'" >&2
    echo "%s" >&2
    return 1
  elif [ $# -gt 1 ]; then
    echo "error: %s takes a single JSON object argument, got an extra argument '$2'" >&2
    echo "%s" >&2
    return 1
  fi
%s`, name, helpCheck, guard, name, usage, name, usage, blobFociCallTail(t.Name))
	}

	// Collect param names in stable (sorted) order so generated bash is
	// deterministic across builds.
	paramNames := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		paramNames = append(paramNames, k)
	}
	sort.Strings(paramNames)

	// Identify positional and required params.
	posSet := make(map[string]bool)
	var positional []string
	if pos, ok := positionalParamsForTool(t); ok {
		positional = pos
		for _, p := range pos {
			posSet[p] = true
		}
	}
	reqSet := make(map[string]bool)
	for _, r := range schema.Required {
		reqSet[r] = true
	}

	// filepathParams are string params with format:filepath. They gain
	// "-"-means-stdin support: passing `--file -` reads the attachment body
	// from stdin into a temp file rather than trying to open a literal file
	// named "-" (TODO #814). Only emitted when the tool actually has such a
	// param, so non-file tools (http_request is custom anyway) are untouched.
	var filepathParams []string
	for _, k := range paramNames {
		if schema.Properties[k].Format == "filepath" && schema.Properties[k].Type == "string" {
			filepathParams = append(filepathParams, k)
		}
	}
	hasStdinFile := len(filepathParams) > 0

	var b strings.Builder
	fmt.Fprintf(&b, "%s() {\n%s\n%s\n", name, helpCheck, guard)

	// Local declarations: every param has a string slot defaulted to empty.
	b.WriteString("  local")
	for _, k := range paramNames {
		fmt.Fprintf(&b, " %s=\"\"", k)
	}
	b.WriteString("\n")
	// Helper locals for the "-"-means-stdin file path (cleaned up after the
	// call). Declared only when a filepath param exists.
	if hasStdinFile {
		b.WriteString("  local __foci_stdin_file=\"\" __foci_rc=0\n")
	}
	// Origin tracking for a positional param, which also gets a --flag arm
	// below. Records which form supplied the value so giving BOTH is an error
	// rather than a silent merge (#1778): `foci_web_search --query a b` used to
	// concatenate into "a b", which is indistinguishable from a typo.
	if len(positional) == 1 {
		fmt.Fprintf(&b, "  local __foci_%s_via=\"\"\n", positional[0])
	}

	// Flag-parsing while-loop. Positional params still get a --flag arm so
	// callers can use either `foci_X --query foo` or `foci_X foo`. The
	// bare-arg case handles the second form below.
	b.WriteString("  while [ $# -gt 0 ]; do\n    case \"$1\" in\n")
	// posDup emits the both-forms-given guard for the positional param's flag
	// arms. Empty for every other param, so non-positional flags are untouched.
	posDup := func(k string) string {
		if len(positional) != 1 || positional[0] != k {
			return ""
		}
		return fmt.Sprintf("if [ \"$__foci_%s_via\" = pos ]; then echo \"error: %s was already given positionally; use --%s OR the positional form, not both\" >&2; return 1; fi; __foci_%s_via=flag; ",
			k, k, strings.ReplaceAll(k, "_", "-"), k)
	}
	var flagList []string
	for _, k := range paramNames {
		flag := strings.ReplaceAll(k, "_", "-")
		flagList = append(flagList, "--"+flag)
		if schema.Properties[k].Type == "boolean" {
			fmt.Fprintf(&b, "      --%s) %s%s=true; shift ;;\n", flag, posDup(k), k)
		} else {
			fmt.Fprintf(&b, "      --%s) %s%s=\"$2\"; shift 2 ;;\n", flag, posDup(k), k)
		}
		// Emit alias arms that set the same canonical variable. Aliases
		// silently skip if the canonical key isn't a schema property
		// (already verified by paramNames iteration).
		for _, alias := range t.Aliases[k] {
			aliasFlag := strings.ReplaceAll(alias, "_", "-")
			flagList = append(flagList, "--"+aliasFlag)
			if schema.Properties[k].Type == "boolean" {
				fmt.Fprintf(&b, "      --%s) %s%s=true; shift ;;\n", aliasFlag, posDup(k), k)
			} else {
				fmt.Fprintf(&b, "      --%s) %s%s=\"$2\"; shift 2 ;;\n", aliasFlag, posDup(k), k)
			}
		}
	}
	// A "valid flags" list can only ever list FLAGS, so when the caller wanted a
	// POSITIONAL the list is complete, correct and useless (#1778). Name the
	// positional form too — the same usage line --help already prints.
	posNote := ""
	if len(positional) == 1 {
		posNote = fmt.Sprintf("        echo \"note: %s <%s> [flags...] — <%s> may also be given as a bare argument\" >&2\n", name, positional[0], positional[0])
	}
	fmt.Fprintf(&b,
		"      --*)\n        echo \"error: unrecognized flag: $1\" >&2\n        echo \"valid flags: %s\" >&2\n%s        return 1 ;;\n",
		strings.Join(flagList, " "), posNote,
	)

	// Positional arg handling.
	switch len(positional) {
	case 0:
		b.WriteString("      *)\n        echo \"error: unexpected positional argument: $1\" >&2\n        return 1 ;;\n")
	case 1:
		// Multi-word join — matches existing query/prompt UX.
		p := positional[0]
		fmt.Fprintf(&b, "      *)\n        if [ \"$__foci_%s_via\" = flag ]; then echo \"error: %s was already given as --%s; use --%s OR the positional form, not both\" >&2; return 1; fi\n        __foci_%s_via=pos; %s=\"$%s $1\"; shift ;;\n",
			p, p, strings.ReplaceAll(p, "_", "-"), strings.ReplaceAll(p, "_", "-"), p, p, p)
	default:
		// No current tool uses multiple positional params. Bail rather than
		// emit unverified code.
		b.WriteString("      *)\n        echo \"error: multiple positional args not supported by generic generator\" >&2\n        return 1 ;;\n")
	}
	b.WriteString("    esac\n  done\n")

	// Trim leading space from joined single-positional.
	if len(positional) == 1 {
		p := positional[0]
		fmt.Fprintf(&b, "  %s=\"${%s# }\"\n", p, p)
	}

	// Stdin-to-tempfile for filepath params: `--file -` reads the attachment
	// body from stdin into a temp file (TODO #814). Must run BEFORE the
	// relative-path resolver below, so "-" is replaced by the tempfile's
	// absolute path and never becomes "$PWD/-". mktemp returns an absolute
	// path, so the resolver's /*) arm then leaves it unchanged.
	for _, k := range filepathParams {
		fmt.Fprintf(&b,
			"  if [ \"$%s\" = \"-\" ]; then\n"+
				"    if [ -n \"$__foci_stdin_file\" ]; then\n"+
				"      echo \"error: only one '-' (stdin) file argument is supported\" >&2\n"+
				"      return 1\n"+
				"    fi\n"+
				"    __foci_stdin_file=\"$(mktemp)\"\n"+
				"    cat > \"$__foci_stdin_file\"\n"+
				"    %s=\"$__foci_stdin_file\"\n"+
				"  fi\n",
			k, k,
		)
	}

	// Resolve relative paths for params with format: filepath. The shell
	// function inherits the caller's cwd; foci-gw's cwd is unrelated, so
	// relative paths sent verbatim fail with confusing "no such file" errors
	// (TODO #754). POSIX case: leave absolute paths unchanged, prefix
	// relatives with $PWD. filepath.Clean on the receive side normalises
	// any . / .. segments.
	for _, k := range paramNames {
		if schema.Properties[k].Format != "filepath" {
			continue
		}
		if schema.Properties[k].Type != "string" {
			continue
		}
		fmt.Fprintf(&b,
			"  [ -n \"$%s\" ] && case \"$%s\" in /*) ;; *) %s=\"$PWD/$%s\" ;; esac\n",
			k, k, k, k,
		)
	}

	// Stdin reader: if the StdinParam value is empty and stdin is not a TTY,
	// read stdin into the variable. Lets pipe usage Just Work — `echo hi |
	// foci_send_to_chat` populates text from stdin without --text.
	if t.StdinParam != "" {
		if _, ok := schema.Properties[t.StdinParam]; !ok {
			return fmt.Sprintf("# error: tool %q StdinParam=%q not in schema\n", t.Name, t.StdinParam)
		}
		// When a filepath param already consumed stdin (`--file -`), don't
		// also drain it into the StdinParam — stdin is single-use.
		extraGuard := ""
		if hasStdinFile {
			extraGuard = " && [ -z \"$__foci_stdin_file\" ]"
		}
		// An explicit "-" means "read this field from stdin", mirroring `--file -`;
		// normalise it to empty so the reader below fills it from the pipe rather
		// than sending a literal "-" (#1007).
		fmt.Fprintf(&b, "  if [ \"$%s\" = \"-\" ]; then %s=\"\"; fi\n", t.StdinParam, t.StdinParam)
		// Guard: StdinParam set + stdin piped = footgun. The reader below
		// skips non-empty values, so piped content would be silently
		// discarded. Error instead. Skipped when --file - already consumed
		// stdin (then --text is a legitimate caption for the attached file).
		stdinFlag := strings.ReplaceAll(t.StdinParam, "_", "-")
		suggestion := fmt.Sprintf(
			"To send piped content as the message body, omit --%s or use --%s -",
			stdinFlag, stdinFlag)
		if hasStdinFile {
			suggestion += ". To attach a file, use --file \\$path"
		}
		fmt.Fprintf(&b,
			// head -c 1 | wc -c (not `read -N 1`) because bash's read can't store
			// NUL bytes in a variable — on all-NUL piped input it silently misses
			// the guard while still consuming the whole stream. head/wc operate on
			// raw bytes, so NUL-safe and short-circuits on the first byte either way.
			//
			// BOUNDED (`timeout 5`), because this branch does not even want stdin:
			// the body already arrived as an argument, and the only thing left to do
			// with the pipe is warn that it will be discarded. A courtesy check must
			// not be able to hang the send it is decorating.
			//
			// [ ! -t 0 ] cannot prevent that on its own — it is an isatty check,
			// i.e. "am I non-interactive", equally true of a pipe with data, a pipe
			// whose writer has not written yet, /dev/null, a regular file, and an fd
			// a parent process merely left open. That last shape is #1552: a
			// cron/daemon context inherits a pipe nobody will ever write to or
			// close, so an unbounded `head -c 1` waits for a byte or an EOF that
			// never come, and a call whose message body was supplied in full
			// produces nothing and dies on its caller's timeout.
			//
			// The bound is a wait, deliberately, NOT a non-blocking poll. `read -t 0`
			// looks like the right tool and is not: in `cmd | foci_send_to_chat
			// --text x` the function reaches this guard before upstream has written
			// byte one, so the poll reports "no data" and the warning is skipped on
			// the most ordinary pipeline there is (measured: the 10MB case below
			// went green-but-wrong). Waiting is correct here; waiting FOREVER is the
			// bug. 5s keeps the warning for every realistic writer and caps the
			// pathological fd at a rounding error.
			"  if [ -n \"$%[1]s\" ] && [ ! -t 0 ]%[2]s; then\n"+
				"    if [ \"$(timeout 5 head -c 1 | wc -c)\" -gt 0 ]; then\n"+
				"      echo \"error: --%s is set but stdin has piped content that will be discarded. %s.\" >&2\n"+
				"      return 1\n"+
				"    fi\n"+
				"  fi\n",
			t.StdinParam, extraGuard, stdinFlag, suggestion)
		// The sibling branch: no value given, so stdin IS the body and reading
		// it is the job. stdinReadSnippet owns that branch's rationale and its
		// text; the ask wrapper emits the same snippet for its questions JSON.
		b.WriteString(stdinReadSnippet(t.StdinParam, extraGuard, stdinFlag))
	}

	// Required-param usage check.
	if len(schema.Required) > 0 {
		var conditions []string
		for _, r := range schema.Required {
			// Boolean required params can't use -z (an unset var renders as
			// empty); treat them as "must be set to true".
			if schema.Properties[r].Type == "boolean" {
				conditions = append(conditions, fmt.Sprintf("[ \"$%s\" != true ]", r))
			} else {
				conditions = append(conditions, fmt.Sprintf("[ -z \"$%s\" ]", r))
			}
		}
		var usage strings.Builder
		usage.WriteString("usage: ")
		usage.WriteString(name)
		for _, p := range positional {
			fmt.Fprintf(&usage, " <%s>", p)
		}
		for _, k := range paramNames {
			if posSet[k] || !reqSet[k] {
				continue
			}
			flag := strings.ReplaceAll(k, "_", "-")
			if schema.Properties[k].Type == "boolean" {
				fmt.Fprintf(&usage, " --%s", flag)
			} else {
				fmt.Fprintf(&usage, " --%s <%s>", flag, k)
			}
		}
		fmt.Fprintf(&b,
			"  if %s; then\n    echo \"%s\" >&2\n    return 1\n  fi\n",
			strings.Join(conditions, " || "),
			usage.String(),
		)
	}

	// Build the params object with jq, type-aware. Strings use --arg; other
	// JSON-valued types use --argjson so jq validates the value as JSON.
	b.WriteString("  local params=\"{}\"\n")
	for _, k := range paramNames {
		ty := schema.Properties[k].Type
		switch ty {
		case "boolean":
			fmt.Fprintf(&b,
				"  [ \"$%s\" = true ] && params=\"$(echo \"$params\" | jq '. + {%s: true}')\"\n",
				k, k,
			)
		case "string":
			fmt.Fprintf(&b,
				"  [ -n \"$%s\" ] && params=\"$(echo \"$params\" | jq --arg v \"$%s\" '. + {%s: $v}')\"\n",
				k, k, k,
			)
		default:
			// integer, number, object, array — validated before jq sees it so a
			// bad value names the flag. jq's own --argjson error names neither
			// the flag nor the value, and prints twice because the first failure
			// leaves $params empty and the next jq rejects that too (#1811).
			want := ty
			switch want {
			case "integer":
				want = "number"
			case "":
				want = "any"
			}
			fmt.Fprintf(&b,
				"  if [ -n \"$%s\" ]; then\n"+
					"    foci__json_arg --%s %s \"$%s\" || return 1\n"+
					"    params=\"$(echo \"$params\" | jq --argjson v \"$%s\" '. + {%s: $v}')\"\n"+
					"  fi\n",
				k, strings.ReplaceAll(k, "_", "-"), want, k, k, k,
			)
		}
	}

	if hasStdinFile {
		// Capture the call's exit, remove any stdin tempfile, then propagate
		// the original exit code.
		fmt.Fprintf(&b,
			"  foci-call \"$(jq -nc --argjson p \"$params\" '{\"tool\":\"%s\",\"params\":$p}')\" || __foci_rc=$?\n"+
				"  [ -n \"$__foci_stdin_file\" ] && rm -f \"$__foci_stdin_file\"\n"+
				"  return $__foci_rc\n}\n",
			t.Name,
		)
	} else {
		fmt.Fprintf(&b,
			"  foci-call \"$(jq -nc --argjson p \"$params\" '{\"tool\":\"%s\",\"params\":$p}')\"\n}\n",
			t.Name,
		)
	}

	return b.String()
}
