# Task: Investigate Compaction Bug — Orphaned tool_use Blocks

## Problem

Compaction is failing with:
```
messages.NNN: `tool_use` ids were found without `tool_result` blocks immediately after: toolu_XXXXX
```

This means the compacted message sequence has orphaned `tool_use` blocks without matching `tool_result` blocks. The Anthropic API rejects this as invalid.

## Investigation

1. Look at the compaction flow — how are messages summarized and reassembled?
2. Is the compaction summary prompt producing output that gets parsed into messages with tool_use but no tool_result?
3. Or is the message truncation/selection dropping tool_result messages while keeping the preceding tool_use?
4. Check if the session JSONL file has valid tool_use/tool_result pairs before compaction attempts
5. Look at the compaction code in `agent/` — how does it decide which messages to keep vs summarize?

## Key files
- `agent/` — compaction logic
- `prompts/compaction-summary.md` — the compaction prompt
- Session JSONL files in `~/data/sessions/`

## Output
Write findings to `/home/rich/git/foci/tasks/compaction-bug-result.md`. This is read-only — investigate and report, do not fix.
