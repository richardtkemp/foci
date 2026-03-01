# TODO #167: Isolated Environment for "none" Context Spawns

## Goal
"None" mode spawns should run in an isolated temporary directory, not the agent's workspace. Any files they create should be listed in the spawn result so the caller knows what was produced.

## Why
"None" spawns are one-shot queries to different models — they have no character context and shouldn't be trusted with the agent's workspace. Currently they have full read/write/exec access to all the same paths as the parent.

## Current Architecture
- `tools/spawn.go:127` — `case "none"` calls `spawnOneShot()` with tool access from `spawnToolSet()`
- Tools available include `read`, `write`, `edit`, `exec` — all operate relative to the agent's workspace
- `spawnOneShot()` at line 179 runs API calls in a loop with tool dispatch
- Tool execution happens via the same `Tool.Run` functions as the parent agent

## Design

### Approach: Temporary workspace with file tracking
1. **Create temp dir** for each none-mode spawn: `os.MkdirTemp("", "foci-spawn-*")`
2. **Override working directory** for tools in the spawn's tool set:
   - `exec` tool: set `Dir` on the command to the temp dir
   - `read`/`write`/`edit` tools: resolve relative paths against temp dir, block access outside it
3. **Track created files**: after spawn completes, `os.ReadDir` the temp dir and list files
4. **Append file list** to the spawn result: "Files created: [list with sizes]"
5. **Don't auto-delete**: leave temp dir for the caller to access. Clean up via a periodic sweep of old spawn dirs (or let OS handle via `/tmp` cleanup).

### Alternative considered: chroot/namespace
Too heavy — requires root, adds complexity. A temp dir with path restriction on the tool set is sufficient.

### Implementation

#### Option A: Tool-level isolation (simpler)
Pass an `overrideWorkdir` to the spawn tool set builder. Each tool checks for this override:
- `exec`: already has the ability to set working dir
- `read`/`write`/`edit`: need a `baseDir` parameter that restricts path resolution

This requires adding a `BaseDir` field to the file tools and checking it in `Run()`.

#### Option B: Process-level isolation (cleaner)
Run the spawn's tool calls via a subprocess (like `foci-call`) with `Chdir` set. But this changes the architecture significantly.

**Recommend Option A.**

### Files to change
- **tools/spawn.go**: Create temp dir for none mode, pass to tool set builder, append file list to result
- **tools/files.go**: Add optional `BaseDir` field to read/write/edit tools; resolve and restrict paths
- **tools/exec.go**: Already supports working dir via command.Dir — just need to wire it
- **tools/spawn_test.go**: Test isolation (write in none mode goes to temp dir, can't escape)

### Edge cases
- Absolute paths: block in none mode (or resolve against temp dir)
- `../` traversal: resolve with `filepath.Clean` and verify prefix
- Symlink escape: use `filepath.EvalSymlinks` before checking prefix
- Large file creation: temp dir inherits `/tmp` quota limits

### What the caller sees
```
[spawn result]
The analysis shows...

---
Files created in /tmp/foci-spawn-abc123/:
  report.md (2.4 KB)
  data.json (890 B)
```
