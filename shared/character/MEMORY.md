# MEMORY.md — What I've Learned

<!-- EDITORIAL POLICY — READ BEFORE EDITING
Aim for ≤15,000 characters. No hard truncation in foci, but every byte
is in every API call's system prompt. Bloat costs mana on every turn.
Keep it curated: has this earned its place in EVERY session?
If it can be looked up when needed, it belongs in a dated memory file.
Edit freely — no need to defer writes or worry about cache invalidation.
See COHERENCE.md: edits to character files should be long-lived.
-->

*Part of the character — the full set of documents that constitute who I am. Facts about my human are in USER.md. This file is what I've discovered through our work together.*

## Past Memories & Conversations Are Searchable — Use `foci_memory_search`
No dated memory files are loaded into context — only MEMORY.md and the other character files are. Everything else stays retrievable via `foci_memory_search "<query>"`: every past dated memory file *and* the full conversation history. Before concluding "I don't have that" or reconstructing from scratch, search.

Why search beats `grep`-ing the memory dir:
- **Reaches conversation history, not just files** — past chats live in the indexed DB, not `memory/`, so grep can't find them at all; search can.
- **Stemmed full-text** — "programming" matches "program"/"programmer"; grep is literal-only.
- **Ranked + scoped** — memory files weighted above chat; `--sort newest|oldest`, `--date-from`/`--date-to` to narrow by time.
- **Context anchors** — each hit carries a `session#rowID`; re-query it to pull the surrounding messages.

## Critical Lessons

<!-- Lessons learned the hard way. Mistakes that taught something.
     Patterns you need to remember every session. -->

## How I Work With Agents

<!-- How you interact with coding agents. What to delegate, what to keep.
     Instruction philosophy. Patterns that work and don't. -->

## Platform (Foci)

<!-- Operational knowledge about foci itself. Mana, cache, build/deploy,
     paths, conventions. Things that make you effective on this platform. -->

## Current Projects

<!-- What's active right now. Move completed projects to dated memory files. -->
