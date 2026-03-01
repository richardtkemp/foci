# Cache Architecture

Foci is designed around Anthropic's prompt cache. Every architectural decision considers cache impact.

## How Anthropic's Cache Works

Anthropic caches the **prefix** of your prompt. If the first N tokens of a request match the first N tokens of a recent request, those tokens are served from cache at ~90% discount. The cache is:

- **Prefix-based**: sessions with identical prompt prefixes share the same cache
- **Prefix-matched**: only contiguous tokens from the start count
- **Time-limited**: ~5 minutes TTL, refreshed on each hit
- **Minimum size**: 1,024 tokens (shorter prefixes aren't cached)

This means: **anything that changes the beginning of your prompt busts the entire cache.**

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

## Design Decisions That Preserve Cache

### Message metadata injection
Time, cost, mana — all injected into each user message, not the system prompt. The system prompt stays stable.

### Prompt rules
Regex transforms on inbound messages happen inside user messages, not by modifying the system prompt.

### Compaction
When conversation history is compressed, only the conversation portion changes. The system prompt prefix is untouched — the cache survives compaction.

### Character file stability
Character files are loaded into memory at startup and rebuilt from disk only on compaction or restart. Editing a character file doesn't immediately bust the cache — changes take effect at the next natural reload point.

### Multiball forking
Forked sessions share the parent's system prompt prefix. Since the prefix is identical, the fork benefits from the existing cache immediately.

### Tool result guard
Oversized tool results are truncated *before* entering the conversation. This matters because the conversation is part of what follows the cached prefix — keeping it smaller means more of the total prompt can benefit from caching.

## What Busts the Cache

| Action | Impact | Cost |
|--------|--------|------|
| Model switch | Full rebuild | ~$2.50 eq |
| `/reload` | System prompt changes | ~$2.50 eq |
| Service restart | New session, new cache | ~$2.50 eq |
| Character file edit | No immediate impact; applies at next compaction/restart | Deferred |
| Adding/removing skills | System prompt changes at next restart | Deferred |

## Monitoring

Check cache efficiency:

```
/cache          — last 5 API calls with cache breakdown
/cost           — cumulative cost with cache savings
```

The `prev_tokens` in message metadata shows: `in` (new input), `out` (output), `cR` (cache read — the good number), `cW` (cache write — first-time cost).

High `cR` relative to `in` means the cache is working. A sudden spike in `cW` with low `cR` means something busted the cache.

## Mana Implications

Cache efficiency directly affects mana (rate limit quota). Cached tokens cost ~10% of uncached tokens against the rate limit. A well-cached session uses ~5x less mana per turn than an uncached one. This is why cache preservation isn't just about cost — it's about how long the agent can exist and work before hitting rate limits.
