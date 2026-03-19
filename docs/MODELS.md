# Model Configuration

Foci uses model groups to route different tasks to different models. This guide covers how to configure models, route tasks, and override models at runtime.

---

## Model Groups

Group assignments are configured in `[groups]`, and per-model settings in `[models.*]`. Three groups are available:

| Group | Purpose | Default call sites |
|-------|---------|-------------------|
| **powerful** | Primary tasks | chat, spawn-clone, background, compaction, memory-capture, memory-consolidate |
| **fast** | Quick spawns | spawn-raw, spawn-character |
| **cheap** | Bulk/utility work | spawn-explore, summarize-tool, summarize-file, prompt-diff |

`powerful` is required. `fast` and `cheap` default to the `powerful` model when not set.

```toml
[groups]
powerful = "opus"           # model name — see [models.*] below
fast = "sonnet"
cheap = "haiku"

[models.opus]
model = "anthropic/claude-opus-4-6"
thinking = "adaptive"
effort = "low"

[models.sonnet]
model = "anthropic/claude-sonnet-4-6"

[models.haiku]
model = "anthropic/claude-haiku-4-5-20251001"
```

Models use `developer/model_id` format in their `model` field. The developer prefix determines the API endpoint and wire format:

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

## Named Models

Named models are defined in `[models.*]` sections. Each entry maps a short name to a `developer/model_id` with optional per-model settings:

```toml
[models.opus]
model = "anthropic/claude-opus-4-6"
thinking = "adaptive"
effort = "low"

[models.sonnet]
model = "anthropic/claude-sonnet-4-6"
thinking = "adaptive"

[models.flash]
model = "google/gemini-2.5-flash"
thinking = "adaptive"

[models.deepseek]
model = "deepseek/deepseek-chat"
# enable_keepalive auto-detected (5m prompt cache TTL)
```

Per-model settings:

| Key | Description |
|-----|-------------|
| `model` | Full `developer/model_id` string (required) |
| `thinking` | `"adaptive"` or `"off"` |
| `effort` | `"low"`, `"medium"`, `"high"` |
| `speed` | `"fast"` or `""` (Opus only) |
| `enable_keepalive` | `nil` (auto-detect), `true`, `false` |
| `prompt_cache_ttl` | Go duration (e.g. `"5m"`) for keepalive interval |

Model names are used everywhere models are accepted: `[groups]`, `/model` command, `--model` CLI flag, and the spawn tool's `model` parameter. Raw `developer/model_id` strings also work (without per-model settings).

---

## Runtime Overrides

### `/model` command (Telegram)

Switch the session model interactively:

```
/model opus          # switch to alias
/model anthropic/claude-opus-4-6   # switch to full model ID
/model               # show current model
```

### `--model` CLI flag

Override the model for a single `send` or `branch` request:

```bash
foci send --model fast "summarize this"
foci send -m opus "think carefully about this"
foci branch --model haiku "quick task"
```

The value can be a group name (`powerful`, `fast`, `cheap`), an alias (`opus`, `haiku`), or a full `developer/model_id`.

Env var: `FOCI_MODEL` (flag takes precedence).

### Spawn tool `model` parameter

Agents can specify a model when spawning sub-agents:

```json
{"tool": "spawn", "input": {"mode": "raw", "model": "haiku", "task": "..."}}
```

---

## How Resolution Works

When a model value is provided (via config, command, or flag), resolution follows these steps:

1. **Named model lookup** — if the value matches a key in `[models.*]`, the `ModelConfig` is loaded (providing `model`, `thinking`, `effort`, `speed`, `enable_keepalive`, `prompt_cache_ttl`)
2. **Parse** — split the `model` field (or raw string) on `/` into `developer` and `model_id` (error if no slash)
3. **Wire format** — inferred from developer: `anthropic` → anthropic, `google`/`gemini` → gemini, `openai` → openai, others → openai (universal fallback)
4. **Endpoint** — auto-selected from developer (`anthropic` → anthropic endpoint, `google` → gemini endpoint, others → openrouter), or explicitly set via `endpoint` config
5. **Per-model settings** — `ResolvedModel` carries thinking, effort, speed, enable_keepalive, and prompt_cache_ttl from the `ModelConfig`

For `--model` flag and `/model` command, the override is per-session: it applies to the target session and persists across restarts (stored in SessionIndex). Group names (`powerful`, `fast`, `cheap`) are resolved to their configured model before applying.

---

## Examples

### Single model (simplest)

```toml
[groups]
powerful = "sonnet"

[models.sonnet]
model = "anthropic/claude-sonnet-4-6"
```

### Cost optimization

```toml
[groups]
powerful = "opus"
fast = "sonnet"
cheap = "haiku"

[groups.calls]
compaction = "cheap"       # compaction doesn't need the powerful model

[models.opus]
model = "anthropic/claude-opus-4-6"
thinking = "adaptive"
effort = "low"

[models.sonnet]
model = "anthropic/claude-sonnet-4-6"

[models.haiku]
model = "anthropic/claude-haiku-4-5-20251001"
```

### Mixed developers

```toml
[groups]
powerful = "opus"
fast = "sonnet"
cheap = "flash"            # use Gemini Flash for cheap tasks

[models.opus]
model = "anthropic/claude-opus-4-6"

[models.sonnet]
model = "anthropic/claude-sonnet-4-6"

[models.flash]
model = "google/gemini-2.5-flash"
thinking = "adaptive"
```

### Custom endpoint

```toml
[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"

[groups]
powerful = "local"

[models.local]
model = "local/my-model"
```

### CLI override examples

```bash
# Use fast group model for a quick task
foci send --model fast "what time is it?"

# Use a specific model for a branch
foci branch -m opus --oneshot "deep analysis task"

# Set via environment for cron
FOCI_MODEL=cheap foci send "routine check"
```

---

## See Also

- [CONFIG.md](CONFIG.md) — full configuration reference (`[groups]`, `[models.*]`, `[endpoints]`)
- [CLI.md](CLI.md) — CLI command reference (`--model` flag)
- [WIRING.md](WIRING.md) — architecture and startup flow
