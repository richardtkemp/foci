# Providers — Using Non-Anthropic Models

Foci is built for Anthropic but works with any provider that speaks the OpenAI or Gemini wire format. Three independent concepts drive model routing:

| Concept | Example | Determines |
|---------|---------|------------|
| **Developer** | `anthropic`, `google`, `deepseek` | Who made the model |
| **Endpoint** | `anthropic`, `openrouter`, `gemini` | Where to send the request (URL, API key) |
| **Wire format** | `anthropic`, `openai`, `gemini` | How to serialize the request |

These are independent — you can send an Anthropic model through OpenRouter, or a DeepSeek model through a local endpoint.

---

## Model Syntax

Models use `developer/model_id` format:

```toml
model = "anthropic/claude-sonnet-4-6"
model = "google/gemini-2.5-flash"
model = "deepseek/deepseek-chat"
```

### Built-in Aliases

Shorthand names are resolved automatically. These are the defaults (overridable via `[models.aliases]`):

| Alias | Resolves to |
|-------|-------------|
| `opus` | `anthropic/claude-opus-4-6` |
| `sonnet` | `anthropic/claude-sonnet-4-6` |
| `haiku` | `anthropic/claude-haiku-4-5-20251001` |
| `gemini-flash` | `google/gemini-2.5-flash` |
| `gemini-pro` | `google/gemini-2.5-pro` |
| `gpt4o` | `openai/gpt-4o` |
| `o3` | `openai/o3` |
| `o4mini` | `openai/o4-mini` |
| `deepseek` | `deepseek/deepseek-chat` |

Custom aliases:
```toml
[models.aliases]
opus = "anthropic/claude-opus-5-0"
local = "local/my-fine-tuned-model"
```

---

## Built-in Endpoints

Four endpoints are pre-configured. Built-in defaults are only created for endpoints that agents actually reference — unused endpoints don't trigger missing-secret warnings.

| Endpoint | Format | URL | API key secret |
|----------|--------|-----|---------------|
| `anthropic` | `anthropic` | SDK default | `anthropic.api_key` |
| `gemini` | `gemini` | SDK default | `gemini.api_key` |
| `openai` | `openai` | SDK default | `openai.api_key` |
| `openrouter` | multi-format | `https://openrouter.ai/api/v1` | `openrouter.api_key` |

OpenRouter is a multi-format endpoint — it supports both Anthropic and OpenAI wire formats via separate URLs (`anthropic_url` and `openai_url`).

---

## How Routing Works

When an agent doesn't specify an `endpoint`, foci auto-selects based on the developer name:

| Developer | Routes to | Wire format |
|-----------|-----------|-------------|
| `anthropic` | `anthropic` endpoint | `anthropic` |
| `google` / `gemini` | `gemini` endpoint | `gemini` |
| `openai` | `openai` endpoint | `openai` |
| Everything else | `openrouter` endpoint | `openai` (universal fallback) |

This means third-party models (DeepSeek, Mistral, Llama, etc.) automatically route through OpenRouter unless you configure a custom endpoint.

Wire format is inferred from the developer name. Unknown developers fall back to the OpenAI wire format, which is the de facto standard for third-party APIs.

---

## OpenRouter

