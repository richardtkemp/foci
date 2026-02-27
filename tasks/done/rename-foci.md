# Task: Plan the rename from foci → FOCI

**PLAN ONLY — do not implement anything. Return a detailed plan.**

## Context

"foci" is the current name for this project. The rename candidate is "FOCI" — Facets Of Coherent Identity. The binary, repo, imports, config, systemd service, docs — everything currently says "foci".

## What needs to change

Produce a comprehensive plan covering every place the name appears and what needs to change. Include:

1. **Go module path** — `module foci` in go.mod, all internal imports
2. **Binary names** — `focigw` (gateway), `foci` (CLI tool)
3. **GitHub repo** — `richardtkemp/foci` → what?
4. **Systemd service** — `foci.service`, `ExecStart=/usr/local/bin/focigw`
5. **Config files** — `foci.toml`, references in docs
6. **Setup script** — `setup.sh` references throughout
7. **Directory structure** — `/home/foci/`, system user `foci`, group `foci-secrets`
8. **SPEC.md** — title, references throughout
9. **Documentation** — all docs/ files
10. **Agent workspaces** — character files reference "foci" in various places
11. **Crontab entries** — any references to foci binary or paths
12. **Telegram bot names** — focibot etc (or are these separate?)
13. **Log file names** — `foci.log`
14. **Import paths in all .go files** — every `"foci/..."` import
15. **Test files** — any hardcoded references
16. **The prompts/ directory** — embedded prompts referencing "foci"

## Questions to answer in the plan

- What's the recommended order of operations? (Some changes have dependencies)
- What can be done atomically vs what needs a migration period?
- Is there a way to support both names during transition (symlinks, aliases)?
- What's the risk of breakage? What's the rollback plan?
- How long would the rename take to implement?
- Are there things that should NOT be renamed (e.g. system user, home dir)?

## Migration of existing user data

The plan must include migration steps for the current single-user installation:
- System user home directory (`/home/foci/`)
- Agent workspace directories and all files within
- Session JSONL files (paths baked into filenames?)
- SQLite databases (todo, reminders, scratchpad, memory search)
- Config file paths
- Crontab entries referencing foci paths
- systemd unit references
- Log file locations
- Any symlinks or path references in scripts

This migration is ad-hoc (script or manual steps) — there's only one user currently, so it doesn't need to be a built-in upgrade system. But the plan must be complete enough to not miss anything.

## Output

A detailed markdown plan. No code changes.
