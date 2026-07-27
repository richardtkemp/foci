---
name: research
description: "Web research with Perplexity via OpenRouter. Use when you need to search the web and synthesize current information. Supports two modes: (1) Basic Sonar for general research and fact-checking, (2) Sonar Deep Research for complex analysis, multi-step queries, or when you're uncertain if deep research might be needed (ask for confirmation). Uses OpenRouter's API - no separate Perplexity key required."
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/research/`), not the deployed `~/shared/skills/` copy.

# OpenRouter Perplexity Research Skill

This skill enables web research using Perplexity models through OpenRouter, with two research depths available.

## Models

- **perplexity/sonar** — Fast, general-purpose web research. Good for fact-checking, recent news, quick lookups.
- **perplexity/sonar-deep-research** — Deeper analysis with multi-step reasoning. Use for complex questions, comparative research, or investigation-style queries.

## When to Suggest Deep Research

Before choosing a model, consider the query:

- **Use basic Sonar** for: "What's X's current stock price?", "Who won Y award?", "Latest news on Z"
- **Suggest Deep Research** for: Multi-part questions, "Compare X vs Y", "How would Z affect...", "What are implications of...", investigative queries

**If unsure**, suggest deep research and ask for confirmation. The user can always say "no, just use basic Sonar" and save tokens.

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
- **Sonar Deep Research**: Same pricing, more reasoning

See references/pricing.md for full details.

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
