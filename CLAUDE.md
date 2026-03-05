# Foci — Claude Code Instructions

## Before You Start

Read `docs/WIRING.md` for the full architecture and wiring diagram. It explains how all packages connect, the agent loop, session branching, and the startup flow.

Read `docs/SPEC.md` for the design intent and philosophy.

## When You Make Changes

- If you modify how packages connect, add new packages, change the startup flow, add tools, or alter the agent loop, **update `docs/WIRING.md`** to reflect the change.
- If you add a new feature, **check if it's appropriate to update `docs/COMPARISON.md`**, searching for additional info if required.
- No backward compatibility is required, the project has not been released yet. Breaking changes and major refactors are fine!! Don't leave ANYTHING hanging around as 'deprecated'

## Key Constraints

- **No circular imports.** `log` and `config` are leaf packages. Check the dependency graph in WIRING.md.
- **Cache sharing depends on byte-identical system prompts.** Don't modify workspace bootstrap behavior without understanding this.
- **Sessions are append-only JSONL.** Branch files have a `branch_meta` first line.
- **Secrets stay out of agent context.** Credentials are in Go structs, never in messages.

## Running

```
make build && ./bin/foci-gw -config foci.toml
make test
make lint
```

## Testing

All tests are self-contained. Run `make test` — should pass in ~1s.

`anthropic/cache_test.go` requires `ANTHROPIC_API_KEY` to run (skipped if not set). This is the critical integration test validating cache sharing across branched sessions.

**Always use `make test` instead of `go test`** — it runs with proper parallelization and consistent flags.

## Standards

- **Tests:** Write tests for all new functionality. Cover happy path, edge cases, and error conditions.
- **Docs:** Update `docs/CONFIG.md` for any new config options. Update `docs/WIRING.md` for any architectural or flow changes.
- **Config:** No magic numbers in code. Thresholds, intervals, model names, percentages, limits — anything that might reasonably vary per deployment — must be a config field with a sensible default. Add fields to the relevant config struct with TOML tags.
- **Config scope:** All config keys should be available both globally (in `[defaults]` or top-level sections) AND per-agent (in `[[agents]]`), unless it genuinely doesn't make sense per-agent (e.g. `workspace`, `id`). Per-agent values override global. This is the design principle — don't add global-only config unless there's a clear reason.
