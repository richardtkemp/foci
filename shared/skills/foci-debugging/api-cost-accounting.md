<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Cost accounting — the traps before you state a number

*Split out of `api-cost.md`, 2026-09-22. That file is where the data lives; this one is what
goes wrong when you turn it into a figure. Read this when you are about to report a cost.*

**`cost_usd` is CUMULATIVE over the CC process — never `SUM` it; sum `calculated_cost_usd`.**
Two consequences. (1) Summing inflates ~quadratically in turns-per-process (to ~13x), yet is 1.0x on
quiet days — which is what lets it survive review. (2) It does not reconcile against the token
columns, which hold one round's snapshot: cost / a token column is a ROUND COUNT dressed as a rate —
3 rounds at Opus-5's $0.50/M cache-read reads as "$1.50/M", indistinguishable from a stale-pricing
fallback. Check per-round usage in the CC transcript before calling a rate wrong.
`calculated_cost_usd` is foci's own priced figure and is authoritative (WIRING.md "Cost columns");
rows before #1674 have it NULL.

**Column scopes differ (#1806):** `input/cache_read/cache_write_tokens` = the turn's FINAL-cycle context fill (a snapshot; pricing them recovers ~20% of cost). `output_tokens` + `turn_input/turn_cache_read/turn_cache_write_tokens` = per-TURN sums, what `calculated_cost_usd` priced. One row = one turn; no cycle ordinal.

**Counting mispriced turns: query the table, not the log.** The `cost divergence` WARN is a sampler —
four gates, including **one warning per model per 10 min** (plus a 3% tolerance and a $0.01 floor), so
log lines undercount by an unknown factor. Get the backend's per-turn figure by differencing the
cumulative column per session (`LAG(cost_usd) OVER (PARTITION BY session ORDER BY id)`). **A reset is
NOT always a drop:** a restarted CC process can open ABOVE the old one's last figure, so nothing falls
and the difference silently spans the boundary — one such row inverted a 27-turn mean. Treat any row
adjacent to a foci restart as unmeasured. **Control every run:** single-turn branch sessions
(`session LIKE '%/b%'`, no predecessor) must equal `calculated_cost_usd` exactly.

## A `cost divergence` WARN: decompose it, don't theorise

The WARN carries every field needed to attribute the gap. Unpriced-TTL residue is the usual culprit — Unknown prices at the 1h rate:

    Unknown = cache_write - ttl_1h - ttl_5m         # all three are in the WARN
    gap =~ Unknown x (rate_1h - rate_5m)            # opus-5: 10.00 - 6.25 $/M

Match to a few microdollars and the cause is settled. Cross-check against the subagent's own transcript, which is the authority:

    ~/.claude/projects/<slug>/<parent-session-uuid>/subagents/agent-<id>.jsonl
    jq -r 'select(.message.stop_reason != null)
           | .message.usage.cache_creation_input_tokens' FILE

Its completed-message cache-write total equals Unknown exactly. Two matching numbers from unrelated artifacts is a diagnosis; one is a coincidence.

Zero `call_type='subagent_turn'` rows means the correction path was never REACHED — missing `stranded=`/`cost correction` lines then say nothing about whether that code works.

## An unexpected model in the cost table

**A model nobody configured means CC switched it, not you.** CC can change model mid-session (Opus safeguards refusing a turn -> a `model_refusal_fallback` record) and foci logs nothing — the only trace is the CC transcript's `type=="system"` line. Searching foci.log first finds nothing and invites a wrong theory. The switch is sticky (later branches relaunch with the new id) but per-session, so other agents resolving normally does NOT rule it out.
