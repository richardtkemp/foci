You're compressing your own conversation so you can continue seamlessly after context resets. Your memory files persist independently — this summary is what stays in-context for continuity.

If a `<previous-summary>` is included, merge the new messages into it. Your output replaces the in-context history.

## Format: Chronological Timeline

Structure the summary as a **timeline**, not categories. Events should appear in the order they happened. This preserves the flow of the session — what led to what, how priorities shifted, what interrupted what.

Each entry should be a concise but specific record:

```
## Session Summary — YYYY-MM-DD (time range or description)

### Session Character
One paragraph: pace, mood, what kind of session this was.

### Timeline

**HH:MM** — [Topic] What happened. Specific details: file paths, commit hashes, exact values, error messages. If the user corrected something, quote them directly.

**HH:MM** — [Topic] Next event...

### Corrections
Mistakes I made this session and what I learned. Be specific — "I assumed X, the user said Y, the truth was Z."

### Open Threads
What's unfinished, deferred, or promised. Include enough context to resume without asking.
```

## What to Preserve (in order of priority)

1. **Exact technical details** — commit hashes, file paths, config values, error messages, command outputs. These are impossible to reconstruct. Quote them.
2. **What the user said** — their words, preferences, corrections, decisions. Direct quotes when they're emphatic or specific.
3. **What was actually done** — commands run, files changed, deploys made, with outcomes.
4. **Reasoning and rejected alternatives** — why this approach, what else was considered.
5. **Emotional/relational context** — frustration, discovery, significance. One sentence is enough.
6. **Background context** — things mentioned in passing that might matter later.

## What to Compress Aggressively

- **Tool call details** — reduce to outcome. "Ran errcheck → 844 errors, sent to coding agent" not the full invocation.
- **Iterative debugging** — collapse to: what was tried, what worked, what the root cause was.
- **Routine operations** — "committed and pushed 5 files" not the git add/commit/push sequence.
- **Background session reports** — one line per completed item unless the detail matters.

## Merging with Previous Summary

When a `<previous-summary>` exists:
- **Keep its timeline structure** — append new events, don't reorganise into categories.
- **Compress older entries** — the further back an event is, the more it can shrink. Recent events get full detail; events from 2+ compactions ago become one-liners.
- **Drop completed items** that are fully resolved and have no follow-up. Trust that memory files captured anything durable.
- **Preserve corrections** — mistakes compound if forgotten. Keep the "Corrections" section across merges until the lesson is clearly internalised (appears in a character file).
- **Open Threads accumulate** — never silently drop an open thread. Either mark it resolved (with how) or carry it forward.

## Anti-Patterns

- **Category buckets** — "Deploys", "Bug Fixes", "Discussions" sections destroy temporal ordering. A bug fix that interrupted a deploy matters; separated into categories, that context is lost.

- **Summary of a summary** — "Earlier work included various bug fixes and improvements" is useless. If you can't preserve the detail, keep the commit hashes and one-line descriptions so they can be looked up.

- **Hedging language** — "Appeared to work", "Seemed to fix". State what happened. If uncertain, say what's uncertain and why.

- **Over-compressing recent events** — The last 30 minutes should be nearly verbatim-detailed. The user may continue exactly where they left off.

## Handoff

End with a brief note to let the user know compaction occurred. They may not realise context was compressed.
