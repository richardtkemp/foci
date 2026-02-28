# Compaction Bug Investigation: Orphaned `tool_use` Blocks

## Actual Root Cause (from session data)

**The autopilot detector** in `agent/agent.go:1008-1022` injects a warning as a **separate user message** immediately after the `user(tool_result)` message. This creates consecutive user messages:

```
assistant: tool_use(toolu_XXX)
user:      tool_result(toolu_XXX)     ← paired correctly
user:      "[system] You've made many consecutive tool calls..."  ← BREAKS IT
```

The Anthropic API requires that `tool_result` blocks appear in the user message **immediately** following the assistant `tool_use`. When a separate `user(text)` message is inserted after the `user(tool_result)`, the API no longer sees the tool_result as "immediately after" the tool_use. During normal agent turns this works (the API apparently tolerates it in the same request), but when the full session is replayed for compaction summarization, the API rejects it.

### Evidence

Session file: `/home/foci/data/sessions/agent/clutch/chat/5970082313.6.jsonl`

Two instances found:
1. Lines 611→612→613: `assistant(tool_use)` → `user(tool_result)` → `user(autopilot_warning)`
2. Lines 747→748→749: same pattern

Both autopilot warnings contain: `"[system] You've made many consecutive tool calls. Stop and verify..."`

### Fix needed

The autopilot warning should be folded into the `toolMsg` as an additional text content block, not injected as a separate user message. In `agent/agent.go`:

```go
// Instead of creating a separate message:
autopilotMsg := anthropic.Message{
    Role:    "user",
    Content: []anthropic.ContentBlock{{Type: "text", Text: "[system] " + prompt}},
}

// Fold the warning into the tool result message:
toolMsg.Content = append(toolMsg.Content,
    anthropic.ContentBlock{Type: "text", Text: "[system] " + prompt},
)
```

This keeps the tool_result and warning in the same user message, maintaining valid tool_use/tool_result adjacency.

---

## Secondary Bug: Split Boundary (fixed in 8c1b7f31)

When `preserveMessages > 0`, the index-based split between `toSummarise` and `preserved` could break tool_use/tool_result pairs. Fixed with:

- **`safeSplitPoint`**: walks the split backward (bounded by `preserveMessages`) to keep pairs together
- **`repairOrphanedToolUse`**: injects synthetic tool_results for any remaining orphans (data corruption)

This fix also defends against the autopilot bug — `repairOrphanedToolUse` detects the pattern where `user(tool_result)` is followed by `user(text)` instead of being a proper pair, and injects synthetic results for any unmatched tool_use IDs.
