# Task: More Config Defaults (Round 2)

Reduce config boilerplate by adding more smart defaults. Same pattern as the previous defaults work (54b5c79d).

## Defaults to add

### 1. `data_dir` → `$HOME/data`
If not set, default to `$HOME/data`.

### 2. Agent `name` → capitalised `id`
If not set, default to `strings.Title(id)` (or equivalent — first letter uppercase). E.g. `clutch` → `Clutch`, `scout` → `Scout`.

### 3. Agent `memory.sources` → single default source
If no memory sources specified, default to:
```
name = $id
dir = $workspace/memory
weight = 1.0
```
This matches the pattern every agent currently uses.

### 4. `sessions.dir` → `$data_dir/sessions`
If not set, default to `$data_dir/sessions`.

### 5. Logging paths → `$HOME/logs/` and `$data_dir/`
- `event_file` → `$HOME/logs/foci.log`
- `api_file` → `$HOME/logs/api.jsonl`
- `payload_file` → `$HOME/logs/api-payload.jsonl`
- `conversation_file` → `$data_dir/conversation.db`

### 6. Bot `token_secret` → `telegram.$botname`
If not set, default to `telegram.<bot-key-name>`. Every bot currently follows this exact pattern.

## Implementation notes

- Apply defaults in `Load()` like the previous round
- Order matters: `data_dir` default must be set before `sessions.dir` and `conversation_file` defaults
- `workspace` default must be set before `memory.sources` default (needs `$workspace`)
- Update SPEC.md, docs/CONFIG.md with new default values
- Update any examples in docs to show minimal config

## Tests

- Each default works when the key is omitted
- Explicit values still override defaults
- Build, test, vet all pass
