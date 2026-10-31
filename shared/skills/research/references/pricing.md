<!-- GOLDEN: ships with foci (shared/skills/research/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# OpenRouter Perplexity cost

Prices change. For current numbers, read the `pricing` field of each model in `https://openrouter.ai/api/v1/models` (USD per token).

Snapshot from that API, 2026-10-31:

| Model | Input | Output | Per web search |
|-------|-------|--------|------------|
| `perplexity/sonar` | $1/M tokens | $1/M tokens | $0.005 |
| `perplexity/sonar-deep-research` | $2/M tokens | $8/M tokens | $0.005 |

Deep research also runs many searches and long reasoning per call, so one call costs far more than a basic one. Latency and timeout rules are in `SKILL.md`.
