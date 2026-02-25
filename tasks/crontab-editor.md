# Task: Safe Crontab Editing Script

## Problem
Editing crontab currently requires reading the entire file, modifying it in memory, and writing the whole thing back via `crontab -`. This is error-prone:
- A bug in the heredoc or quoting can silently drop entries
- No backup before writing
- No validation that the new crontab is valid
- No diff to show what changed

The orchestrating agent does this regularly (adding heartbeats, adjusting schedules, toggling entries).

## Proposed Solution
A shell script `~/scripts/crontab-edit.sh` with subcommands for safe operations.

### Interface
```bash
crontab-edit.sh list                          # show numbered crontab lines
crontab-edit.sh add "SCHEDULE" "COMMAND"      # append a new entry
crontab-edit.sh add-after PATTERN "LINE"      # add line after matching pattern
crontab-edit.sh remove PATTERN                # remove lines matching pattern (with confirmation output)
crontab-edit.sh disable PATTERN               # comment out matching lines (prepend #)
crontab-edit.sh enable PATTERN                # uncomment matching lines (remove leading #)
crontab-edit.sh replace PATTERN "NEW_LINE"    # replace matching line
crontab-edit.sh edit LINE_NUM "NEW_CONTENT"   # replace specific line by number
crontab-edit.sh diff                          # show diff between current and last backup
crontab-edit.sh backup                        # manual backup
crontab-edit.sh restore                       # restore from last backup
```

### Safety Features
1. **Auto-backup** — every mutation backs up current crontab to `~/.crontab-backups/YYYY-MM-DDTHH:MM:SS.bak` before writing
2. **Dry-run output** — every mutation prints a diff of what would change before applying
3. **Validation** — after writing, read back and verify the expected change was applied
4. **Pattern matching** — patterns match against the full line (grep -F by default, -E for regex)
5. **No silent failures** — if pattern matches zero lines, exit with error and message
6. **Preserve structure** — comments, blank lines, env vars all preserved

### Implementation Notes
- Pure bash, no dependencies beyond coreutils
- `--help` / `-h` on every subcommand
- Backup dir created on first use
- Keep last 50 backups, prune older ones
- All output to stderr except `list` (stdout for piping)
