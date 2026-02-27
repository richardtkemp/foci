# Task: Log privacy — messages_in_log config

## Context
User messages are currently written in full to the event log. This is a privacy issue. Add a config key to control this.

## Requirements

1. New config key `messages_in_log` (boolean, global and per-agent, default **false**)
2. When **false**: messages log at DEBUG level with generic text like "message from user" — no content included
3. When **true**: messages log at INFO level with full message content (current behaviour)
4. This applies to the event log (foci.log), not the API payload log
5. Update docs/CONFIG.md and SPEC.md
6. Write tests
7. Commit and push when done
