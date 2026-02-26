# Cache Bust Investigation Findings

## Executive Summary

The cache bust was likely **not caused by the flushed in-flight messages**. The flush at 10:01:39 was followed by 4 successful cached API calls, proving the conversation prefix remained intact. The cache bust at 10:05:35 was caused by something that changed between 10:02:20 and 10:05:35 — most likely a change to the system prompt.

## Key Findings

### 1. The Flush Mechanism

Location: `agent/agent.go:680-693`

When `/stop` cancels an agent turn, a defer flushes `newMessages` to the session file. The slice accumulates:
- User message (line 684)
- Assistant message after each API response (line 859)
- Tool result message after tool execution (line 989)

The 3 flushed messages indicate a **structurally complete turn**: user → assistant (with tool_use) → tool_result. This is NOT an orphaned/incomplete state.

### 2. Runtime Repair Mechanism

Location: `agent/agent.go:609-619` and `agent/agent.go:1072-1101`

Unlike `RepairOrphans()` which runs only at startup, `repairInterruptedToolCalls()` runs at the **start of every turn**. If a session ends with an assistant message containing tool_use blocks without matching tool_results, it:

1. Detects the orphaned tool_use blocks
2. Creates a synthetic tool_result message: `"Tool call interrupted by service restart"`
3. Appends it to both in-memory messages AND the session file
4. Logs: `"repaired %d interrupted tool calls in %s"`

This means even if the flush DID create orphaned messages, they would be repaired on the next turn — and the repair appends to the END, preserving the cache prefix.

### 3. Why the Flush Didn't Immediately Bust Cache

The 4 successful cached calls after the flush (10:01:52, 10:01:55, 10:01:59, 10:02:20) prove:

1. The flushed messages didn't alter the conversation prefix
2. Any repair that occurred appended to the end (not inserted in the middle)
3. The system prompt remained byte-identical

Anthropic's prompt cache is **prefix-matched**. Adding messages to the end doesn't invalidate cached tokens — only changes to the prefix do.

### 4. What Causes Cache Busts

From `docs/WIRING.md` and code analysis:

**Cache requires byte-identical:**
- System prompt (workspace files + environment block + secrets block + extra blocks)
- Conversation history prefix

**Changes that bust cache:**
- Any edit to workspace files (IDENTITY.md, SOUL.md, etc.)
- `/reload` command (re-reads all workspace files)
- Compaction (replaces entire session file — explicitly resets cache baseline at line 908)
- Changes to available secrets or bitwarden status
- Any modification to `ExtraSystemBlocks`

### 5. Likely Cause of 10:05:35 Cache Bust

The 3-minute gap between the last cached call (10:02:20) and the bust (10:05:35) is well within Anthropic's 5-minute cache TTL. Something changed the **system prompt** in this window:

**Most likely causes:**
1. **Workspace file edited** — Any change to IDENTITY.md, SOUL.md, etc. would change the system prompt
2. **`/reload` command** — Forces re-read of all workspace files and skills
3. **Compaction** — Would be logged, but check if any compaction fired

**Less likely:**
- Secrets/bitwarden status change (would require config edit)
- Extra system blocks change (would require code change or skill file edit)

## Recommendations for Further Investigation

1. **Check logs for `/reload`** between 10:02 and 10:05
2. **Check logs for compaction** during that window
3. **Check workspace file timestamps** — any file modified in that 3-minute window?
4. **Check if skills were edited** — skill files are loaded as extra system blocks

## Code References

| Component | Location |
|-----------|----------|
| Flush defer | `agent/agent.go:685-693` |
| newMessages accumulation | `agent/agent.go:684, 859, 989` |
| Runtime repair | `agent/agent.go:609-619, 1072-1101` |
| Startup repair | `session/store.go:254-320`, `main.go:261` |
| Cache baseline reset | `agent/agent.go:908` |
| System prompt assembly | `workspace/bootstrap.go`, `agent/agent.go:695-720` |
| Cache breakpoint | `agent/agent.go:1001-1030` |

## Architectural Notes

The system has **two repair mechanisms**:

1. **Startup (`RepairOrphans`)** — Scans ALL session files, repairs any ending with orphaned tool_use. Runs once at process start.

2. **Runtime (`repairInterruptedToolCalls`)** — Checks loaded session, repairs if needed. Runs every turn.

This dual approach ensures:
- Orphaned sessions from crashes are fixed at startup
- Orphaned sessions from `/stop` are fixed immediately on next turn
- No need for periodic scanning

The flush mechanism is designed for SIGTERM scenarios (process killed during tool execution), ensuring no messages are lost. Combined with runtime repair, it maintains both durability and cache stability.
