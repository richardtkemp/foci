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

	"foci/log"
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
func NewExecBridge(registry *Registry, ctx context.Context) (*ExecBridge, error) {
	n := bridgeCounter.Add(1)
	sockPath := fmt.Sprintf("/tmp/foci-exec-%d-%d.sock", os.Getpid(), n)
	funcsPath := fmt.Sprintf("/tmp/foci-exec-%d-%d-funcs.sh", os.Getpid(), n)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("exec bridge listen: %w", err)
	}
	// Restrict socket access
	if err := os.Chmod(sockPath, 0600); err != nil {
		listener.Close()
		os.Remove(sockPath)
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
		listener.Close()
		os.Remove(sockPath)
		cancel()
		return nil, fmt.Errorf("exec bridge write funcs: %w", err)
	}

	// Start accept loop
	b.wg.Add(1)
	go b.acceptLoop()

	log.Debugf("execbridge", "started sock=%s tools=%d", sockPath, b.exportedToolCount())
	return b, nil
}

// SockPath returns the unix socket path for FOCI_SOCK env var.
func (b *ExecBridge) SockPath() string { return b.sockPath }

// FuncsPath returns the shell functions file path.
func (b *ExecBridge) FuncsPath() string { return b.funcsPath }

// Close stops the listener, waits for in-flight connections, and removes files.
func (b *ExecBridge) Close() {
	b.cancel()
	b.listener.Close()
	b.wg.Wait()
	os.Remove(b.sockPath)
	os.Remove(b.funcsPath)
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
	defer conn.Close()

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

	log.Debugf("execbridge", "call tool=%s", req.Tool)
	result, err := tool.Execute(b.ctx, req.Params)
	if err != nil {
		writeResponse(conn, "", err.Error())
		return
	}

	// Strip HTTP headers from http_request results so piping works cleanly.
	// The tool returns "HTTP <status>\nHeader: val\n...\n\n<body>" — in a pipe
	// context only the body is useful (e.g. `foci_http_request url | jq .`).
	// Pass --include-headers to foci_http_request to keep status/headers.
	if req.Tool == "http_request" && !req.IncludeHeaders {
		result = stripHTTPHeaders(result)
	}

	writeResponse(conn, result, "")
}

