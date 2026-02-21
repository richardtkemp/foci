# Scripts Directory Reorganization - Executive Summary

## Problem
31 files in flat `scripts/` directory - mix of active tools, analysis scripts, monitoring, and temporary experiments. Hard to find things, unclear what's production vs experimental.

## Solution
Organize into 6 subdirectories by purpose:

```
scripts/
├── coding-agents/    (7 files) - Coding agent automation
├── monitoring/       (8 files) - System & service monitoring  
├── token-analysis/   (7 files) - Token tracking & optimization
├── maintenance/      (6 files) - Regular maintenance tasks
├── skills/           (2 files) - Skill utilities
└── archive/          (1 file)  - Deprecated scripts
```

## Impact

### What Needs Manual Updates
**USER CRONTAB (4 entries) - MUST UPDATE:**
```bash
# Run this after migration:
crontab -e

# Change these 4 paths:
scripts/gcalcli-weekly-reauth.sh     → scripts/maintenance/gcalcli-weekly-reauth.sh
scripts/check-openclaw-branch.sh     → scripts/maintenance/check-openclaw-branch.sh  
scripts/track_claude_usage.sh        → scripts/coding-agents/track_claude_usage.sh
scripts/check-memory-size.sh         → scripts/maintenance/check-memory-size.sh
```

### What Gets Auto-Updated
These will be updated as part of the migration:
- ✅ OpenClaw cron jobs (`cron/jobs.json`)
- ✅ HEARTBEAT.md morning routine
- ✅ AGENTS.md skill installer reference
- ✅ TOOLS.md coding agent references
- ✅ Skill files (claude-or-opencode, moltbook)
- ✅ Internal script calls

### What's Safe (No Impact)
- ✅ No Docker volumes mounting scripts/
- ✅ No systemd services referencing scripts/
- ✅ No symlinks to break
- ✅ No Netdata hardcoded paths (uses webhook)

## Migration Steps

1. **Create subdirectories** (2 min)
2. **Move files with `git mv`** (5 min)
3. **Update all references** (20 min)
   - cron/jobs.json
   - HEARTBEAT.md
   - AGENTS.md
   - TOOLS.md
   - skills/*/SKILL.md
   - Internal script paths
4. **Update user crontab** (5 min) ⚠️ MANUAL STEP
5. **Test critical scripts** (15 min)
   - Cron jobs
   - Token tracking
   - Coding agent launch
6. **Create README files** (10 min)
7. **Commit & push** (3 min)

**Total time:** ~60 minutes

## Risk Assessment

**Medium Risk** - User crontab requires manual update

**Mitigation:**
- Keep backup branch for quick rollback
- Test all cron jobs manually after migration
- Monitor logs for 24 hours after migration
- Document exact commands for rollback

## Benefits

1. **Clearer organization** - Easy to find scripts by purpose
2. **Better documentation** - README in each subdirectory
3. **Easier onboarding** - New users can navigate by category
4. **Safer deprecation** - Clear archive/ for old scripts
5. **Git history preserved** - Using `git mv` keeps full history

## Deprecated Scripts to Mark

These work but have better alternatives:
- `coding-agent-tmux-watcher.sh` → Use agent-specific watchers
- `launch-claude-tmux.sh` → Use `launch-coding-agent-tmux.sh -a claude`

Mark with deprecation notice, remove after 1 month if unused.

## Quick Reference After Migration

```bash
# Daily token tracking
python3 scripts/token-analysis/token-usage-tracker.py
python3 scripts/token-analysis/token-usage-grapher.py

# Launch coding agent
bash scripts/coding-agents/launch-coding-agent-tmux.sh -a claude ...

# Install skill
bash scripts/skills/install-skill.sh <skill-name>

# Monitoring
bash scripts/monitoring/monitor-coolify-health.sh
bash scripts/monitoring/container-uptime-report.py

# Maintenance
bash scripts/maintenance/cleanup-idle-tmux.sh
bash scripts/maintenance/morning-routine.sh
```

## Next Steps

**Before migrating, review:**
- Full plan: `scripts/REORGANIZATION_PLAN.md`
- Current script usage patterns
- Any custom scripts you've added

**After migrating:**
- Update user crontab immediately
- Test cron jobs manually
- Monitor logs for script-not-found errors
- Update any personal scripts referencing scripts/

## Questions?

See detailed plan in `REORGANIZATION_PLAN.md` for:
- Complete file listing with classifications
- All reference locations that need updating
- Detailed migration commands
- Testing procedures
- Rollback plan
