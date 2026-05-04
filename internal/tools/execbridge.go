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

	"foci/internal/log"
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

// NewExecBridgeStable creates an exec bridge with a stable socket path derived
// from stableID (typically a foci session key). The socket survives process
// restarts: on creation, any existing stale socket at the same path is removed
// and a new listener is started. This allows long-lived backend sessions (e.g.
// Claude Code) to reconnect after a foci restart without needing new env vars.
func NewExecBridgeStable(registry *Registry, ctx context.Context, stableID string) (*ExecBridge, error) {
	// Sanitize: session keys may contain slashes (e.g. "telegram/c123/456")
	safe := strings.ReplaceAll(stableID, "/", "-")
	sockPath := fmt.Sprintf("%s/exec-%s.sock", tempdir.Dir(), safe)
	funcsPath := fmt.Sprintf("%s/exec-%s-funcs.sh", tempdir.Dir(), safe)

	// Remove stale socket from a previous process (if any).
	_ = os.Remove(sockPath)

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

	log.Debugf("execbridge", "session=%s started sock=%s tools=%d", SessionKeyFromContext(ctx), sockPath, b.exportedToolCount())
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
	log.Debugf("execbridge", "closed sock=%s", b.sockPath)
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
		writeResponse(conn, "", fmt.Sprintf("invalid request: %v", err))
		return
	}

	tool := b.registry.Get(req.Tool)
	if tool == nil {
		writeResponse(conn, "", fmt.Sprintf("unknown tool: %s", req.Tool))
		return
	}
	if !tool.ExecExport {
		writeResponse(conn, "", fmt.Sprintf("tool %s not exported for exec", req.Tool))
		return
	}

	log.Debugf("execbridge", "session=%s call tool=%s", SessionKeyFromContext(b.ctx), req.Tool)
	result, err := tool.Execute(b.ctx, req.Params)
	if err != nil {
		writeResponse(conn, "", err.Error())
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

	writeResponse(conn, text, "")
}

func writeResponse(conn net.Conn, result, errMsg string) {
	resp := struct {
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}{Result: result, Error: errMsg}
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

// shellPositionalParams is the legacy fallback for tools that haven't yet
// migrated to the Tool.Positional field. New tools should declare positional
// params on the Tool struct itself; positionalParamsForTool prefers the
// struct field over this map. Once all entries here are also set on their
// respective Tool structs, the map can be removed.
var shellPositionalParams = map[string][]string{
	"http_request": {"url"},
	"todo":         {"action"},
	"summary":      {"prompt"},
	"tmux":         {"operation"},
}

// positionalParamsForTool returns a tool's positional schema params. Prefers
// the Tool.Positional struct field; falls back to the legacy
// shellPositionalParams map. The bool reports whether any are declared.
func positionalParamsForTool(t *Tool) ([]string, bool) {
	if len(t.Positional) > 0 {
		return t.Positional, true
	}
	if pos, ok := shellPositionalParams[t.Name]; ok {
		return pos, true
	}
	return nil, false
}

// generateHelpText builds a help string for a tool from its description and JSON schema.
func generateHelpText(t *Tool) string {
	var b strings.Builder
	b.WriteString(t.Description)

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
	}

	// Extract parameter info from JSON schema.
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(t.Parameters, &schema) == nil && len(schema.Properties) > 0 {
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
	{"add", "add --text TEXT [--priority high|medium|low] [--tag TAGS]", "--text --priority --tag"},
	{"list", "list [--tag T] [--status open|done|dropped|all] [--priority P] [--sort F] [--reverse] [--limit N]", "--tag --status --priority --sort --reverse --limit"},
	{"list-all", "list-all [--tag T] [--priority P] [--sort F] [--reverse] [--limit N]", "--tag --priority --sort --reverse --limit"},
	{"search", "search <query> [--sort F] [--reverse] [--limit N]", "--sort --reverse --limit"},
	{"get", "get <id>", ""},
	{"complete", "complete <id> [--reason TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason"},
	{"drop", "drop <id> [--reason TEXT]   (or --id N / --ids 1,2,3)", "--id --ids --reason"},
	{"edit", "edit --id N [--text TEXT] [--priority P] [--tag T]", "--id --ids --text --priority --tag"},
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
  local text="" priority="" tag="" query="" status="" id="" ids="" reason="" sort="" reverse="" limit=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --text) text="$2"; shift 2 ;;
      --priority) priority="$2"; shift 2 ;;
      --tag) tag="$2"; shift 2 ;;
      --query) query="$2"; shift 2 ;;
      --status) status="$2"; shift 2 ;;
      --id) id="$2"; shift 2 ;;
      --ids) ids="$2"; shift 2 ;;
      --reason) reason="$2"; shift 2 ;;
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
          echo "valid flags: --text --priority --tag --query --status --id --ids --reason --sort --reverse --limit" >&2
        fi
        return 1 ;;
      *) # positional: first positional is text/query/id depending on action
        case "$action" in
          add|edit) text="$text $1" ;;
          search) query="$query $1" ;;
          get|complete|drop|remove) id="$1" ;;
        esac
        shift ;;
    esac
  done
  text="${text# }"
  query="${query# }"
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
`, name, todoHelpCheck, guard, todoActionsBashCase(), name)

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
//   - Positional params (per shellPositionalParams) accept bare args, joined
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
			Type string `json:"type"`
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

	var b strings.Builder
	fmt.Fprintf(&b, "%s() {\n%s\n%s\n", name, helpCheck, guard)

	// Local declarations: every param has a string slot defaulted to empty.
	b.WriteString("  local")
	for _, k := range paramNames {
		fmt.Fprintf(&b, " %s=\"\"", k)
	}
	b.WriteString("\n")

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

	// Stdin reader: if the StdinParam value is empty and stdin is not a TTY,
	// read stdin into the variable. Lets pipe usage Just Work — `echo hi |
	// foci_send_to_chat` populates text from stdin without --text.
	if t.StdinParam != "" {
		if _, ok := schema.Properties[t.StdinParam]; !ok {
			return fmt.Sprintf("# error: tool %q StdinParam=%q not in schema\n", t.Name, t.StdinParam)
		}
		fmt.Fprintf(&b, "  if [ -z \"$%s\" ] && [ ! -t 0 ]; then\n    %s=\"$(cat)\"\n  fi\n", t.StdinParam, t.StdinParam)
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

	fmt.Fprintf(&b,
		"  foci-call \"$(jq -nc --argjson p \"$params\" '{\"tool\":\"%s\",\"params\":$p}')\"\n}\n",
		t.Name,
	)

	return b.String()
}
