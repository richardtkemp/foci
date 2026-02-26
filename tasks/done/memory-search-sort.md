# Task: Add sort order to memory_search tool

## Context
The memory_search tool currently sorts results by FTS5 relevance rank only. Add a `sort` parameter so results can also be sorted by recency.

## Requirements

1. Add optional `sort` parameter to the memory_search tool schema
   - Values: `"relevance"` (default, current behaviour) and `"recency"` 
   - If not provided, default to relevance
2. When sort=recency, order results by file modification time (newest first), then by position within file
3. Memory files are already ranked higher than conversation history — this weighting should still apply regardless of sort order
4. Update tool description to mention the sort parameter
5. Write tests for both sort orders
6. Update SPEC.md and docs/WIRING.md
7. Commit and push when done
