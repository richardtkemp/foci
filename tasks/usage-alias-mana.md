# Task: Make /usage an undocumented alias for /mana

## Problem
`/usage` and `/mana` are redundant commands. `/mana` is the primary one (configurable name, shows mana percentage). `/usage` shows raw usage data but isn't useful enough to justify its own command.

## Requirements
1. Remove `NewUsageCommand` — replace with registering `/usage` as an alias that calls the same handler as `/mana`
2. `/usage` should not appear in `/help` output (undocumented alias)
3. `/mana` (or whatever the configured name is) remains the documented command

## Implementation
- Remove `command.NewUsageCommand` and its registration
- Register `/usage` as a hidden alias pointing to the mana handler
- Or simply: in the mana command, also register "usage" as an alternate name

## Update docs
- Remove /usage from any docs if listed
- Keep /mana documented
