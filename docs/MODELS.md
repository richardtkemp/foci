# Model Configuration

Foci supports single-model and multi-model setups. This guide covers how to configure models, route different tasks to different models, and override models at runtime.

---

## Single-Model Mode (Default)

By default, every agent uses one model for all tasks. Set it in `[llm]`:

```toml
[llm]
model = "anthropic/claude-sonnet-4-6"
```

Per-agent override:

```toml
[[agents]]
id = "research"
model = "gemini/gemini-2.5-flash"
```

Models use `developer/model_id` format. The developer prefix determines the API endpoint and wire format:

| Developer | Wire format | Default endpoint |
|-----------|-------------|------------------|
| `anthropic` | anthropic | anthropic |
| `google` / `gemini` | gemini | gemini |
| `openai` | openai | openai |
| Others (e.g. `deepseek`) | openai | openrouter |

---

## Multi-Model Mode

When `[models] powerful` is set, foci routes different tasks to different models. Three groups are available:

| Group | Purpose | Default call sites |
|-------|---------|-------------------|
| **powerful** | Primary tasks | chat, spawn-clone, background, compaction, memory-capture, memory-consolidate |
| **fast** | Quick spawns | spawn-raw, spawn-character |
| **cheap** | Bulk/utility work | spawn-explore, summarize-tool, summarize-file, prompt-diff |

`fast` and `cheap` default to the `powerful` model when not set.

```toml
[models]
powerful = "opus"           # alias — see Model Aliases below
fast = "sonnet"
cheap = "haiku"
```

Ungrouped call sites (`keepalive`, `count-tokens`) always use the session model regardless of group configuration.

---

## Call Site Overrides

Override which group a specific call site uses with `[models.calls]`:

```toml
[models.calls]
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

## Model Aliases

Aliases map short names to full `developer/model_id` identifiers. Configure in `[models.aliases]`:

```toml
[models.aliases]
opus = "anthropic/claude-opus-4-6"
sonnet = "anthropic/claude-sonnet-4-6"
haiku = "anthropic/claude-haiku-4-5"
flash = "gemini/gemini-2.5-flash"
local = "local/my-fine-tuned-model"
```

Built-in defaults (used when `[models.aliases]` is not configured):

| Alias | Resolves to |
|-------|------------|
| `opus` | `anthropic/claude-opus-4-6` |
| `sonnet` | `anthropic/claude-sonnet-4-6` |
| `haiku` | `anthropic/claude-haiku-4-5` |
| `flash` | `gemini/gemini-2.5-flash` |
| `pro` | `gemini/gemini-2.5-pro` |
| `gpt4o` | `openai/gpt-4o` |
| `o3` | `openai/o3` |
| `o4mini` | `openai/o4-mini` |

Aliases are used everywhere models are accepted: `[models]` groups, `/model` command, `--model` CLI flag, and the spawn tool's `model` parameter.

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

1. **Alias lookup** — if the value matches a key in `[models.aliases]`, it's replaced with the alias target
2. **Parse** — split on `/` into `developer` and `model_id` (error if no slash)
3. **Wire format** — inferred from developer: `anthropic` → anthropic, `google`/`gemini` → gemini, `openai` → openai, others → openai (universal fallback)
4. **Endpoint** — auto-selected from developer (`anthropic` → anthropic endpoint, `google` → gemini endpoint, others → openrouter), or explicitly set via `endpoint` config

For `--model` flag and `/model` command, the override is per-session: it applies to the target session and persists across restarts (stored in SessionIndex). Group names (`powerful`, `fast`, `cheap`) are resolved to their configured model before applying.

---

## Examples

### Single model (simplest)

```toml
[llm]
model = "anthropic/claude-sonnet-4-6"
```

### Multi-model with cost optimization

```toml
[llm]
model = "anthropic/claude-sonnet-4-6"    # default/session model

[models]
powerful = "opus"
fast = "sonnet"
cheap = "haiku"

[models.calls]
compaction = "cheap"       # compaction doesn't need the powerful model
```

### Mixed developers

```toml
[llm]
model = "anthropic/claude-opus-4-6"

[models]
powerful = "opus"
fast = "sonnet"
cheap = "flash"            # use Gemini Flash for cheap tasks

[models.aliases]
flash = "gemini/gemini-2.5-flash"
```

### Custom endpoint

```toml
[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"

[llm]
model = "local/my-model"

[models.aliases]
local = "local/my-model"
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

- [CONFIG.md](CONFIG.md) — full configuration reference (`[models]`, `[endpoints]`, `[llm]`)
- [CLI.md](CLI.md) — CLI command reference (`--model` flag)
- [WIRING.md](WIRING.md) — architecture and startup flow
