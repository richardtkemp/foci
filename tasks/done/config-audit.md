# Task: Config Audit — Global vs Per-Agent (#81)

## What
Audit all config options and produce a report of what's currently global-only that should also be configurable per-agent (with per-agent overriding global).

## Do NOT implement changes — just produce the report.

## How

1. Read through all fields in `config.go` — every struct (`TelegramConfig`, `SessionsConfig`, `LoggingConfig`, etc.)
2. Read through `AgentConfig` — what's already per-agent
3. For each global-only field, assess: does it make sense to vary per-agent?
4. Check `main.go` wiring to see which globals are already resolved with per-agent overrides

## Report format

Write the report to `docs/config-audit.md` with three sections:

### Already per-agent (with global fallback)
List fields that already have the `*bool` / per-agent override pattern. Confirm they work correctly.

### Should be per-agent
Fields that are currently global-only but would benefit from per-agent overrides. For each:
- Field name and current location
- Why per-agent makes sense
- Suggested implementation (which pattern to follow)

### Global-only is correct
Fields that should stay global (e.g. `data_dir`, `http.port`, `logging.level`). Brief rationale.

## Do not
- Change any code
- Change any config
- Implement any overrides

Just the report. Commit and push `docs/config-audit.md`.
