# Tool Piping

Tool piping exposes foci tools as shell functions inside `exec` commands. Instead of chaining tool calls through the model (one inference pass per step), you can compose tools with unix pipes in a single exec invocation. Intermediate data never enters context.

## Architecture

```
exec subprocess                       foci process
┌─────────────────────┐               ┌───────────────┐
│ foci_http_request ──┼──connect────▶ │ goroutine/conn │
│ foci_web_fetch    ──┼──connect────▶ │ goroutine/conn │
│ foci_spawn        ──┼──connect────▶ │ goroutine/conn │
└─────────────────────┘               └───────────────┘
    /tmp/foci-exec-<pid>-<n>.sock
```

Each exec call creates a per-exec unix socket (0600 perms). The `foci-call` binary connects, sends a JSON request, and prints the result. Shell wrapper functions provide ergonomic interfaces on top of `foci-call`.

## Available Functions

### `foci_web_search <query>`
Search the web. All arguments become the query string.
```bash
foci_web_search "golang error handling best practices"
```

### `foci_web_fetch <url> [--raw]`
Fetch a URL and return content as markdown (or raw HTML with `--raw`).
```bash
foci_web_fetch https://example.com/api-docs
foci_web_fetch https://example.com/page --raw
```

### `foci_http_request <url> [--method M] [--header 'K: V'] [--body B] [--save-to P]`
Make an HTTP request with full control over method, headers, and body.
```bash
foci_http_request https://api.example.com/data
foci_http_request https://api.example.com/items --method POST --body '{"name":"test"}'
foci_http_request https://api.example.com/file --save-to /tmp/output.json
foci_http_request https://api.example.com/auth --header 'Authorization: Bearer token123'
```

### `foci_memory_search <query>`
Search memory files and conversation history.
```bash
foci_memory_search "database migration"
```

### `foci_send_telegram <text>`
Send a Telegram message. Reads from stdin when no arguments (pipe-friendly).
```bash
foci_send_telegram "Build completed successfully"
echo "Pipeline results: all green" | foci_send_telegram
```

### `foci_todo <action> [args...]`
Manage the todo list.
```bash
foci_todo add "Review PR #42"
foci_todo list
foci_todo complete 3
foci_todo search "review"
foci_todo remove 5
```

### `foci_spawn <prompt> [--model M] [--context C]`
Spawn a sub-call to a model.
```bash
foci_spawn "Summarize this data" --model haiku --context none
```

## Composition Examples

Search the web and send the top results via Telegram:
```bash
foci_web_search "latest golang release" | head -5 | foci_send_telegram
```

Fetch an API, filter with jq, and send a notification:
```bash
foci_http_request https://api.github.com/repos/golang/go/releases/latest \
  | jq -r '.tag_name + ": " + .name' \
  | foci_send_telegram
```

Search memory for context, then ask a model to summarize:
```bash
context=$(foci_memory_search "deployment checklist")
foci_spawn "Summarize this: $context" --model haiku --context none
```

Fetch a page and save processed output:
```bash
foci_web_fetch https://example.com/docs | grep -i "api" > /tmp/api-notes.txt
```

## Dependencies

- **jq** — used by shell functions for safe JSON construction (avoids injection from special characters in URLs/text)
- **foci-call** — small Go binary installed to `/usr/local/bin` by `setup.sh`

## How It Works Internally

1. When `exec` runs a command (non-background mode), it creates an `ExecBridge`
2. The bridge opens a unix socket at `/tmp/foci-exec-<pid>-<n>.sock`
3. A shell functions file is generated with `foci_<toolname>()` for each tool with `ExecExport: true`
4. The command is wrapped: `set -o pipefail; source <funcs.sh>; <original command>`
5. `FOCI_SOCK` environment variable is set so `foci-call` knows where to connect
6. Each function call connects to the socket, sends a JSON request, and returns the result
7. After the command exits, the bridge is closed and socket/funcs files are cleaned up

## Limitations

- **Background mode:** Tool piping is not available in `background: true` exec calls (daemon mode)
- **Large responses:** 1MB scanner buffer limit (tools already truncate output)
- **jq dependency:** Functions fail with "command not found" if jq is not installed
- **foci-call not in PATH:** Functions fail if the binary is not installed
