<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Cost accounting — the traps before you state a number

*Split out of `api-cost.md`, 2026-09-22. That file is where the data lives; this one is what
goes wrong when you turn it into a figure. Read this when you are about to report a cost.*

**Total cost from the ledger's views (`call_costs`, `daily_costs`, `turn_costs`) — never from a
stored figure.** A view prices each call from its own counts at the rates in effect when it was billed,
or, for a pre-ledger delegated row (`kind='legacy'`, `cost_basis='recorded'`), takes the figure foci
recorded then (`legacy_calculated_cost_usd`), because that row's counts are not what was billed.
`cost_usd` NULL means unpriced (a model or class with no rate) — count those, never read them as $0.
WIRING.md "Cost ledger" has the rules.

**The backend's OWN figure is a report, not a cost:** `backend_reports.cost_usd`. For CC it is
CUMULATIVE over the process — never `SUM` it. Summing inflates ~quadratically in turns-per-process (to
~13x), yet is 1.0x on quiet days, which is what lets it survive review. It does not reconcile against
a snapshot of counts either: cost / a token count is a ROUND COUNT dressed as a rate — 3 rounds at
Opus-5's $0.50/M cache-read reads as "$1.50/M". Check per-round usage in the CC transcript before
calling a rate wrong.

**Legacy calls hold TURN totals, and their turn holds the fill (#1806):** a `kind='legacy'` call's
`call_tokens` are what the pre-ledger row priced (the turn's sums — or, before #1854 and on every
codex/opencode row, the final cycle's context fill; `detail.turn_totals` says which), and the turn's
`legacy_*` columns are its final-cycle context fill. One legacy call = one turn (or one subagent share);
a direct-API call is one API call.

**Checking Claude Code's price against CC's own figure: per PROCESS, with the CLI.** CC's figure
is cumulative per CC process, and every process is its own `backend_reports` scope
(`scope_key = <session>@<launch nanos>`): the totals it restored at launch, then each result. Its own
cost is a DIFFERENCE of two of its reports, and the calls it covers carry `detail.scope`. Do not
difference by session (`LAG(cost_usd) OVER (PARTITION BY session …)`, the pre-ledger recipe, #1940):
a restarted process can open ABOVE the old one's last figure, and the difference silently spans the
boundary. Do not compare the latest report with all calls booked so far either: a running subagent or
an unsettled remainder makes that look like a gap. `foci-gw ledger-shadow -live ~/data/api.db -shadow
~/data/api.db -since <time>` does it properly — read its "CC processes" section and FLAGS (pointed at one
ledger, its per-turn and total sections only compare the ledger with itself). The live alarm is #2111 P3.

**A remainder row is CC's spend no call record holds** (`kind` overhead or compaction,
`finality='derived'`): utility calls, a subagent's real output when its transcript has no final line,
compactions. A turn-less remainder costing more than its process's largest call is flagged by the CLI:
look for a transcript the adapter never read before calling it overhead.

## An unexpected model in the cost table

**A model nobody configured means CC switched it, not you.** CC can change model mid-session (Opus safeguards refusing a turn -> a `model_refusal_fallback` record) and foci logs nothing — the only trace is the CC transcript's `type=="system"` line. Searching foci.log first finds nothing and invites a wrong theory. The switch is sticky (later branches relaunch with the new id) but per-session, so other agents resolving normally does NOT rule it out.
