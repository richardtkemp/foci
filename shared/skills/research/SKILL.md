---
name: research
description: "Web research with Perplexity via OpenRouter. Use when you need to search the web and synthesize current information. Basic Sonar is the DEFAULT and suits almost all use cases; reserve Sonar Deep Research for when the user asks for it, the subject is genuinely complex, or you specifically want an extremely detailed response — it takes minutes and can outlive your tool timeout. Uses OpenRouter's API - no separate Perplexity key required."
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/research/`), not the deployed `~/shared/skills/` copy.

# OpenRouter Perplexity Research

## Models

- **`perplexity/sonar`** — fast web research: fact-checks, recent news, quick lookups. Returns in seconds; a 300s tool timeout is ample.
- **`perplexity/sonar-deep-research`** — multi-step reasoning for complex, comparative or investigative questions. Takes **minutes and can outlive ANY timeout you set**.

## First: is there a structured feed?

For table lookups (prices, model catalogues, release dates, versions, changelogs), a real API beats both tiers: exact values, no hallucination, no wait. Example: `https://openrouter.ai/api/v1/models` gives per-model `pricing` and a `created` timestamp in one fast call; Perplexity gave partial coverage of the same question. Use research models for synthesis, comparison and "why did X happen".

## Which tier: basic Sonar is the DEFAULT

Use basic `sonar` unless one of these is true:

1. The user explicitly asked for deep research.
2. The subject is genuinely complex: multi-part, comparative, causal, investigative, where sources must be weighed against each other.
3. You specifically want an EXTREMELY detailed response.

Deep research is the exception you must justify. (This reverses the old rule "if unsure, suggest deep research".) If unsure, run basic Sonar first and read the answer. When it falls short, its failure names the sub-questions that need depth ("I could not verify X from official sources"), so the follow-up can be narrow.

Deep research rules (measured 2026-08-06: one call blew a 600s timeout and surfaced ~25 min later as an async `net/http: request canceled (Client.Timeout ... while reading body)`):

- Never put it on the critical path of work you are mid-way through. Treat it as fire-and-collect-later.
- A timed-out call still bills you for the reasoning you never read.

## Making requests

Use **`foci_http_request`** (`http_request` on the API loop). The key is the `openrouter.api_key` secret; it resolves server-side, if `openrouter.ai` is in that secret's `allowed_hosts`.

> ⚠️ **Do NOT use `curl` with `$OPENROUTER_API_KEY`.** The variable is not exported in the agent shell, so curl returns `401 Missing Authentication header`.

```
foci_http_request https://openrouter.ai/api/v1/chat/completions \
  --method POST \
  --header 'Authorization: Bearer {{secret:openrouter.api_key}}' \
  --header 'Content-Type: application/json' \
  --body '{"model":"perplexity/sonar","messages":[{"role":"user","content":"Your research query here"}]}' \
  | jq -r '.choices[0].message.content'
```

The answer, with citations, is `.choices[0].message.content`; token counts are in `.usage`. Include the citations in your reply.

Cost: see **references/pricing.md**. Token cost is small either way; for deep research the real cost is wall-clock.
