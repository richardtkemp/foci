package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"foci/internal/peercred"
	"foci/internal/tempdir"
)

// bridgeCounter provides unique socket paths across concurrent exec calls.
var bridgeCounter atomic.Int64

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
	go b.acceptLoop()

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

func (b *ExecBridge) acceptLoop() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			// Expected when listener is closed
			return
		}
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
		Tool           string          `json:"tool"`
		Params         json.RawMessage `json:"params"`
		IncludeHeaders bool            `json:"include_headers,omitempty"`
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
	result, err := tool.Execute(b.ctx, req.Params)
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

	// Strip HTTP headers from http_request results so piping works cleanly.
	// The tool returns "HTTP <status>\nHeader: val\n...\n\n<body>" — in a pipe
	// context only the body is useful (e.g. `foci_http_request url | jq .`).
	// Pass --include-headers to foci_http_request to keep status/headers.
	text := result.Text
	if req.Tool == "http_request" && !req.IncludeHeaders {
		text = stripHTTPHeaders(text)
	}

	// When the tool spilled the full result to disk (large http body, etc.),
	// pass the file pointer through instead of inlining megabytes onto the
	// socket. foci-call streams the file straight to stdout, so a pipe
	// (`foci_http_request url | jq`) gets the complete body and CC applies its
	// own output truncation to the final result. text remains the inline
	// preview for the non-spilled case / fallback.
	writeBridgeResponse(conn, bridgeResponse{
		Result:     text,
		ResultFile: result.ResultFile,
		ResultSize: result.ResultSize,
	})
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

