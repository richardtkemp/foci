# Model Configuration

Foci uses model groups to route different tasks to different models. This guide covers how to configure models, route tasks, and override models at runtime.

---

## Model Groups

Group assignments are configured in `[groups]` using `developer/model_id` strings. `[groups]` is free-form — arbitrary group names work, and any name you add can be referenced from `[groups.calls]` or resolved via the `--model` flag. Three built-in groups are recognised by foci's own call sites:

| Group | Purpose | Default call sites |
|-------|---------|-------------------|
| **powerful** | Primary tasks | chat, spawn-clone, background, compaction, memory-capture, memory-consolidate |
| **fast** | Quick spawns | spawn-raw, spawn-character |
| **cheap** | Bulk/utility work | spawn-explore, summarize-tool, summarize-file, prompt-diff |

`powerful` is required. `fast` and `cheap` default to the `powerful` model when not set.

```toml
[groups]
powerful = "anthropic/claude-opus-4-6"
fast = "anthropic/claude-sonnet-4-6"
cheap = "anthropic/claude-haiku-4-5-20251001"
```

Models use `developer/model_id` format. The developer prefix determines the API endpoint and wire format:

| Developer | Wire format | Default endpoint |
|-----------|-------------|------------------|
| `anthropic` | anthropic | anthropic |
| `google` / `gemini` | gemini | gemini |
| `openai` | openai | openai |
| Others (e.g. `deepseek`) | openai | openrouter |

Ungrouped call sites (`keepalive`, `count-tokens`) always use the session model regardless of group configuration.

---

## Call Site Overrides

Override which group a specific call site uses with `[groups.calls]`:

```toml
[groups.calls]
compaction = "cheap"        # use cheap model for compaction instead of powerful
spawn-clone = "fast"        # use fast model for clone spawns
```

Keys are call site names, values are group names (`powerful`, `fast`, `cheap`).

### Call Site Reference

| Call site | Default group | Description |
|-----------|--------------|-------------|
| `chat` | powerful | Main conversation |
| `spawn-clone` | powerful | Clone spawn (full context) |
| `background` | powerful | Background tasks |
| `compaction` | powerful | Context compaction |
| `memory-capture` | powerful | Memory extraction |
| `memory-consolidate` | powerful | Memory consolidation |
| `spawn-raw` | fast | Raw spawn (no character) |
| `spawn-character` | fast | Character spawn |
| `spawn-explore` | cheap | Exploration spawn |
| `summarize-tool` | cheap | Tool result summarization |
| `summarize-file` | cheap | File summarization |
| `prompt-diff` | cheap | Prompt diff generation |
| `keepalive` | *(session)* | Keepalive pings |
| `count-tokens` | *(session)* | Token counting |

---

## Runtime Overrides

### `/model` command (Telegram)

Switch the session model interactively:

```
/model anthropic/claude-opus-4-6   # switch to a specific model
/model                              # show keyboard / current model
```

