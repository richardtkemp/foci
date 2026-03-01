# Security: Add blocked path checks to read/write/edit tools

## Problem
The exec tool checks `store.IsBlockedCommand()` before running commands, blocking access to secrets.toml and /proc/self/environ. But the read, write, and edit tools in `tools/files.go` have **no path restrictions at all**. An agent can `read` secrets.toml directly.

Currently, secret values are redacted in the read output (by the tool result guard), but:
1. Commented-out lines with plain text tokens are NOT redacted
2. The file structure itself leaks information
3. Write/edit could modify secrets.toml

## Fix
Add `store.IsBlockedPath(path)` checks to all three file tool handlers in `tools/files.go`:
- `readFile()` (line ~85)
- `writeFile()` (line ~115) 
- `editFile()` (line ~132)

### Implementation
The file tools don't currently receive the secrets store. You need to:

1. Update `NewReadTool()`, `NewWriteTool()`, `NewEditTool()` to accept or have access to the `*secrets.Store`
2. In each handler, before any file I/O, call `store.IsBlockedPath(resolvedPath)` and return an error if blocked
3. Make sure the path is resolved (absolute) before checking, since `IsBlockedPath` uses substring matching

### Pattern to follow
Look at how `execCommand()` in `tools/exec.go` line ~97-99 does it:
```go
if store != nil && store.IsBlockedCommand(p.Command) {
    return "", fmt.Errorf("command references a blocked path")
}
```

For file tools, the equivalent is:
```go
if store != nil && store.IsBlockedPath(p.Path) {
    return "", fmt.Errorf("access denied: path is restricted")
}
```

### Passing the store to file tools
The file tool constructors (`NewReadTool` etc.) return `*Tool` which has a generic `Handler`. Check how exec gets its store — likely via closure or the registry. The tool registry's `Execute` method in `tools/registry.go` may need to pass the store through, or the file tools need to capture it at construction time.

## Tests
Add tests in `tools/files_test.go`:
- Read of secrets.toml path → error
- Read of path containing "secrets.toml" → error  
- Write to secrets.toml → error
- Edit of secrets.toml → error
- Read of /proc/self/environ → error
- Normal file read still works

## Docs
No doc changes needed — this is an internal security hardening.
