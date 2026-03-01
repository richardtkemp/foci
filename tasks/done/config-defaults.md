# Task: Improve Agent Config Defaults

Three config fields should have sensible defaults derived from the agent ID, reducing boilerplate in foci.toml.

## Changes

### 1. workspace default → `~/$id`
If `workspace` is empty, default to `$HOME/$id` (e.g. agent id `clutch` → `/home/foci/clutch`).

### 2. telegram_bot default → `$id`
If `telegram_bot` is empty, default to the agent's `id` value (e.g. agent id `clutch` → bot key `clutch`). Only if that bot key exists in `[telegram.bots]`.

### 3. Rename image_save_dir → received_files_dir
The field stores all received files (images, videos, video notes, documents), not just images. Rename throughout:
- Config field: `image_save_dir` → `received_files_dir` (in AgentConfig, TelegramConfig)
- TOML key: `image_save_dir` → `received_files_dir`
- Default: if empty, default to `$workspace/received_files` (e.g. `/home/foci/clutch/received_files`)
- No backward compat needed — clean break, just rename everything

## Implementation notes

- Apply defaults in `Load()` after reading the TOML, same place other defaults are set
- `$HOME` is whatever the process home dir is (os.UserHomeDir or the existing pattern)
- Update SPEC.md, docs/CONFIG.md
- Update all references in the codebase (grep for `ImageSaveDir` and `image_save_dir`)

## Post-implementation: Config and filesystem cleanup (done by Clutch, not CC)

After this is deployed, Clutch will:
1. Rename existing `images_received` dirs to `received_files` for all agents
2. Remove `workspace`, `telegram_bot`, and `received_files_dir` from foci.toml wherever they match the new defaults

## Tests

- Agent with no workspace set gets `$HOME/$id`
- Agent with no telegram_bot set gets `$id` if bot exists
- Agent with no received_files_dir gets `$workspace/received_files`
- Old `image_save_dir` key is gone (no backward compat)
