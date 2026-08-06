---
name: research
description: "Web research with Perplexity via OpenRouter. Use when you need to search the web and synthesize current information. Basic Sonar is the DEFAULT and suits almost all use cases; reserve Sonar Deep Research for when the user asks for it, the subject is genuinely complex, or you specifically want an extremely detailed response — it takes minutes and can outlive your tool timeout. Uses OpenRouter's API - no separate Perplexity key required."
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/research/`), not the deployed `~/shared/skills/` copy.

# OpenRouter Perplexity Research Skill

This skill enables web research using Perplexity models through OpenRouter, with two research depths available.

## Models

- **perplexity/sonar** — Fast, general-purpose web research. Good for fact-checking, recent news, quick lookups.
- **perplexity/sonar-deep-research** — Deeper analysis with multi-step reasoning. Use for complex questions, comparative research, or investigation-style queries.

## Latency — read this BEFORE choosing a tier

The two tiers cost the same per token and differ by **orders of magnitude in wall-clock**. That is the
cost that actually bites, and it is not visible in the depth-based descriptions above.

| Model | Typical latency | Tool timeout |
|-------|-----------------|--------------|
| `perplexity/sonar` | seconds | 300s is ample |
| `perplexity/sonar-deep-research` | **minutes — can outlive ANY timeout you set** | see below |

**Measured 2026-08-06:** one `sonar-deep-research` call blew a **600s** timeout and surfaced roughly
25 minutes later as an async
`net/http: request canceled (Client.Timeout ... while reading body)`. Basic `sonar` answered the same
question in seconds.

- **Never put a deep-research call on the critical path of work you are mid-way through.** The answer
  can arrive after you have finished, moved on, or ended the turn. Treat it as fire-and-collect-later,
  not a blocking lookup.
- **Try basic `sonar` first.** It often suffices, and when it does not its failure is *informative* —
  "I could not verify X from official sources" names the sub-questions that actually need depth, so
  the follow-up can be narrow instead of open-ended.
- **A timed-out deep-research call still bills you** for reasoning you never read.

## Before either tier: is there a structured feed?

For **table lookups** — prices, model catalogues, release dates, version numbers, changelogs — a real
API beats both tiers on every axis: exact values, no hallucination surface, no wait, and re-runnable
later to detect drift.

Worked example: for "current Anthropic model prices and when they took effect",
`https://openrouter.ai/api/v1/models` returns per-model `pricing` **and** a `created` timestamp — the
whole answer in one fast call. The same question put to Perplexity came back with partial coverage and
declined to confirm several models.

Research models earn their keep where no such feed exists: synthesis, comparison, causal "why did X
happen", anything needing judgement across sources. Reach for them there, not for lookups.

## Which tier — basic Sonar is the DEFAULT

**Use basic `sonar` unless one of these three is true:**

1. **The user explicitly asked for deep research.**
2. **The subject is genuinely complex** — multi-part, comparative, causal, investigative; the kind of
   question where sources must be weighed against each other rather than looked up.
3. **You specifically want an EXTREMELY detailed response.**

**Basic Sonar is suitable for almost all use cases.** Treat deep research as the exception you must
justify, not the safe default.

This reverses the advice that used to sit here ("if unsure, suggest deep research"). Defaulting to
deep research is not the cautious choice — it costs minutes of wall-clock (see Latency above), can
outlive your tool timeout, still bills you when it times out, and usually answers no better. If
basic Sonar turns out to be insufficient, its shortfall is *informative* and tells you exactly what
to escalate; starting deep loses that signal along with the time.

If unsure: **run basic Sonar first and read the answer.** That is cheaper and faster than deliberating,
and it is the same move as trying the cheap experiment before the expensive one.

## API Setup

The skill uses OpenRouter's API via the **`foci_http_request`** tool (aka `http_request`), which
resolves the API key **server-side** from the secret store — you never handle the raw key.

> ⚠️ **Do NOT use `curl` with an `$OPENROUTER_API_KEY` env var.** That variable is *not* exported in
> the agent shell, so curl returns `401 Missing Authentication header`. Use the secret template below.

The key lives as the `openrouter.api_key` secret; reference it as `{{secret:openrouter.api_key}}` in
the `Authorization` header. It resolves at execution time, provided `openrouter.ai` is in that
secret's `allowed_hosts` in `secrets.toml`.

## Making Requests

Call OpenRouter's completion endpoint with `foci_http_request`:

```
foci_http_request https://openrouter.ai/api/v1/chat/completions \
  --method POST \
  --header 'Authorization: Bearer {{secret:openrouter.api_key}}' \
  --header 'Content-Type: application/json' \
  --body '{"model":"perplexity/sonar","messages":[{"role":"user","content":"Your research query here"}]}' \
  | jq -r '.choices[0].message.content'
```

Swap the model to `perplexity/sonar-deep-research` for deeper analysis.

**Response structure** (pull with `jq` so only what you need hits context):
- `.choices[0].message.content` — research result with citations
- `.usage.prompt_tokens`, `.usage.completion_tokens` — token counts for tracking

## Workflow

1. **Parse the request** — Is this a research task?
2. **Pick the model** — Basic Sonar for simple queries, suggest Deep Research for complex ones
3. **Make the API call** — Use `foci_http_request` (never curl/env-var) to hit the OpenRouter endpoint
4. **Return results** — Include citations and summary in your response

## Cost Reference

- **Sonar**: $2/M input tokens, $8/M output tokens
- **Sonar Deep Research**: Same token pricing, more reasoning — but the real cost is WALL-CLOCK, not tokens

See references/pricing.md for full details, including measured latency and timeout guidance.

## Example: Basic Research

Query: "What's the current state of AI safety regulations in the EU?"

```bash
foci_http_request https://openrouter.ai/api/v1/chat/completions \
  --method POST \
  --header 'Authorization: Bearer {{secret:openrouter.api_key}}' \
  --header 'Content-Type: application/json' \
  --body '{"model":"perplexity/sonar","messages":[{"role":"user","content":"What is the current state of AI safety regulations in the EU?"}]}' \
  | jq -r '.choices[0].message.content'
```

## Example: Complex Research (suggest deep research first)

Query: "Compare the business models and recent financial performance of OpenAI, Anthropic, and Mistral"

Suggestion to user: "This looks like a complex comparative analysis. Want me to use Deep Research for a more thorough investigation, or just quick research with basic Sonar?"

If confirmed, use `perplexity/sonar-deep-research`.
