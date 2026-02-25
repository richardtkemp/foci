# Task: /prompts Command (#77)

## What it does
Show which prompts are configured, where they point, and what uses them. No content — just metadata.

## Output should include

1. **Configured prompts for this agent:**
   - Compaction summary prompt (path, whether file exists)
   - Session reset prompt (path, whether file exists)
   - Compaction handoff message (inline value)
   - Fork prompt (path or inline, whether file exists)

2. **Prompt files on disk:**
   - Scan the shared prompts dir (`/home/clod/shared/prompts/`) and agent workspace prompts dir (`{workspace}/prompts/`)
   - List each .md file found
   - Mark which ones are referenced by config vs orphaned

3. **Cron usage** (nice to have, can skip if complex):
   - Which prompt files are referenced in crontab entries
   - This might be too hard to detect automatically — skip if so

## Format
```
Configured prompts (agent: clutch):
  compaction_summary    /home/clod/shared/prompts/compaction-summary.md  ✓
  session_reset         /home/clod/shared/prompts/session-reset.md       ✓
  handoff_msg           [inline: 63 chars]
  fork_prompt           [default]

Prompt files on disk:
  /home/clod/shared/prompts/
    compaction-summary.md        [configured]
    daily-health-check.md        [cron/other]
    daily-memory-review.md       [cron/other]
    memory-formation.md          [cron/other]
    ...
  /home/clod/clutch/prompts/
    HEARTBEAT.md                 [cron/other]
```

## Implementation
- Add as a new slash command in the command handler
- Read config for prompt paths, check file existence
- Scan prompt directories with os.ReadDir
- Update SPEC.md, any command docs

## Verification
- `/prompts` shows accurate prompt config and files
- Files marked correctly as configured vs not
- `go build && go test ./... && go vet ./...`