[OpenRouter](https://openrouter.ai) is the default gateway for third-party models. It supports multiple wire formats, so Anthropic models sent through OpenRouter still use the Anthropic format.

No special configuration needed — just set your API key:

```toml
# secrets.toml
[openrouter]
api_key = "sk-or-..."
```

Then use any model OpenRouter supports:
```toml
model = "deepseek/deepseek-chat"       # auto-routes to openrouter
model = "meta-llama/llama-3-70b"       # auto-routes to openrouter
```

To explicitly route an Anthropic model through OpenRouter instead of direct:
```toml
[[agents]]
id = "research"
model = "anthropic/claude-sonnet-4-6"
endpoint = "openrouter"
```

---

## Custom Endpoints

Add a custom endpoint for local models, private deployments, or alternative providers:

```toml
[endpoints.local]
format = "openai"
url = "http://localhost:11434/v1"
api_key = "local.api_key"

[[agents]]
id = "local-agent"
model = "ollama/llama3"
endpoint = "local"
```

### Endpoint Config Fields

| Field | Type | Description |
|-------|------|-------------|
| `format` | string | Wire format: `"anthropic"`, `"openai"`, or `"gemini"` |
| `url` | string | Base URL (empty = SDK default) |
| `anthropic_url` | string | Anthropic-format URL (for multi-format endpoints) |
| `openai_url` | string | OpenAI-format URL (for multi-format endpoints) |
| `gemini_url` | string | Gemini-format URL (for multi-format endpoints) |
| `api_key` | string | Secret name in `secrets.toml` (e.g. `"local.api_key"`) |
| `http_timeout` | string | HTTP timeout, Go duration format (e.g. `"120s"`) |

Multi-format endpoints set format-specific URLs (`anthropic_url`, `openai_url`) instead of a single `format` + `url`. This is how OpenRouter supports both Anthropic and OpenAI wire formats.

---

## API Keys

API keys are stored in `secrets.toml`, never in `foci.toml`. The convention is `{endpoint_name}.api_key`:

```toml
# secrets.toml

[anthropic]
api_key = "sk-ant-..."
# setup_token = "sk-ant-..."   # alternative: higher priority than api_key

[gemini]
api_key = "AIza..."

[openai]
api_key = "sk-..."

[openrouter]
api_key = "sk-or-..."
```

Custom endpoints follow the same pattern — if you define `[endpoints.groq]` with `api_key = "groq.api_key"`, put the key in `secrets.toml` as:

```toml
[groq]
api_key = "gsk_..."
```

---

## Feature Availability

Not all features work with every provider. This table shows what's available:

| Feature | Anthropic | Gemini | OpenAI / other |
|---------|-----------|--------|----------------|
| Prompt caching | Full (prefix-based) | Context cache (server-side, TTL-based) | No |
| Extended thinking | Full (`adaptive` / `off`) | Partial (`adaptive` / `off`) | No |
| Streaming | Yes (requires `use_sdk = true`) | No | No |
| Server tools (web_search, web_fetch) | Yes | No | No |
| Token counting | Yes | Yes | No |
| Mana / usage tracking | Yes | No | No |
| Effort level | Yes (`low` / `medium` / `high`) | No | No |
| Speed mode | Yes (Opus only, `fast`) | No | No |
| Tool use | Yes | Yes | Yes |
| Images | Yes | Yes | Yes |

### Provider-Specific Config

Anthropic, Gemini, and OpenAI each have a dedicated config section. Settings here become defaults for agents using that provider's format:

```toml
[anthropic]
effort = "low"              # low, medium, high
thinking = "adaptive"       # adaptive, off
# speed = ""                # "fast" for Opus (beta, 6x pricing)
# streaming = false         # requires use_sdk = true
# http_timeout = "600s"

[gemini]
# thinking = "adaptive"     # adaptive, off
# cache_ttl = "1h"          # context cache TTL ("0" disables)
# http_timeout = "120s"

[openai]
# base_url = ""             # override for compatible endpoints
# http_timeout = "120s"
```

Per-agent `effort`, `thinking`, and `speed` in `[[agents]]` override these defaults. At runtime, unsupported params are silently skipped; if a model returns a 400 error about thinking/effort/speed, the params are stripped and the request is retried once.

---

## Examples

### OpenAI Direct

```toml
# foci.toml
[[agents]]
id = "gpt-agent"
model = "openai/gpt-4o"

# secrets.toml
[openai]
api_key = "sk-..."
```

### Gemini

```toml
# foci.toml
[[agents]]
id = "gemini-agent"
model = "google/gemini-2.5-flash"

# secrets.toml
[gemini]
api_key = "AIza..."
```

### DeepSeek via OpenRouter

```toml
# foci.toml
[[agents]]
id = "deep"
model = "deepseek/deepseek-chat"
# No endpoint needed — auto-routes to openrouter

# secrets.toml
[openrouter]
api_key = "sk-or-..."
```

### Local Ollama

```toml
# foci.toml
[endpoints.local]
format = "openai"
url = "http://localhost:11434/v1"
api_key = "local.api_key"

[[agents]]
id = "local"
model = "ollama/llama3"
endpoint = "local"

# secrets.toml
[local]
api_key = "ollama"   # Ollama doesn't check this, but the field is required
```

### Mixed Setup

```toml
# foci.toml
[defaults]
model = "anthropic/claude-sonnet-4-6"

[[agents]]
id = "main"
# Uses default model (Sonnet via Anthropic)

[[agents]]
id = "research"
model = "google/gemini-2.5-flash"

[[agents]]
id = "cheap"
model = "deepseek/deepseek-chat"

# secrets.toml
[anthropic]
api_key = "sk-ant-..."

[gemini]
api_key = "AIza..."

[openrouter]
api_key = "sk-or-..."
```