func writeResponse(conn net.Conn, result, errMsg string) {
	resp := struct {
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}{Result: result, Error: errMsg}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	conn.Write(data)
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
//	_foci_json "tool" "key1 key2 key3" "$@" && return $?
//
// The guard fires only when ALL three conditions are met:
//  1. Exactly one argument provided
//  2. It parses as a JSON object (not array, string, number, etc.)
//  3. Every key in the parsed object is a valid parameter name for the tool
//
// This prevents false positives when a single positional arg happens to
// look like JSON (e.g. searching for a JSON string).
const jsonPassthroughHelper = `# JSON passthrough: if the sole arg is a JSON object with valid param keys, use it directly.
_foci_json() {
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
export -f _foci_json

`

// writeShellFuncs generates a bash file defining foci_<toolname>() for each
// exported tool. Functions use jq for safe JSON construction and foci-call
// for socket communication.
func (b *ExecBridge) writeShellFuncs() error {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Auto-generated by foci exec bridge — do not edit\n\n")
	sb.WriteString(jsonPassthroughHelper)

	for _, t := range b.registry.All() {
		if !t.ExecExport {
			continue
		}
		fn := generateShellFunc(t)
		if fn != "" {
			sb.WriteString(fn)
			sb.WriteString(fmt.Sprintf("export -f foci_%s\n\n", t.Name))
		}
	}

	return os.WriteFile(b.funcsPath, []byte(sb.String()), 0600)
}

// toolParamKeys extracts the property names from a tool's JSON schema Parameters.
// Returns a space-separated string suitable for the _foci_json bash helper.
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

// generateShellFunc returns a bash function definition for a tool.
// Each tool gets a function named foci_<toolname> with appropriate argument handling.
// Every function starts with a JSON passthrough guard: if the sole argument is
// a valid JSON object whose keys are all valid parameter names for this tool,
// it is sent directly to foci-call as tool params.
func generateShellFunc(t *Tool) string {
	name := "foci_" + t.Name
	validKeys := toolParamKeys(t)
	guard := fmt.Sprintf("  _foci_json %q %q \"$@\" && return $?", t.Name, validKeys)

	switch t.Name {
	case "web_search", "memory_search":
		// Simple query tools: all args become the query string
		return fmt.Sprintf(`%s() {
%s
  local query=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --query) query="$2"; shift 2 ;;
      *) query="$query $1"; shift ;;
    esac
  done
  query="${query# }"
  if [ -z "$query" ]; then
    echo "usage: %s <query>" >&2
    return 1
  fi
  foci-call "$(jq -nc --arg q "$query" '{"tool":"%s","params":{"query":$q}}')"
}
`, name, guard, name, t.Name)

	case "web_fetch":
		// URL as first arg, optional --raw flag
		return fmt.Sprintf(`%s() {
%s
  local url="" raw=false
  while [ $# -gt 0 ]; do
    case "$1" in
      --raw) raw=true; shift ;;
      *) url="$1"; shift ;;
    esac
  done
  if [ -z "$url" ]; then
    echo "usage: %s <url> [--raw]" >&2
    return 1
  fi
  foci-call "$(jq -nc --arg u "$url" --argjson r "$raw" '{"tool":"web_fetch","params":{"url":$u,"raw":$r}}')"
}
`, name, guard, name)

	case "http_request":
		// URL as first arg, flags for method, headers, body, save_to
		// --include-headers: output full HTTP status/headers + body (default: body only)
		return fmt.Sprintf(`%s() {
%s
  local url="" method="GET" body="" save_to="" headers="{}" inc_headers=false
  while [ $# -gt 0 ]; do
    case "$1" in
      --method) method="$2"; shift 2 ;;
      --body) body="$2"; shift 2 ;;
      --header) headers="$(echo "$headers" | jq --arg k "${2%%%%:*}" --arg v "${2#*: }" '. + {($k): $v}')"; shift 2 ;;
      --save-to) save_to="$2"; shift 2 ;;
      --include-headers) inc_headers=true; shift ;;
      *) url="$1"; shift ;;
    esac
  done
  if [ -z "$url" ]; then
    echo "usage: %s <url> [--method METHOD] [--header 'K: V'] [--body BODY] [--save-to PATH] [--include-headers]" >&2
    return 1
  fi
  local params
  params="$(jq -nc --arg u "$url" --arg m "$method" --argjson h "$headers" '{"url":$u,"method":$m,"headers":$h}')"
  if [ -n "$body" ]; then
    params="$(echo "$params" | jq --arg b "$body" '. + {body: $b}')"
  fi
  if [ -n "$save_to" ]; then
    params="$(echo "$params" | jq --arg s "$save_to" '. + {save_to: $s}')"
  fi
  foci-call "$(jq -nc --argjson p "$params" --argjson ih "$inc_headers" '{"tool":"http_request","params":$p,"include_headers":$ih}')"
}
`, name, guard, name)

	case "send_telegram":
		// Text as args or stdin; optional --file flag
		return fmt.Sprintf(`%s() {
%s
  local text="" file_path="" send_as=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --file) file_path="$2"; shift 2 ;;
      --send-as) send_as="$2"; shift 2 ;;
      *) text="$text $1"; shift ;;
    esac
  done
  text="${text# }"
  if [ -z "$text" ] && [ -z "$file_path" ]; then
    if [ ! -t 0 ]; then
      text="$(cat)"
    fi
  fi
  if [ -z "$text" ] && [ -z "$file_path" ]; then
    echo "usage: %s <text> [--file PATH] [--send-as TYPE]" >&2
    echo "  or: echo 'message' | %s" >&2
    return 1
  fi
  local params="{}"
  if [ -n "$text" ]; then
    params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
  fi
  if [ -n "$file_path" ]; then
    params="$(echo "$params" | jq --arg f "$file_path" '. + {file_path: $f}')"
  fi
  if [ -n "$send_as" ]; then
    params="$(echo "$params" | jq --arg s "$send_as" '. + {send_as: $s}')"
  fi
  foci-call "$(jq -nc --argjson p "$params" '{"tool":"send_telegram","params":$p}')"
}
`, name, guard, name, name)

	case "todo":
		// action as first arg, rest varies by action
		return fmt.Sprintf(`%s() {
%s
  local action="$1"; shift 2>/dev/null || true
  local text="" priority="" tag="" query="" status="" id="" reason=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --text) text="$2"; shift 2 ;;
      --priority) priority="$2"; shift 2 ;;
      --tag) tag="$2"; shift 2 ;;
      --query) query="$2"; shift 2 ;;
      --status) status="$2"; shift 2 ;;
      --id) id="$2"; shift 2 ;;
      --reason) reason="$2"; shift 2 ;;
      *) # positional: first positional is text/query/id depending on action
        case "$action" in
          add|edit) text="$text $1" ;;
          search) query="$query $1" ;;
          get|complete|remove) id="$1" ;;
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
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    search)
      local params='{"action":"search"}'
      [ -n "$query" ] && params="$(echo "$params" | jq --arg q "$query" '. + {query: $q}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    get)
      foci-call "$(jq -nc --argjson id "$id" '{"tool":"todo","params":{"action":"get","id":$id}}')"
      ;;
    complete)
      local params='{"action":"complete"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$reason" ] && params="$(echo "$params" | jq --arg r "$reason" '. + {reason: $r}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    edit)
      local params='{"action":"edit"}'
      [ -n "$id" ] && params="$(echo "$params" | jq --argjson i "$id" '. + {id: $i}')"
      [ -n "$text" ] && params="$(echo "$params" | jq --arg t "$text" '. + {text: $t}')"
      [ -n "$priority" ] && params="$(echo "$params" | jq --arg p "$priority" '. + {priority: $p}')"
      [ -n "$tag" ] && params="$(echo "$params" | jq --arg g "$tag" '. + {tag: $g}')"
      foci-call "$(jq -nc --argjson p "$params" '{"tool":"todo","params":$p}')"
      ;;
    remove)
      foci-call "$(jq -nc --argjson id "$id" '{"tool":"todo","params":{"action":"remove","id":$id}}')"
      ;;
    *)
      echo "usage: %s <add|list|search|get|complete|edit|remove> [args...]" >&2
      return 1
      ;;
  esac
}
`, name, guard, name)

	case "summary":
		// Prompt as argument; content from --file or stdin
		return fmt.Sprintf(`%s() {
%s
  local prompt="" file=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --file) file="$2"; shift 2 ;;
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
    file="$(mktemp /tmp/foci-summary-XXXXXX)"
    cat > "$file"
    trap "rm -f '$file'" EXIT
  fi
  if [ -z "$file" ]; then
    echo "error: no input — provide --file or pipe stdin" >&2
    return 1
  fi
  foci-call "$(jq -nc --arg f "$file" --arg p "$prompt" '{"tool":"summary","params":{"file":$f,"prompt":$p}}')"
}
`, name, guard, name, name)

	case "spawn":
		// prompt as args, optional --model and --context flags
		return fmt.Sprintf(`%s() {
%s
  local prompt="" model="" ctx_mode=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --model) model="$2"; shift 2 ;;
      --context) ctx_mode="$2"; shift 2 ;;
      *) prompt="$prompt $1"; shift ;;
    esac
  done
  prompt="${prompt# }"
  if [ -z "$prompt" ]; then
    echo "usage: %s <prompt> [--model MODEL] [--context none|character_only|clone_current]" >&2
    return 1
  fi
  local params
  params="$(jq -nc --arg p "$prompt" '{"prompt":$p}')"
  if [ -n "$model" ]; then
    params="$(echo "$params" | jq --arg m "$model" '. + {model: $m}')"
  fi
  if [ -n "$ctx_mode" ]; then
    params="$(echo "$params" | jq --arg c "$ctx_mode" '. + {context: $c}')"
  fi
  foci-call "$(jq -nc --argjson p "$params" '{"tool":"spawn","params":$p}')"
}
`, name, guard, name)

	case "tmux":
		// Subcommand-style dispatch (same pattern as todo)
		return fmt.Sprintf(`%s() {
%s
  local op="$1"; shift 2>/dev/null || true
  local name="" command="" workdir="" watch="" keys="" enter="" lines="" window="" threshold="" raw=""
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
      --threshold) threshold="$2"; shift 2 ;;
      --raw) raw=true; shift ;;
      *) shift ;;
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
      [ -n "$threshold" ] && params="$(echo "$params" | jq --argjson t "$threshold" '. + {threshold_seconds: $t}')"
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
`, name, guard, name)

	default:
		// Generic: JSON passthrough handles the common case;
		// fall back to treating $1 as raw JSON params
		return fmt.Sprintf(`%s() {
%s
  foci-call "$(jq -nc --argjson p "$1" '{"tool":"%s","params":$p}')"
}
`, name, guard, t.Name)
	}
}
