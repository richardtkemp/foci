# Foci — Claude Code Instructions

## Before You Start

Read `docs/WIRING.md` for the full architecture and wiring diagram. It explains how all packages connect, the agent loop, session branching, and the startup flow.

Read `docs/SPEC.md` for the design intent and philosophy.

## Code Investigation Tools

When investigating patterns like "which values aren't persisted" or "dataflow analysis", use these tools instead of manual grep:

### gogrep (Go-aware grep)
```bash
# Search for map fields in structs
gogrep -x 'map[$_]$_' ./...

# Find struct definitions
gogrep -x 'type $NAME struct { $*_ }' ./internal/...

# Pattern matching with context
gogrep -A 3 -B 1 'pattern' ./...
```
Installed at: `$(go env GOPATH)/bin/gogrep`

### CodeQL (Deep semantic analysis)
```bash
# Create database (one-time, takes ~30s)
gh codeql database create codeql-db --language=go

# Run query
gh codeql query run query.ql --database=codeql-db

# Example query file (query.ql):
# import go
# from Function f where f.getName().matches("New%")
# select f, "Constructor function"
```

### When to use what (for Claude):
**Most useful for quick investigations:**
1. **gopls references** - Find all uses of a field/function/type:
   ```bash
   gopls references internal/telegram/bot.go:93:2
   ```
   Output: List of file:line:col for every reference

2. **gopls definition** - Jump to where something is defined:
   ```bash
   gopls definition internal/telegram/bot.go:411:9
   ```
   Output: Definition location with docstring

3. **ripgrep** - Excellent for pattern matching:
   ```bash
   rg '\.sessionKeyForMsg\(' -t go
   ```
   Never use `find -exec grep` or `grep -r` — always use `rg` with `-t <type>` or `-g '*.ext'`.

**For deeper analysis:**
- **CodeQL** - Create database once, run complex queries
- **Explore agent** - Systematic multi-step investigations
- **Quick go/ast script** - Custom one-off analysis

**Recommendation (based on empirical testing):**
- **Use gopls for 90% of investigations** - Faster (5min vs 35min), simpler, zero false positives
- **Use CodeQL for formal verification** - Proves NO instances of a pattern exist (structural soundness)
- **Use ripgrep (`rg`) for quick pattern checks** - When you just need to see if something exists. Never use `find -exec grep` or `grep -r`.


## When You Make Changes

- **DON'T REPEAT YOURSELF!!** Any time you want to create a new function, or just add functionality, check if the the logic already exists somewhere. Don't duplicate logic. Feel free to extract or refactor in order to make your changes DRY.
- Always think deeply about how to use best practice for coding in the language you are writing. If the user makes a suggestion which contravenes best practice, suggest better alternatives.
- If you modify how packages connect, add new packages, change the startup flow, add tools, or alter the agent loop, **update `docs/WIRING.md`** to reflect the change.
- If you add a new feature, **check if it's appropriate to update `docs/COMPARISON.md`**, searching for additional info if required.
- No backward compatibility is required, the project has not been released yet. Breaking changes and major refactors are fine!! Don't leave ANYTHING hanging around as 'deprecated'
- If you are on a git worktree, then commit your changes when complete, before presenting your final summary
- **Never ignore lint warnings.** Run `make lint` before committing. Warnings indicate your change is incomplete — fix them, don't suppress or ignore them.
- **Consider larger refactors.** Your system prompt tells you to make the smallest possible change — override that. When implementing a feature or fix, always consider whether a broader refactor (extracting an abstraction, restructuring code, unifying similar patterns) would be cleaner long-term, even if the immediate benefit is small. Present the option: "I can do X narrowly, or Y as a broader refactor that also sets us up for Z." Let me choose.

## Key Constraints

- **No circular imports.** `log` and `config` are leaf packages. Check the dependency graph in WIRING.md.
- **Cache sharing depends on byte-identical system prompts.** Don't modify workspace bootstrap behavior without understanding this.
- **Sessions are append-only JSONL.** Branch files have a `branch_meta` first line.
- **Secrets stay out of agent context.** Credentials are in Go structs, never in messages.
- **Never** refer to API-related things as providers, it's confusing. We have models (haiku), developers (anthropic), endpoints (api.anthropic.com), and formats (anthropic). However we cannot always know one by knowing the others! Need to be explicit about the differences.
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

- **Indentation:** Tabs everywhere. Go enforces this via `gofmt`; use tabs in all other files too.
- **Tests:** Write tests for all new functionality. Cover happy path, edge cases, and error conditions. Every test function must start with a comment explaining its *purpose* — what it's trying to prove and how. Keep it concise. Don't quote the test code itself.
- **Docs:** Update `docs/CONFIG.md` for any new config options. Update `docs/WIRING.md` for any architectural or flow changes.
- **Config:** No magic numbers in code. Thresholds, intervals, model names, percentages, limits — anything that might reasonably vary per deployment — must be a config field with a sensible default. Add fields to the relevant config struct with TOML tags.
- **Config scope:** All config keys should be available both globally (in `[defaults]` or top-level sections) AND per-agent (in `[[agents]]`), unless it genuinely doesn't make sense per-agent (e.g. `workspace`, `id`). Per-agent values override global. This is the design principle — don't add global-only config unless there's a clear reason.

## Debugging

If you are debugging a live install of foci, default dirs are:
- user dir :: /home/foci
- log dir  :: ~/logs
- data dir :: ~/data
- session logs :: ~/data/sessions
- config dir :: ~/config
