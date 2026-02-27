# Task: CLI --if-inactive flag

## Context
Todo #100. Foci's CLI branch command needs an `--if-inactive` flag — the opposite of the existing `--if-active`.

Use case: cron heartbeat branches should only fire when the session is INACTIVE. If the user is already talking, the heartbeat is redundant and wastes mana.

Example usage:
```
foci branch --oneshot --if-inactive 30m -a clutch "Check emails and calendar"
```

This means: "only start this branch if the target session has been inactive for at least 30 minutes."

## Requirements
1. Add `--if-inactive DURATION` flag to the CLI branch command
2. It should check the target agent's main session last activity time
3. If the session has been active within the specified duration, skip the branch silently (exit 0, no error)
4. Should work alongside existing flags like `--oneshot`
5. Update docs/CONFIG.md and any CLI help text
6. Write tests
7. Commit and push when done
