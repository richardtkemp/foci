# Clod — Claude Code Instructions

## Before You Start

Read `docs/WIRING.md` for the full architecture and wiring diagram. It explains how all packages connect, the agent loop, session branching, and the startup flow.

Read `SPEC.md` for the design intent and philosophy.

## When You Make Changes

If you modify how packages connect, add new packages, change the startup flow, add tools, or alter the agent loop, **update `docs/WIRING.md`** to reflect the change.

## Key Constraints

- **No circular imports.** `log` and `config` are leaf packages. Check the dependency graph in WIRING.md.
- **Cache sharing depends on byte-identical system prompts.** Don't modify workspace bootstrap behavior without understanding this.
- **Sessions are append-only JSONL.** Branch files have a `branch_meta` first line.
- **Secrets stay out of agent context.** Credentials are in Go structs, never in messages.

## Running

```
go build -o clod . && ./clod -config clod.toml
go test ./...
go vet ./...
```

## Testing

All tests are self-contained except `anthropic/cache_test.go` which needs `ANTHROPIC_API_KEY`. Run `go test ./...` — should pass in ~1s.

## Standards

- **Tests:** Write tests for all new functionality. Cover happy path, edge cases, and error conditions.
- **Docs:** Update `docs/CONFIG.md` for any new config options. Update `docs/WIRING.md` for any architectural or flow changes.
- **Config:** New behaviour should be configurable where appropriate. Add fields to the relevant config struct with TOML tags and sensible defaults.
- **Config scope:** All config keys should be available both globally (in `[defaults]` or top-level sections) AND per-agent (in `[[agents]]`), unless it genuinely doesn't make sense per-agent (e.g. `workspace`, `id`). Per-agent values override global. This is the design principle — don't add global-only config unless there's a clear reason.
