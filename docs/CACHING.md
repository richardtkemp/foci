# Cache Architecture

Foci is designed around Anthropic's prompt cache. Every architectural decision considers cache impact.

## How Anthropic's Cache Works

Anthropic caches the **prefix** of your prompt. If the first N tokens of a request match the first N tokens of a recent request, those tokens are served from cache at ~90% discount. The cache is:

- **Prefix-based**: sessions with identical prompt prefixes share the same cache
- **Prefix-matched**: only contiguous tokens from the start count
- **Time-limited**: 60 minutes TTL, refreshed on each hit
- **Minimum size**: 1,024 tokens (shorter prefixes aren't cached)

This means: **anything that changes the beginning of your prompt busts the entire cache.**

Pricing difference (cache read vs write):

| Model | Cache Read | Cache Write | Savings |
|-------|-----------|-------------|---------|
| Haiku | $0.30/MTok | $1.25/MTok | 76% |
| Opus | $0.50/MTok | $6.25/MTok | 92% |

## What Foci Caches

The system prompt is assembled in a stable order:

```
1. Environment block (agent config, paths, platform info)
2. Character files (SOUL.md, COHERENCE.md, CRAFT.md, USER.md, MEMORY.md)
3. Secrets list (available secret names)
4. Skills list (available skill names + descriptions)
5. ─── end of cached prefix ───
6. Conversation history (messages, tool calls, results)
```

Items 1–4 form the **cached prefix**. They change rarely (character edits, config changes, skill additions). The conversation history grows at the end, which doesn't affect the prefix cache.

## Cache Breakpoints

Two `cache_control: {"type": "ephemeral"}` breakpoints per API request:

1. **System prompt** — on the last `SystemBlock` (set by `bootstrap.SystemBlocks()`). Caches the entire system prompt so it's not re-tokenized each turn.

2. **Conversation history** — on the last content block of the second-to-last message (set by `withCacheBreakpoint()` in `agent.go`). Caches system prompt + all conversation history up to the previous turn.

Breakpoints are added **only to the API request payload**, never persisted to session storage. `withCacheBreakpoint()` returns a shallow copy of the messages slice — the originals saved to session history never have `cache_control` markers.

## Cache Stability Invariant

**The conversation history sent to the Anthropic API MUST be a strict append-only extension of the previous request.** New messages must only ever appear at the end — never inserted in the middle.

Anthropic's cache is prefix-matched. If any message shifts position (because an injected message was inserted before it), all cached tokens after that point are invalidated.

**Per-session turn lock:** `HandleMessageWithAttachments` acquires a per-session mutex before doing any work. This serializes all turns on the same session — concurrent callers (Telegram messages, async notifications, scheduled wakes, HTTP `/send`) wait until the current turn completes. Each turn loads the full session history (including messages saved by the previous turn), processes, and saves — guaranteeing strict append-only ordering.

Different sessions run concurrently — the lock is per-session, not global. Branch sessions and parent sessions have different keys and do not block each other.

## Design Decisions That Preserve Cache

### Message metadata injection
Time, cost, mana, token breakdown — all injected into each user message, not the system prompt. The system prompt stays stable. Dynamic values like timestamps go on messages because they're past the cache breakpoint.

### Message transforms
Regex transforms on inbound messages happen inside user messages, not by modifying the system prompt.

### Compaction
When conversation history is compressed, only the conversation portion changes. The system prompt prefix is untouched — the cache survives compaction.

### Character file stability
Character files are loaded into memory at startup and rebuilt from disk only on compaction or restart. Editing a character file doesn't immediately bust the cache — changes take effect at the next natural reload point (like a compaction).

### Session branching (cache sharing)
Branch sessions copy the parent's system prompt + message history at a point in time. When `LoadFull()` builds a message list starting with the parent's prefix, the cache breakpoint lands on the same byte-identical prefix. The API hits cache (read pricing) instead of re-tokenizing (write pricing). The system prompt MUST be byte-identical between parent and branch for cache hit.

### Multiball forking
Forked sessions share the parent's system prompt prefix. Since the prefix is identical, the fork benefits from the existing cache immediately.

### Tool result guard
Oversized tool results are truncated *before* entering the conversation. This matters because the conversation is part of what follows the cached prefix — keeping it smaller means more of the total prompt can benefit from caching.

### Keepalive
When idle, the keepalive timer fires a lightweight branch session to keep the cache prefix warm. Does no real work — just ensures the cache TTL doesn't expire during quiet periods.

## What Busts the Cache

| Action | Impact | Cost |
|--------|--------|------|
| Model switch | Full rebuild | ~$2.50 eq |
| `/reload` | System prompt changes | ~$2.50 eq |
| Service restart | New session, new cache | ~$2.50 eq |
| Character file edit | No immediate impact; applies at next compaction/restart | Deferred |
| Adding/removing skills | System prompt changes at next compaction/restart | Deferred |

### Cache bust alerts

When a single API call writes more than a configurable threshold of cache tokens, foci sends an immediate Telegram notification. This is a plain Telegram message, not an agent turn — zero tokens spent.

```toml
[logging]
cache_bust_detect = true           # alert when cache_read drops vs previous request
cache_bust_idle_minutes = 10       # suppress alert if session idle > N minutes (cache expired naturally)
```

```
⚠️ Cache write: 43,201 tokens ($0.27) on agent:main:main
```

Default threshold: 20,000 tokens. Set to 0 to disable. Helps catch system prompt mutations, unexpected session resets, or compaction failures that silently blow up costs.

## Monitoring

Check cache efficiency:

```
/cache          — last 5 API calls with cache breakdown
/cost           — cumulative cost
```

The `prev_tokens` in message metadata shows: `in` (new input), `out` (output), `cR` (cache read — the good number), `cW` (cache write — first-time cost).

High `cR` relative to `in` means the cache is working. A sudden spike in `cW` with low `cR` means something busted the cache.

**Verify in `api.jsonl`:** `cache_read > 0` on the second message in a session means caching is working.

## Mana Implications

Cache efficiency directly affects mana (rate limit quota). Cached tokens cost ~10% of uncached tokens against the rate limit. A well-cached session uses ~5x less mana per turn than an uncached one. This is why cache preservation isn't just about cost — it's about how long the agent can exist and work before hitting rate limits.
