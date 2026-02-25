# Task: Config Audit Implementation (#94)

Implement the changes identified in docs/config-audit.md. This is a big task — commit in logical chunks.

## Part 1: Remove compaction_model

`compaction_model` should not exist. Compaction must always use the same model as the session it runs in. Remove the config field, remove any wiring, and ensure compaction uses the agent's own model.

## Part 2: Agent-only fields need global equivalents

These fields currently only exist on `AgentConfig` with no global default. Add global equivalents in `[telegram]` or a new `[defaults]` section (whichever fits the existing pattern), so agents inherit defaults and only override what's different.

Fields:
- `model`
- `heartbeat_interval`
- `duplicate_messages`
- `inject_agent_warnings`
- `max_tool_loops`
- `max_output_tokens`
- `tts_rate`
- `system_files`

## Part 3: Per-agent compaction settings

These are currently global in `[sessions]`. Add per-agent overrides:
- `compaction_threshold`
- `compaction_summary_prompt`
- `compaction_handoff_msg`
- `compaction_notify`

Use the established `*bool` / `*float64` / `*string` pattern where nil = use global.

## Part 4: Per-agent session_reset_prompt

Currently global in `[sessions]`. Add per-agent override.

## Part 5: Per-agent skills and prompt_rules

- `skills.dirs` — per-agent override of global `[skills] dirs`
- `prompt_rules` — per-agent override of global `[[prompt_rules]]`

## Part 6: Per-agent TTS config

Whatever TTS settings exist beyond `tts_rate` — make them per-agent.

## Part 7: Per-agent behavioural settings

- `exec_auto_background` (threshold for auto-backgrounding exec/http_request)
- `max_concurrent_spawns`

## Part 8: Per-agent UX/credentials

Any remaining behavioural or UX settings from the audit that make sense per-agent.

## Implementation notes

- Use the established pattern: `*type` on AgentConfig (nil = use global), concrete type on global config with `md.IsDefined` defaulting
- Wire resolution in main.go's setupAgent, same as show_tool_calls, startup_notification etc.
- Update docs/CONFIG.md for every new field (both tables)
- Update SPEC.md where relevant
- Commit each part separately for clean history

## Verification
After each part:
- `go build && go test ./... && go vet ./...`
- Final commit: push all
