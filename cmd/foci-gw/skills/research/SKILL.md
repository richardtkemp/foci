---
name: research
description: "Web research with Perplexity via OpenRouter. Use when you need to search the web and synthesize current information. Supports two modes: (1) Basic Sonar for general research and fact-checking, (2) Sonar Deep Research for complex analysis, multi-step queries, or when you're uncertain if deep research might be needed (ask for confirmation). Uses OpenRouter's API - no separate Perplexity key required."
---

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

The skill uses OpenRouter's API. Your OpenRouter API key must be configured in secrets.toml.

## Making Requests

Call OpenRouter's completion endpoint with the Perplexity model ID:

```
POST https://openrouter.ai/api/v1/chat/completions

Headers:
  Authorization: Bearer {{secret:openrouter.api_key}}

Body:
{
  "model": "perplexity/sonar",  // or "perplexity/sonar-deep-research"
  "messages": [
    {
      "role": "user",
      "content": "Your research query here"
    }
  ]
}
```

**Response structure:**
- `choices[0].message.content` — Research result with citations
- `usage.prompt_tokens`, `usage.completion_tokens` — Token counts for tracking

## Workflow

1. **Parse the request** — Is this a research task?
2. **Pick the model** — Basic Sonar for simple queries, suggest Deep Research for complex ones
3. **Make the API call** — Use `http_request` to hit the OpenRouter endpoint
4. **Return results** — Include citations and summary in your response

## Cost Reference

- **Sonar**: $2/M input tokens, $8/M output tokens
- **Sonar Deep Research**: Same pricing, more reasoning

See references/pricing.md for full details.

## Example: Basic Research

Query: "What's the current state of AI safety regulations in the EU?"

```
http_request(
  method: "POST",
  url: "https://openrouter.ai/api/v1/chat/completions",
  headers: {
    "Authorization": "Bearer {{secret:openrouter.api_key}}",
    "Content-Type": "application/json"
  },
  body: '{"model":"perplexity/sonar","messages":[{"role":"user","content":"What is the current state of AI safety regulations in the EU?"}]}'
)
```

## Example: Complex Research (suggest deep research first)

Query: "Compare the business models and recent financial performance of OpenAI, Anthropic, and Mistral"

Suggestion to user: "This looks like a complex comparative analysis. Want me to use Deep Research for a more thorough investigation, or just quick research with basic Sonar?"

If confirmed, use `perplexity/sonar-deep-research`.
