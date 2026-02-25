# Task: Directory-based crontab management

## Overview
Replace monolithic crontab editing with a directory structure where each cron job is its own file. Clod's gateway watches the directories and auto-rebuilds the system crontab by concatenating all files.

## Directory Structure
```
~/crontab/
├── _env                          # env vars (PATH, CLOD_ADDR, etc) — goes at top of crontab
├── clutch/
│   ├── memory-formation.cron
│   ├── heartbeat.cron
│   └── morning-routine.cron
├── fotini/
│   ├── heartbeat.cron
│   └── memory-formation.cron
└── helen/
    ├── heartbeat.cron
    └── daily-vocab-review.cron
```

Each `.cron` file contains one or more crontab lines (schedule + command). Files can include comments. The filename is purely descriptive — the actual schedule is in the file content.

## Config
```toml
[crontab]
enabled = true                    # default true, global only (not per-agent)
dir = "crontab"                   # relative to agent home dir, default "crontab"
```

`enabled = false` disables the watcher entirely — clod won't touch the system crontab.

## Behaviour

### Rebuild trigger
- **On startup** — always rebuild from directory contents
- **On file change** — use fsnotify to watch `dir` recursively. Any create/modify/delete/rename triggers a rebuild
- **Debounce** — 500ms debounce on fsnotify events (multiple rapid file changes = one rebuild)

### Rebuild process
1. Back up current crontab to `~/.crontab-backups/YYYY-MM-DDTHH:MM:SS.bak`
2. Read `_env` file if it exists → prepend to output
3. Walk all subdirectories, read all `.cron` files
4. Sort by: directory name (agent), then filename (for deterministic ordering)
5. For each agent directory, emit a section divider:
   ```
   # ===== clutch =====
   ```
6. Within each agent section, each file gets a comment: `# --- heartbeat.cron ---`
7. Concatenate all content with blank line separators
7. Install via `crontab -` 
8. Log: "crontab rebuilt: N jobs from M agents"

### Backup management
- Keep last 50 backups in `~/.crontab-backups/`
- Prune older ones on each rebuild

### Validation
Before installing, validate each non-comment, non-blank, non-env line:
- Must have at least 6 fields (5 time fields + command)
- Time fields should match cron patterns (digits, `*`, `/`, `-`, `,`)
- If validation fails: log WARNING with file name and line, skip that file, install the rest
- `crontab -` also validates at install time — if it rejects, log ERROR, restore from backup

### Error handling
- If `crontab -` fails, log ERROR and restore from the auto-backup
- If the directory doesn't exist, create it on startup (with the configured agent subdirectories)

### Bootstrap
On first run (directory doesn't exist or is empty):
1. Create the directory structure based on configured agents
2. Parse the current system crontab and split into per-agent files based on `-a AGENT` flags in the commands
3. Lines without `-a` go into a `_general/` subdirectory
4. Log: "migrated N crontab entries into directory structure"

This means existing crontabs are automatically migrated — no manual work needed.

## What it does NOT do
- No validation of cron schedule syntax (crontab handles this)
- No per-agent config — this is a global system feature
- No UI/slash command (yet) — just file-based management
- Doesn't manage non-clod crontab entries from other users

## File format
Each `.cron` file is plain crontab format:
```
# Heartbeat: autonomous check-in (only when idle 45+ min)
*/45 * * * * clod branch --oneshot --if-inactive 45m -a clutch -mf clutch/prompts/HEARTBEAT.md 2>&1 >> /home/clod/logs/cron.log
```

Comments and blank lines are preserved.

## Implementation Notes
- fsnotify is already a dependency (used by tmux watch) — check, if not, add it
- The watcher runs in a goroutine started from main.go
- Rebuild is mutex-protected (one rebuild at a time)
- Tests: create temp dir structure, verify concatenation order, verify backup creation, verify bootstrap migration
- Update SPEC.md, docs/CONFIG.md, docs/WIRING.md
- Commit and push when done

## Migration
When this ships, the existing monolithic crontab gets automatically split into the directory structure on first run. After that, all crontab management happens through file operations on individual `.cron` files.
