# Task: CLI help flags for all subcommands

## Problem
`foci send -h` hangs — it tries to connect to the gateway and send "-h" as a message instead of showing help. `--help` and `-h` should show usage for the base CLI and all subcommands.

## Requirements
1. `foci -h` and `foci --help` → show top-level usage (already works via `foci --help`)
2. `foci send -h` and `foci send --help` → show send-specific usage
3. `foci branch -h` and `foci branch --help` → show branch-specific usage
4. Same for all other subcommands: `status`, `eval`, `command`, `ping`
5. Should exit immediately with status 0, not connect to gateway

## Implementation
Check for `-h` or `--help` in args before processing flags/connecting. Each subcommand should have its own usage text showing relevant flags.

## Update docs
- docs/CLI.md with any changes
