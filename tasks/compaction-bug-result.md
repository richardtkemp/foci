# Compaction Bug Investigation: Orphaned `tool_use` Blocks

## Root Cause

The bug is in `compaction/compact.go`, lines 146–163 — the split between `toSummarise` and `preserved` messages.

```go
toSummarise = messages[:len(messages)-preserveN]
preserved = messages[len(messages)-preserveN:]
```

This split is purely index-based. It does not respect tool_use/tool_result pairing. The Anthropic API requires that every `tool_use` block (in an assistant message) must be immediately followed by a user message containing a `tool_result` block with a matching `tool_use_id`. These two messages are an atomic pair.

### How the bug manifests

When `preserveN > 0`, the split can land in the middle of a tool_use/tool_result pair:

**Scenario A — orphaned tool_use at end of `toSummarise`:**
```
messages[...]:
  [N-5] user:      "do something"
  [N-4] assistant:  tool_use(id=toolu_ABC)     ← ends up in toSummarise
  [N-3] user:       tool_result(id=toolu_ABC)  ← ends up in preserved
  [N-2] user:       "next question"
  [N-1] assistant:  "here's my answer"
```
If `preserveN=3`, the split is at `len-3`. The assistant message with `tool_use` goes into `toSummarise` and gets fed to the summarization API call as the second-to-last message (before the summary prompt is appended). The summarization request itself will contain an assistant message with `tool_use` blocks at `summaryMessages[N-4]` but with the matching `tool_result` removed — **the summarization API call itself will fail** with the orphaned tool_use error.

**Scenario B — orphaned tool_use deeper in session (data corruption):**
If a process crash or bug caused a tool_result to be lost from the middle of the session, the entire `toSummarise` slice sent to the summarization API would contain orphaned tool_use blocks. This fails the API call even though the split point was correct.

**Scenario C — multiple tool calls in a single turn:**
An assistant message can contain multiple `tool_use` blocks. The agent loop collects all tool results into a single user message. These two messages are an inseparable pair. Any split that separates them creates orphans.

## Code Flow

1. `Agent.HandleMessage` (`agent/agent.go:895`) calls `Compactor.Compact()` after a non-tool-use turn completes.
2. `Compact()` (`compaction/compact.go:125`) loads the full session, splits it into `toSummarise` and `preserved` at line 161.
3. `toSummarise` + a summary prompt is sent to the API as `summaryMessages` (line 167–180). If `toSummarise` contains any orphaned tool_use blocks, **this API call fails**.
4. `preserved` messages are appended verbatim after the compacted header (line 243). If `preserved` starts with a tool_result user message that was separated from its tool_use, the compacted session becomes invalid.

## Fix: Two-Pronged Approach

### 1. Bounded walk-back at the split point (`safeSplitPoint`)

After computing the initial `splitIdx`, walk it backward up to `preserveMessages` positions looking for unmatched tool_use blocks. This keeps recent tool_use/tool_result pairs intact by moving them into `preserved`.

```go
func safeSplitPoint(messages []anthropic.Message, splitIdx, maxWalkBack int) int
```

At each step, if `messages[splitIdx-1]` is an assistant message with tool_use blocks that have no matching tool_result at `messages[splitIdx]`, decrement splitIdx. The walk is bounded by `maxWalkBack` (= `preserveMessages`, default 25).

In practice, role alternation means this usually walks back just 1 position. The bound is a safety net for corrupt sessions with unusual message sequences.

### 2. Synthetic tool_result injection (`repairOrphanedToolUse`)

After the walk-back, scan `toSummarise` for any remaining orphaned tool_use blocks (ones where the following message doesn't contain matching tool_results). For each orphan, inject a synthetic `tool_result` with `is_error=true` and an explanatory message. This handles:

- Data corruption (tool_results lost from mid-session due to crashes)
- Orphans too old to be reached by the bounded walk-back

```go
func repairOrphanedToolUse(messages []anthropic.Message) []anthropic.Message
```

The repair operates on the slice sent to the summarization API — it does not modify the stored session.

## Test cases

1. Split breaks a tool_use/tool_result pair — walk-back fixes it (moves back 1).
2. Orphaned tool_use deep in toSummarise — synthetic tool_result injected.
3. Multiple consecutive tool pairs at boundary — walk-back handles them.
4. Walk-back bounded by maxWalkBack — stops even if orphans remain (synthetic injection covers the rest).
5. Assistant message with multiple tool_use blocks — entire pair stays together.
6. No orphans — functions are no-ops.
7. End-to-end Compact with tool_use messages in session.
