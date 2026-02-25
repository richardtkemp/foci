# Task: Write docs/CLI.md (#79)

## What
Document all `clod` CLI commands with usage, flags, and examples.

## How to find what to document
1. Read `cmd/clod/main.go` — all CLI subcommands are defined there
2. Check the usage/help strings already in the code
3. Run `clod --help` if useful

## Expected commands to document (verify against source)
- `clod send` — send message to agent session
- `clod branch` — fork a branch session
- Plus any other subcommands

## For each command, document
- Synopsis / one-line description
- Full usage with all flags
- Flag descriptions with defaults
- Examples (2-3 practical ones)
- Exit codes / error behaviour if relevant

## Format
Standard CLI reference markdown. See other docs in the docs/ directory for style.

## Also
- Add a link to CLI.md from SPEC.md (in the appropriate section)
- Update any existing references to CLI usage

## Verification
- All commands in the source are documented
- All flags are documented
- Examples are accurate
- `go build && go test ./... && go vet ./...` (no code changes expected, but verify)
