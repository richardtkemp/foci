# Task: CLI help flags for all subcommands

## Problem
`clod send -h` hangs — it tries to connect to the gateway and send "-h" as a message instead of showing help. `--help` and `-h` should show usage for the base CLI and all subcommands.

## Requirements
1. `clod -h` and `clod --help` → show top-level usage (already works via `clod --help`)
2. `clod send -h` and `clod send --help` → show send-specific usage
3. `clod branch -h` and `clod branch --help` → show branch-specific usage
4. Same for all other subcommands: `status`, `eval`, `command`, `ping`
5. Should exit immediately with status 0, not connect to gateway

## Implementation
Check for `-h` or `--help` in args before processing flags/connecting. Each subcommand should have its own usage text showing relevant flags.

## Update docs
- docs/CLI.md with any changes
