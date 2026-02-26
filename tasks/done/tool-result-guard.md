# Task: Tool result guard (rename from tool truncation)

## Problem
Currently when a tool result exceeds the limit, the result is truncated to the limit and the full output is saved to a temp file. This wastes context on partial data and the agent loses the tail. 

## New behaviour
When a tool result exceeds the limit, return **only** a message telling the agent where the full output was saved and how to extract what it needs. No partial content at all.

### Message format
```
Result too large ({actual_size} chars, limit {limit}). Full output saved to {temp_file_path}.
Use {tool_hint} to extract what you need.
```

### Tool hints (based on file content/extension)
- If the result looks like JSON or the tool was reading a `.json`/`.jsonl` file → `jq`
- If the result looks like markdown or the tool was reading a `.md` file → `mdq` 
- Otherwise → `grep`, `sed`, `head`, `tail`, or `ack`

The hint should be contextual, e.g.:
- "Use `jq` to query specific fields, or `head`/`tail` to inspect sections."
- "Use `mdq` to query specific sections, or `grep`/`sed` to extract what you need."
- "Use `head -n 50 {path}` to preview, or `grep`/`ack` to search for specific content."

### Config
- Rename config field from whatever it currently is to `tool_result_guard` or similar
- Default limit: **5000 characters** (down from current 10000)
- Keep configurable: `[tools] max_tool_result_chars = 5000`

### Naming
- Rename all references from "truncation" to "guard" throughout the codebase
- Log level should be DEBUG, not WARN — this is expected behaviour, not a warning
- Log message should say "tool result guard" not "tool result truncated"
- The temp file path pattern can stay the same

## What NOT to change
- The temp file saving mechanism — keep that as-is
- The exec timeout/background behaviour — separate concern

## Update docs
- SPEC.md — update tool result guard section
- docs/CONFIG.md — rename/update config field
