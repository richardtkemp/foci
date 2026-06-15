# Configuration (`foci.toml` & `secrets.toml`)

Foci is configured by `foci.toml` (with secrets in a parallel `secrets.toml`), both in the config dir. You won't usually edit these mid-conversation, but understanding them explains *why you are the way you are*.

## Smart defaults: most config derives from the agent ID

A key foci principle — given an agent's `id`, foci auto-derives:

| Field | Derivation |
|-------|-----------|
| `workspace` | `$HOME/<id>` |
| `name` | `<id>` capitalised |
| `memory.sources` | a default source at `$workspace/memory` |
| `platforms[telegram].bot` | `<id>` (token looked up by this name) |
| `platforms[telegram].received_files_dir` | `$workspace/received_files` |
| `platforms[discord]` | auto-created only if a global Discord config exists |

So a minimal `[[agents]]` entry is often just `id = "..."` plus a model and any non-default overrides.

## Top-level shape

`foci.toml` has many tables; the ones that shape an agent's behaviour:

- `[[agents]]` — one block per agent (id, name, emoji, workspace, backend, per-agent overrides).
- `[[platforms]]` — messaging channels (Telegram, Discord) and global platform settings.
- per-agent `backend` + `backend_config` — backend selection and model (see below; `backend_config` is a per-agent table, not a global one).
- `[sessions]` — compaction threshold, max tokens, preserved messages, prompt files.
- `[memory]` — sources (combined additively per-agent), search backend, limits, conversation weight.
- `[keepalive]`, `[background]`, `[reflection]`, `[maintenance]` — the periodic timers (cache warmup, idle work, memory formation, consolidation/reset).
- `[nudge]`, `[behavior]`, `[voice]`, `[display]` — behavioural tuning.
- `[debug]` — verbose per-package logging flags (`extra_ccstream_logging`, etc.).

For durable scheduled turns, see **scheduled-tasks.md** (they live in the generated crontab).

Config **cascades** (per-platform-per-agent → per-agent → per-platform-global → global → code default); pointer fields take the first non-nil, slices combine, maps overlay. Per-agent fields must stay nil to inherit — that's how the merge works.

## Backends & models

- `backend`: empty or `"api"` = foci's own API loop; `"claude-code"` (CC streaming) or `"claude-code-tmux"` (CC via tmux) = delegate to that Claude Code backend. See **tools-backend.md**.
- `backend_config.model`: a model alias or `developer/model_id`. Per-agent overrides the global default. **Changing the model requires a restart to apply — `/reload` does not pick it up.**
- **Model groups** (used by the `spawn` tool and internal tasks): `powerful` (chat, clone, background, compaction, memory), `fast` (spawn raw/character), `cheap` (explore, summarise). Named models live in `[models.<alias>]` with fields like `model`, `thinking`, `effort`, `context`, `cache_ttl`.

## Secrets

- `secrets.toml` sits alongside `foci.toml` and holds API keys/tokens. **Never** in `foci.toml`, git, chat, or logs.
- Reference a secret as `{{secret:SECTION.KEY}}` (e.g. `{{secret:brave.api_key}}`) — resolved **server-side** at tool-execution time. In `foci_http_request` headers it's resolved against `allowed_hosts`; in a body/form field it additionally requires `allowed_in_body`.
- The system prompt lists the *names* of available secrets so you know what's referenceable — never the values.

## Provisioning / defaults

On startup foci **seeds** embedded default character files, prompts, and skills into `~/shared/` — but only files that don't already exist; existing files are never overwritten. So shipped-skill *edits* don't propagate to installs that already have the file; only brand-new files seed on the next start.

## Full reference

Don't reconstruct config from memory — the repo ships the authoritative sources:

- **`foci.toml.example`** (and `secrets.toml.example`) at the repo root — an exhaustive, commented example config covering every table with realistic values. The fastest way to see the actual shape.
- **`docs/CONFIG.md`** ("Foci Configuration Reference") — the full list of config keys, their types, defaults, and possible values, organised by table.

For durable scheduled turns (crontab), see **scheduled-tasks.md**.
