# Model Configuration

Foci uses model groups to route different tasks to different models. This guide covers how to configure models, route tasks, and override models at runtime.

---

## Model Groups

Group assignments are configured in `[groups]` using `developer/model_id` strings. Three groups are available:

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

1. **Parse** — split the `developer/model_id` string on `/` into `developer` and `model_id` (error if no slash)
2. **Wire format** — inferred from developer: `anthropic` → anthropic, `google`/`gemini` → gemini, `openai` → openai, others → openai (universal fallback)
3. **Endpoint** — auto-selected from developer (`anthropic` → anthropic endpoint, `google` → gemini endpoint, others → openrouter), or explicitly set via `endpoint` config

For `--model` flag and `/model` command, the override is per-session: it applies to the target session and persists across restarts (stored in SessionIndex). Group names (`powerful`, `fast`, `cheap`) are resolved to their configured model before applying.

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
