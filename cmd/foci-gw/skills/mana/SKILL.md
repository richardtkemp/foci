---
name: mana
description: Analyze mana (Anthropic rate limit quota) consumption patterns. Use when investigating "where did the mana go", diagnosing cache busts, estimating cost of planned work, or understanding API spend efficiency.
---

# Mana Analysis

Mana = Anthropic's rate limit quota. Resets to 100% every 5 hours. It represents remaining API quota before rate limiting.

## Key Concepts

Anthropic plans have a **usage limit per 5-hour window**. The `cost_usd` values logged per API call are calculated from Anthropic's per-token list pricing — what the same usage *would* cost on the pay-per-use API. They're useful for **relative** comparisons between turns but aren't actual spend on subscription plans.

**Mana is the real constraint.** It's the rate limit that resets every 5 hours. Each percentage point of mana corresponds to a fraction of your plan's 5-hour budget.

**Mana is shared** across ALL consumers on the same Anthropic account: foci API, Claude Code, mobile app, desktop app. If mana drops faster than expected, check whether other apps were also consuming.

## Quick Diagnostics

### "Where did the mana go?"

```bash
# Total cost in last N hours
sqlite3 ~/data/api.db "SELECT SUM(cost_usd), COUNT(*) FROM api_calls WHERE ts > datetime('now', '-3 hours')"

# Biggest individual calls
sqlite3 ~/data/api.db "SELECT ts, call_type, cost_usd, cache_read, cache_write FROM api_calls WHERE ts > datetime('now', '-3 hours') ORDER BY cost_usd DESC LIMIT 10"

# Cache busts (cache_read = 0 with large cache_write)
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_write FROM api_calls WHERE cache_read = 0 AND cache_write > 10000 ORDER BY ts DESC LIMIT 10"
```

### "Why did the cache bust?"

Cache busts show as `cache_read=0` with large `cache_write`. Common causes:
- **Session switch:** cron heartbeat ran between your turns (different session = different prefix)
- **Model switch:** changing models invalidates the entire cache
- **Restart/deploy:** new process = new cache
- **Compaction:** context was compressed, changing the prefix

### "Can I afford this?"

Rough estimates (Opus, relative to mana):
- **Simple Q&A turn:** ~$0.07-0.10 (cache hit) → <0.1% mana
- **Multi-tool investigation (5 turns):** ~$0.50-1.00 → ~0.5% mana
- **Long coding session (30 min):** ~$5-15 → ~4-10% mana
- **Cache bust + rebuild:** ~$2.50 one-time → ~2% mana

## How Mana Works

1. Anthropic tracks API usage in a **5-hour window that resets to 100%**
2. Utilization = (usage in window) / (plan limit) x 100
3. Mana = 100% - utilization
4. Old usage **rolls off** the trailing edge — mana recovers over time
5. The window slides continuously, not in discrete steps

### Why the ratio varies

At low utilization, the $/mana% ratio appears inflated. This suggests Anthropic's rate limiter isn't purely dollar-cost-based — it may factor in token counts, request rates, or have a non-linear curve. Above ~15% utilization, the ratio stabilises.

### Mana data sources

- **Mana readings:** Embedded in `[meta]` headers in session JSONL files. Cached for 5 minutes by foci (fetched from Anthropic's OAuth usage API).
- **API costs:** Logged per-call in `~/data/api.db` with `cost_usd`, token counts, session ID, timestamps.

## Common Mana Drains

1. **Cron heartbeats** — each invocation runs multiple tool calls on Opus. Can cost several percent mana per hour even with no user activity.
2. **Cache busts** — session interleaving (cron fires between user turns), deploys, model switches.
3. **Separate sessions** — each session maintains its own cache prefix. A session that hasn't been active recently pays a full cache write on its first turn.