Bare `/model` shows a keyboard with one button per model the agent's backend
advertises in the [live capability catalogue](#live-model-capability-catalogue),
with the current model marked by a check. If the catalogue is still cold (no
fetch has landed and nothing was restored from `state.db`), it falls back to
asking you to type the `developer/model_id` name.

### `--model` CLI flag

Override the model for a single `send` or `branch` request:

```bash
foci send --model fast "summarize this"
foci send -m anthropic/claude-opus-4-6 "think carefully about this"
foci branch --model anthropic/claude-haiku-4-5 "quick task"
```

The value can be a group name (`powerful`, `fast`, `cheap`) or a `developer/model_id` string.

Env var: `FOCI_MODEL` (flag takes precedence).

### Spawn tool `model` parameter

Agents can specify a model when spawning sub-agents:

```json
{"tool": "spawn", "input": {"mode": "raw", "model": "anthropic/claude-haiku-4-5", "task": "..."}}
```

### `/effort` command (Telegram)

The effort levels offered by `/effort` are sourced live from the
[capability catalogue](#live-model-capability-catalogue): a model advertises its
own set, so e.g. `opus-4-8` offers `low/medium/high/xhigh/max`. When the model
is unknown or the catalogue is cold, `/effort` falls back to the static
`low/medium/high` set. See [COMMANDS.md](COMMANDS.md) for usage detail.

---

## Live Model Capability Catalogue

Foci keeps a live, per-backend catalogue of model capabilities — context window,
max output, valid effort levels, and thinking modes — fetched from the backend's
`GET /v1/models` endpoint rather than hardcoded.

- **Per backend, not per model.** Capabilities are a property of the backend
  *type*, so each backend (`ccstream` for the Claude Code delegated loops, `api`
  for the direct Anthropic API loop) owns its own record. Both Anthropic-backed
  backends fill from `/v1/models`, but the records stay separate so a future
  non-Anthropic backend can carry its own capability source. Caps are read
  through the agent's backend (`Agent.ModelCaps`), never via a global lookup, so
  each agent sees only its own backend's record (`modelcaps.LookupFor(backend,
  model)`).
- **Where caps come from.** The fetch is seeded in the background at startup from
  the CC OAuth credentials. The catalogue is persisted to the `model_caps` table
  in `state.db` and restored synchronously on startup, so lookups don't miss the
  live caps during the brief gap before the first fetch lands. It re-fetches on
  every startup and then every 48h.
- **Fallback.** The static `internal/modelinfo` registry remains the fallback
  whenever credentials are absent, the API is unreachable, or a model isn't in
  the catalogue. First-ever runs (no persisted rows, cold cache) behave exactly
  as before the catalogue existed. Pricing and the speed/fast flag are not in the
  catalogue — the models API doesn't expose them — and stay in `modelinfo`.

Context-window and effort consumers prefer the live caps when present, sitting
between any explicit config override and the static registry.

---

## How Resolution Works

When a model value is provided (via config, command, or flag), resolution follows these steps:

1. **Named-model lookup.** First, the value is checked against the `[models.*]` map (see [Per-model configuration](#per-model-configuration-models)). A named-model entry can substitute a *different* `developer/model_id` than the key suggests, and carries per-model settings (thinking, effort, cache, provider routing, etc.). If the value matches a `[models.<name>]` table, that entry's `developer/model_id` (or the name itself if not overridden) is used as the resolved identifier, and the entry's settings are attached.
2. **Parse** — split the resulting `developer/model_id` string on `/` into `developer` and `model_id` (error if no slash)
3. **Wire format** — inferred from developer: `anthropic` → anthropic, `google`/`gemini` → gemini, `openai` → openai, others → openai (universal fallback)
4. **Endpoint** — auto-selected from developer (`anthropic` → anthropic endpoint, `google` → gemini endpoint, others → openrouter), or explicitly set via `endpoint` config

The named-model lookup runs *before* the `developer/model_id` string is parsed, so a `[models."anthropic/claude-haiku-4-5"]` entry can transparently reroute calls to a different underlying model without touching call sites.

For `--model` flag and `/model` command, the override is per-session: it applies to the target session and persists across restarts (stored in SessionIndex). Group names (`powerful`, `fast`, `cheap`) are resolved to their configured model before applying.

---

## Per-model configuration (`[models.*]`)

The `[models.*]` map lets you attach settings to a specific model identifier. Each key is a model name (typically a `developer/model_id` string); the sub-table holds per-model knobs. A `[models.*]` entry can also *substitute* a different underlying `developer/model_id` than its key, which is what makes the named-model lookup in [How Resolution Works](#how-resolution-works) able to reroute calls transparently.

Recognised per-model settings:

| Key | Purpose |
|-----|---------|
| `thinking` | Default thinking mode for the model |
| `effort` | Default effort level (e.g. `low`/`medium`/`high`) |
| `speed` | Default speed setting |
| `context` | Override the effective context window |
| `enable_keepalive` | Whether keepalive pings are sent for this model |
| `cache_ttl` | Prompt-cache TTL override |
| `cache_strategy` | Prompt-cache strategy override |

### Provider routing (`[models.*.provider]`)

For OpenRouter-backed models, a `[models.<name>.provider]` sub-table controls OpenRouter's provider selection. All keys are optional and map directly onto OpenRouter's routing parameters:

| Key | Purpose |
|-----|---------|
| `order` | Ordered list of preferred providers |
| `sort` | Sort mode for candidate providers |
| `ignore` | Providers to skip |
| `quantizations` | Allowed quantization levels |
| `max_price` | Price ceiling |
| `allow_fallbacks` | Whether to fall back to other providers |
| `data_collection` | Accept providers that log requests for training |

```toml
[models."anthropic/claude-haiku-4-5"]
effort = "low"
cache_ttl = "10m"

[models."anthropic/claude-haiku-4-5".provider]
order = ["Anthropic"]
allow_fallbacks = true
max_price = 5
```

---

## Registry overrides (`[[modelinfo]]`)

The static `internal/modelinfo` registry is the fallback source of truth for pricing, context windows, and capability flags when the [live capability catalogue](#live-model-capability-catalogue) is cold or unavailable. You can extend or override it from config with `[[modelinfo]]` entries.

Each entry can:

- **Override** pricing, context window, or capability flags for a model that already exists in the registry.
- **Define** an entirely new model the registry doesn't ship (useful for self-hosted or custom endpoints).

Capability flags you can set:

| Flag | Meaning |
|------|---------|
| `can_effort` | Model accepts effort levels |
| `can_thinking` | Model supports extended thinking |
| `can_speed` | Model supports the speed toggle |
| `can_caching` | Model benefits from prompt caching |

```toml
[[modelinfo]]
name = "anthropic/claude-haiku-4-5"
context = 200000
can_effort = true
can_thinking = true
can_caching = true

[[modelinfo]]
name = "local/my-finetune"
context = 32768
can_effort = false
can_thinking = false
can_caching = false
```

---

## Fallback chains (`[groups.fallbacks]`)

When a model fails (rate limit, 5xx, etc.), Foci can fall back to a designated alternate model. Per-model failover chains are configured in `[groups.fallbacks]`: the key is the failing model, the value is the model to try next.

```toml
[groups.fallbacks]
"anthropic/claude-haiku-4-5" = "anthropic/claude-opus-4-6"
"anthropic/claude-sonnet-4-6" = "google/gemini-2.5-pro"
```

With the above, a failed call to `anthropic/claude-haiku-4-5` is automatically retried against `anthropic/claude-opus-4-6`. Fallbacks apply to the API backend only — the delegated backends (CC, Codex, OpenCode) pick their own fallback internally and ignore this table.

---

## Routing variants (`:nitro`, `:floor`, `:free`, `:thinking`)

OpenRouter-style routing variants are silently supported as suffixes on a model id: `anthropic/claude-haiku-4-5:nitro`, `...:floor`, `...:free`, `...:thinking`. When an exact variant lookup matches a known model, it is used as-is. When the variant string does not resolve to a known model, the lookup falls back to the **base model** (the part before the `:`), so a typo or unsupported variant degrades gracefully rather than erroring.

---

## Examples

### Single model (simplest)

```toml
[groups]
powerful = "anthropic/claude-sonnet-4-6"
```

### Cost optimization

```toml
[groups]
powerful = "anthropic/claude-opus-4-6"
fast = "anthropic/claude-sonnet-4-6"
cheap = "anthropic/claude-haiku-4-5-20251001"

[groups.calls]
compaction = "cheap"       # compaction doesn't need the powerful model
```

### Mixed developers

```toml
[groups]
powerful = "anthropic/claude-opus-4-6"
fast = "anthropic/claude-sonnet-4-6"
cheap = "google/gemini-2.5-flash"    # use Gemini Flash for cheap tasks
```

### Custom endpoint

```toml
[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"

[groups]
powerful = "local/my-model"
```

### CLI override examples

```bash
# Use fast group model for a quick task
foci send --model fast "what time is it?"

# Use a specific model for a branch
foci branch -m anthropic/claude-opus-4-6 --oneshot "deep analysis task"

# Set via environment for cron
FOCI_MODEL=cheap foci send "routine check"
```

---

## See Also

- [CONFIG.md](CONFIG.md) — full configuration reference (`[groups]`, `[endpoints]`)
- [COMMANDS.md](COMMANDS.md) — Telegram command reference (`/model`, `/effort`, `/thinking`)
- [CLI.md](CLI.md) — CLI command reference (`--model` flag)
- [WIRING.md](WIRING.md) — architecture and startup flow
