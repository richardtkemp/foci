<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Prompt-cache bust diagnosis

The cached prefix is the system prompt (environment block + character files) + history, per session. Foci caches character files in memory: edits take effect at the next compaction or restart, not mid-session.

A "bust" is a call that reads less of the cache than its request should have: less than the previous call's cache read + write, inside the TTL. The ledger's cache watch (`accounting/cachewatch.go`) checks every booked call on every backend. It logs each bust at WARN under `ledger/<agent>` (`cache bust: session … read X of an expected Y …; cause …`) and posts `⚠️ Cache bust: read X of Y cached tokens …` to chat (`[notify] cache_bust_notify`). Its cause is inferred from the counts (`model_switch`, `prompt_changed`, `fork_miss`, `prefix_changed`); a bust older than the TTL is an `expiry` at INFO. For a Claude Code call the report also quotes the evidence below (#3221).

```bash
# Find the bust boundary (every backend)
sqlite3 -readonly -safe ~/data/api.db "SELECT c.billed_at, c.session, t.class, t.count FROM api_calls c JOIN call_tokens t ON t.call_id = c.id WHERE t.class LIKE 'cache%' ORDER BY c.billed_at DESC LIMIT 40"
# A CC call's cache evidence, as booked (#3221)
sqlite3 -readonly -safe ~/data/api.db "SELECT billed_at, session, call_key, detail FROM api_calls WHERE detail LIKE '%cache_miss_reason%' OR detail LIKE '%request_change%' ORDER BY billed_at DESC LIMIT 20"
```

Common causes: a character/system-prompt edit picked up by `bootstrap.Reload()`, a model switch, a relaunch with a different environment block, compaction on another session that shares the prefix.

## Direct API (`backend = "api"`)

Only this transport writes the payload log (`api-payload.jsonl`). Diff the two calls' `.request.system[]` (jq in `api-cost.md`).

## Claude Code: the transcript records

CC has no payload log, but its transcript (path: `sessions.md`) carries the request's structure. Verified on CC 2.1.325:

| Record | What it holds |
|---|---|
| assistant line, `.message.diagnostics.cache_miss_reason` | Anthropic's cache diagnostics, which CC requests on every call. `{type, cache_missed_input_tokens}`. Types: `model_changed`, `system_changed`, `tools_changed`, `messages_changed` name the first divergence from the previous request; `previous_message_not_found` and `unavailable` mean no comparison was made (not evidence of a change). `null` diagnostics = no divergence, or not asked. Docs: https://platform.claude.com/docs/en/build-with-claude/cache-diagnostics |
| `api-request` | One per request CC sent: `id`, `base` (the request it extends), `keep` (how many of base's messages it kept unchanged), `tail` (digests of the messages it appended), `shapeHash` (system prompt + tools), `params` (model, thinking, betas, effort …), `cacheControl` (where the breakpoints are). The assistant line's `requestRef` names it. |
| `api-request-shape` | `shapeHash` → the full `system` and `tools`. Written when the shape is new. Diff two shapes to see a system-prompt or tool change. |
| `api-request-blob` | `hash` → a message body that is not elsewhere in the transcript (system reminders, tool additions). |

Reading a bust from them:

- **Diagnosis first.** A `*_changed` reason on the bust's assistant line is the answer. Seen in practice: `system_changed` on the first call after a relaunch, with the read stopping at ~10.4k (the stable part of the system prompt).
- **Then the digests.** Find the bust's `api-request` (its `requestRef`) and its `base`. The request changed nothing before its new messages when `keep` = base's `keep + len(tail)` and `shapeHash` and `params` match. A smaller `keep` means an earlier message changed. A different `shapeHash` means the system prompt or tools changed: diff the two `api-request-shape` records. A shape change alone does not bust: a tool added mid-conversation reads in full.
- **No `base`** (the first request of a CC process) means the digests cannot say; rely on the diagnosis.
- **Digest unchanged, diagnosis `unavailable` or absent, read still short**: the request matched, yet the cache did not hold the prefix. Not a foci or CC change; record the case.

```bash
T=<transcript.jsonl>
# The bust's line: diagnosis, counts, request record id
jq -c 'select(.type=="assistant" and .message.diagnostics != null) | {timestamp, requestRef, d: .message.diagnostics, r: .message.usage.cache_read_input_tokens, w: .message.usage.cache_creation_input_tokens}' "$T"
# The request and its base
jq -c --arg id <requestRef> 'select(.type=="api-request" and .id==$id) | {id, base, keep, tail: (.tail|length), shapeHash, model: .params.model}' "$T"
```

The system prompt foci launched CC with is also in tracing: with `[tracing]` on, the first turn after a launch or prompt change exports a `system_prompt` event (full text + hash) to Langfuse (skill `langfuse-v4-api`); later turns carry only the hash. Grep `foci.log` for `reason=prompt-changed` (DEBUG) and restarts.

## opencode, codex

No payload log and no diagnostics: the cache watch's inferred cause and Langfuse's `system_prompt` events are what there is.

For a guided, step-by-step analysis use the **`cache-diagnosis`** skill.