// stripHTTPHeaders removes the HTTP status/header block from an http_request
// result, returning only the body. The format is "HTTP <status>\n...headers...\n\n<body>".
// If the separator isn't found, the result is returned unchanged.
func stripHTTPHeaders(result string) string {
	if idx := strings.Index(result, "\n\n"); idx >= 0 && strings.HasPrefix(result, "HTTP ") {
		return result[idx+2:]
	}
	return result
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
//	foci__json "tool" "key1 key2 key3" "$@" && return $?
//
// The guard fires only when ALL three conditions are met:
//  1. Exactly one argument provided
//  2. It parses as a JSON object (not array, string, number, etc.)
//  3. Every key in the parsed object is a valid parameter name for the tool
//
// This prevents false positives when a single positional arg happens to
// look like JSON (e.g. searching for a JSON string).
//
// Note: helpers use foci__ prefix (not _foci_) because Claude Code's shell
// snapshot mechanism filters out underscore-prefixed functions.
const jsonPassthroughHelper = `# Trace helper: logs to stderr when FOCI_TRACE is set.
foci__trace() { [ -n "${FOCI_TRACE:-}" ] && echo "FOCI_TRACE[$1]: ${*:2}" >&2; return 0; }
export -f foci__trace

# JSON passthrough: if the sole arg is a JSON object with valid param keys, use it directly.
foci__json() {
  local tool="$1" valid_keys="$2"; shift 2
  [ $# -eq 1 ] || return 1
  [ "${1:0:1}" = "{" ] || return 1
  # Verify it parses as an object and every key is a valid param name.
  local keys
  keys="$(echo "$1" | jq -r 'if type=="object" then keys[] else error end' 2>/dev/null)" || return 1
  for k in $keys; do
    case " $valid_keys " in
      *" $k "*) ;;
      *) return 1 ;;
    esac
  done
  foci-call "$(jq -nc --argjson p "$1" '{"tool":"'"$tool"'","params":$p}')"
}
export -f foci__json

`

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

	return os.WriteFile(b.funcsPath, []byte(sb.String()), 0600)
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
		// Tools with empty schemas use the JSON-blob fallback; nothing to validate.
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
	return b.String()
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
	{"add", "add --text TEXT [--body TEXT] [--title TEXT] [--priority high|medium|low] [--tag TAGS]   (alias: create; --title prepended in bold to --body/--text)", "--text --body --title --priority --tag"},
	{"list", "list [--tag T] [--status open|done|dropped|all] [--priority P] [--sort F] [--reverse] [--limit N]", "--tag --status --priority --sort --reverse --limit"},
	{"list-all", "list-all [--tag T] [--priority P] [--sort F] [--reverse] [--limit N]", "--tag --priority --sort --reverse --limit"},
	{"search", "search <query> [--sort F] [--reverse] [--limit N]", "--sort --reverse --limit"},
	{"get", "get <id>", ""},
	{"complete", "complete <id> [--reason|--notes|--note|--text TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason --notes --note --text"},
	{"drop", "drop <id> [--reason|--notes|--note|--text TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason --notes --note --text"},
	{"edit", "edit <id> [--text TEXT] [--append-text|--note|--add TEXT] [--append] [--priority P] [--tag T]   (alias: update; or --id N / --ids 1,2,3)", "--id --ids --text --append --append-text --add --note --notes --priority --tag"},
	{"remove", "remove --id N   (or --ids 1,2,3)", "--id --ids"},
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
	guard := fmt.Sprintf("  foci__json %q %q \"$@\" && return $?", t.Name, validKeys)

	switch t.Name {
	case "http_request":
		// URL as first arg, flags for method, headers, body, save_to, etc.
		return fmt.Sprintf(`%s() {
%s
%s
  local url="" method="GET" body="" body_file="" save_to="" save_json_path="" headers="{}" query="{}" inc_headers=false background=false timeout="" max_bytes="" files="[]" form_fields="{}"
  while [ $# -gt 0 ]; do
    case "$1" in
      --method) method="$2"; shift 2 ;;
      --body) body="$2"; shift 2 ;;
      --body-file) body_file="$2"; shift 2 ;;
      --header) headers="$(echo "$headers" | jq --arg k "${2%%%%:*}" --arg v "${2#*: }" '. + {($k): $v}')"; shift 2 ;;
      --headers) headers="$2"; shift 2 ;;
      --query) query="$2"; shift 2 ;;
      --save-to) save_to="$2"; shift 2 ;;
      --save-from-json-path) save_json_path="$2"; shift 2 ;;
      --timeout) timeout="$2"; shift 2 ;;
      --max-response-bytes) max_bytes="$2"; shift 2 ;;
      --files) files="$2"; shift 2 ;;
      --form-fields) form_fields="$2"; shift 2 ;;
      --background) background=true; shift ;;
      --include-headers) inc_headers=true; shift ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid flags: --method --body --body-file --header --headers --query --save-to --save-from-json-path --timeout --max-response-bytes --files --form-fields --background --include-headers" >&2
        return 1 ;;
      *) url="$1"; shift ;;
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
  [ -n "$save_to" ] && params="$(echo "$params" | jq --arg s "$save_to" '. + {save_to: $s}')"
  [ -n "$save_json_path" ] && params="$(echo "$params" | jq --arg s "$save_json_path" '. + {save_from_json_path: $s}')"
  [ -n "$timeout" ] && params="$(echo "$params" | jq --argjson t "$timeout" '. + {timeout: $t}')"
  [ -n "$max_bytes" ] && params="$(echo "$params" | jq --argjson m "$max_bytes" '. + {max_response_bytes: $m}')"
  [ "$background" = true ] && params="$(echo "$params" | jq '. + {background: true}')"
  [ "$query" != "{}" ] && params="$(echo "$params" | jq --argjson q "$query" '. + {query: $q}')"
  [ "$files" != "[]" ] && params="$(echo "$params" | jq --argjson f "$files" '. + {files: $f}')"
  [ "$form_fields" != "{}" ] && params="$(echo "$params" | jq --argjson f "$form_fields" '. + {form_fields: $f}')"
  foci-call "$(jq -nc --argjson p "$params" --argjson ih "$inc_headers" '{"tool":"http_request","params":$p,"include_headers":$ih}')"
}
`, name, helpCheck, guard, name)

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
  if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    if [ -n "$action_usage" ]; then
      echo "usage: foci_todo $action_usage"
    else
      echo "usage: foci_todo <add|list|list-all|search|get|complete|drop|edit|remove> [args...]"
      echo "Run 'foci_todo --help' for full help."
    fi
    return 0
  fi
  local text="" priority="" tag="" query="" status="" id="" ids="" reason="" sort="" reverse="" limit="" append="" append_text="" body="" title=""
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
      --tag) tag="$2"; shift 2 ;;
      --query) query="$2"; shift 2 ;;
      --status) status="$2"; shift 2 ;;
      --id) id="$2"; shift 2 ;;
      --ids) ids="$2"; shift 2 ;;
      --reason) reason="$2"; shift 2 ;;
      --notes) reason="$2"; shift 2 ;;
      --note) reason="$2"; shift 2 ;;
      --append-text) append_text="$2"; shift 2 ;;
      --add) append_text="$2"; shift 2 ;;
      --append) append=true; shift ;;
      --sort) sort="$2"; shift 2 ;;
      --limit) limit="$2"; shift 2 ;;
      --reverse) reverse=true; shift ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        if [ -n "$action_flags" ]; then
          echo "valid flags for '$action': $action_flags" >&2
        elif [ -n "$action" ]; then
          echo "'$action' takes no flags" >&2
        else
          echo "valid flags: --text --priority --tag --query --status --id --ids --reason --notes --note --append --append-text --add --sort --reverse --limit" >&2
        fi
        return 1 ;;
      *) # positional: first positional is text/query/id depending on action
        case "$action" in
          add) text="$text $1" ;;
          search) query="$query $1" ;;
          get|complete|drop|remove) id="$1" ;;
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
  # #941: --body is an alias for the todo text; --title (if set) is prepended in bold.
  if [ "$action" = add ]; then
    [ -n "$body" ] && text="$body"
    if [ -n "$title" ]; then
      if [ -n "$text" ]; then text="$(printf '*%%s*\n\n%%s' "$title" "$text")"; else text="*$title*"; fi
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
  fi
  case "$action" in
    add)
      local params='{"action":"add"}'
      [ -n "$text" ] && params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
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
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    list-all)
      local params='{"action":"list","status":"all"}'
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"
      [ -n "$reverse" ] && params="$(echo "$params" | jq '. + {reverse: true}')"
      [ -n "$limit" ] && params="$(echo "$params" | jq --argjson l "$limit" '. + {limit: $l}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    search)
      local params='{"action":"search"}'
      [ -n "$query" ] && params="$(echo "$params" | jq --arg q "$query" '. + {query: $q}')"
      [ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"
      [ -n "$reverse" ] && params="$(echo "$params" | jq '. + {reverse: true}')"
      [ -n "$limit" ] && params="$(echo "$params" | jq --argjson l "$limit" '. + {limit: $l}')"
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
    edit)
      local params='{"action":"edit"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      [ -n "$text" ] && params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
      [ -n "$append" ] && params="$(echo "$params" | jq '. + {append: true}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    remove)
      local params='{"action":"remove"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$ids" ] && params="$(echo "$params" | jq --argjson i "$ids" '. + {ids: $i}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    *)
      echo "usage: %s <add|list|list-all|search|get|complete|drop|edit|remove> [args...]" >&2
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
  while [ $# -gt 0 ]; do
    case "$1" in
      --file) file="$2"; shift 2 ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid flags: --file" >&2
        return 1 ;;
      *) prompt="$prompt $1"; shift ;;
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
  fi
  if [ -z "$file" ]; then
    echo "error: no input — provide --file or pipe stdin" >&2
    return 1
  fi
  foci-call "$(jq -nc --arg f "$file" --arg p "$prompt" '{"tool":"summary","params":{"file":$f,"prompt":$p}}')"
}
`, name, helpCheck, guard, name, name)

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
      --watch) watch="$2"; shift 2 ;;
      --keys) keys="$2"; shift 2 ;;
      --enter) enter="$2"; shift 2 ;;
      --lines) lines="$2"; shift 2 ;;
      --window) window="$2"; shift 2 ;;
      --threshold-seconds) threshold_seconds="$2"; shift 2 ;;
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
		// by the foci__json passthrough guard), via --json, or piped on stdin.
		// The optional grader params may live INSIDE that JSON object, or be
		// supplied as flags (merged in below) for CLI convenience. Async tool —
		// returns immediately after posting the first question.
		return fmt.Sprintf(`%s() {
%s
%s
  local json="" grader="" grader_args="" grader_timeout="" grader_on_error=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --json) json="$2"; shift 2 ;;
      --grader) grader="$2"; shift 2 ;;
      --grader-args) grader_args="$2"; shift 2 ;;
      --grader-timeout-seconds) grader_timeout="$2"; shift 2 ;;
      --grader-on-error) grader_on_error="$2"; shift 2 ;;
      --*)
        echo "error: unrecognized flag: $1" >&2
        echo "valid: --json --grader --grader-args --grader-timeout-seconds --grader-on-error (or pass JSON positionally, or pipe it on stdin)" >&2
        return 1 ;;
      *) json="$1"; shift ;;
    esac
  done
  if [ -z "$json" ] && [ ! -t 0 ]; then
    json="$(cat)"
  fi
  if [ -z "$json" ]; then
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
`, name, helpCheck, guard, name, name, name)

	default:
		// Schema-driven generic: emits a flag-parsing function whose
		// accepted flags are exactly those advertised by generateHelpText.
		// This is the default path for any tool that doesn't have
		// hand-rolled UX (stdin reading, accumulator flags, subcommand
		// dispatch). See generateGenericShellFunc for behavior details.
		return generateGenericShellFunc(t)
	}
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
//   - JSON-typed params (object/array/integer/number) use jq --argjson, so jq
//     validates the value at parse time
//
// If the schema is unparseable or empty the function falls back to the legacy
// JSON-blob behavior so the foci__json passthrough still works for callers
// that hand-construct the params object.
func generateGenericShellFunc(t *Tool) string {
	name := "foci_" + t.Name
	helpText := generateHelpText(t)
	escapedHelp := strings.ReplaceAll(helpText, "'", "'\\''")
	helpCheck := fmt.Sprintf("  if [ \"${1:-}\" = \"-h\" ] || [ \"${1:-}\" = \"--help\" ]; then\n    echo '%s'\n    return 0\n  fi", escapedHelp)
	validKeys := toolParamKeys(t)
	guard := fmt.Sprintf("  foci__json %q %q \"$@\" && return $?", t.Name, validKeys)

	var schema struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Format string `json:"format"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(t.Parameters, &schema); err != nil || len(schema.Properties) == 0 {
		// Fallback to legacy JSON-blob behavior when schema unavailable.
		return fmt.Sprintf(`%s() {
%s
%s
  foci-call "$(jq -nc --argjson p "$1" '{"tool":"%s","params":$p}')"
}
`, name, helpCheck, guard, t.Name)
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

	// Flag-parsing while-loop. Positional params still get a --flag arm so
	// callers can use either `foci_X --query foo` or `foci_X foo`. The
	// bare-arg case handles the second form below.
	b.WriteString("  while [ $# -gt 0 ]; do\n    case \"$1\" in\n")
	var flagList []string
	for _, k := range paramNames {
		flag := strings.ReplaceAll(k, "_", "-")
		flagList = append(flagList, "--"+flag)
		if schema.Properties[k].Type == "boolean" {
			fmt.Fprintf(&b, "      --%s) %s=true; shift ;;\n", flag, k)
		} else {
			fmt.Fprintf(&b, "      --%s) %s=\"$2\"; shift 2 ;;\n", flag, k)
		}
		// Emit alias arms that set the same canonical variable. Aliases
		// silently skip if the canonical key isn't a schema property
		// (already verified by paramNames iteration).
		for _, alias := range t.Aliases[k] {
			aliasFlag := strings.ReplaceAll(alias, "_", "-")
			flagList = append(flagList, "--"+aliasFlag)
			if schema.Properties[k].Type == "boolean" {
				fmt.Fprintf(&b, "      --%s) %s=true; shift ;;\n", aliasFlag, k)
			} else {
				fmt.Fprintf(&b, "      --%s) %s=\"$2\"; shift 2 ;;\n", aliasFlag, k)
			}
		}
	}
	fmt.Fprintf(&b,
		"      --*)\n        echo \"error: unrecognized flag: $1\" >&2\n        echo \"valid flags: %s\" >&2\n        return 1 ;;\n",
		strings.Join(flagList, " "),
	)

	// Positional arg handling.
	switch len(positional) {
	case 0:
		b.WriteString("      *)\n        echo \"error: unexpected positional argument: $1\" >&2\n        return 1 ;;\n")
	case 1:
		// Multi-word join — matches existing query/prompt UX.
		p := positional[0]
		fmt.Fprintf(&b, "      *) %s=\"$%s $1\"; shift ;;\n", p, p)
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
			"  if [ -n \"$%s\" ] && [ ! -t 0 ]%s; then\n"+
				"    if [ \"$(head -c 1 | wc -c)\" -gt 0 ]; then\n"+
				"      echo \"error: --%s is set but stdin has piped content that will be discarded. %s.\" >&2\n"+
				"      return 1\n"+
				"    fi\n"+
				"  fi\n",
			t.StdinParam, extraGuard, stdinFlag, suggestion)
		fmt.Fprintf(&b, "  if [ -z \"$%s\" ] && [ ! -t 0 ]%s; then\n    %s=\"$(cat)\"\n  fi\n", t.StdinParam, extraGuard, t.StdinParam)
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
			// integer, number, object, array — jq validates value as JSON.
			fmt.Fprintf(&b,
				"  [ -n \"$%s\" ] && params=\"$(echo \"$params\" | jq --argjson v \"$%s\" '. + {%s: $v}')\"\n",
				k, k, k,
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
