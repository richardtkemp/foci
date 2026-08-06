<!-- GOLDEN: ships with foci (shared/skills/research/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# OpenRouter Perplexity Cost and Latency

> **Latency, timeout guidance and the prefer-a-structured-feed rule live in `SKILL.md`** — they are
> operational rules you need before you call anything, so they belong in the file you open first.
> Kept here only as the measured backing: `sonar-deep-research` blew a 600s timeout on 2026-08-06 and
> surfaced ~25 min later as an async `Client.Timeout` while basic `sonar` answered in seconds; token
> pricing is identical between the tiers, so wall-clock is the differentiating cost.

## Per-Model Pricing

| Model | Input | Output | Context |
|-------|-------|--------|---------|
| perplexity/sonar | $2/M tokens | $8/M tokens | 128K |
| perplexity/sonar-deep-research | $2/M tokens | $8/M tokens | 128K |

## Cost Examples

- Basic query (500 input + 200 output tokens): ~$0.003
- Longer research (2000 input + 1500 output tokens): ~$0.016
- Deep research (3000 input + 3000 output tokens): ~$0.030

## Why OpenRouter?

- No separate Perplexity API account needed
- Centralized billing alongside other models
- Same pricing as direct Perplexity API
- Integrates seamlessly with existing OpenRouter setup
