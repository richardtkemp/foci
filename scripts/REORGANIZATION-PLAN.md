# Scripts Directory Reorganization Plan

## Current Status
31 files in flat scripts/ directory - mix of active tools, analysis scripts, monitoring, and temporary experiments.

## Proposed Structure

```
scripts/
├── coding-agents/          # Coding agent automation
│   ├── claude-tmux-watcher.sh
│   ├── opencode-tmux-watcher.sh
│   ├── coding-agent-tmux-watcher.sh (deprecated wrapper)
│   ├── launch-coding-agent-tmux.sh
│   ├── launch-claude-tmux.sh (deprecated - use launch-coding-agent-tmux.sh)
│   ├── track_claude_usage.sh
│   └── CODING_AGENT_STATES.md
│
├── monitoring/             # System & service monitoring
│   ├── monitor-coolify-health.sh
│   ├── coolify-monitor-exclusions.txt
│   ├── container-uptime-report.py
│   ├── container-uptime-docker-stats.py
│   ├── container-monitor-exclusions.txt
│   ├── netdata-notify.sh
│   ├── netdata-notify.mjs
│   └── cron-health-probe.sh
│
├── token-analysis/         # Token usage tracking & optimization
│   ├── token-usage-tracker.py
│   ├── token-usage-grapher.py
│   ├── analyze-session-tokens.py
│   ├── token-budget-analyzer.py
│   ├── compaction-matrix-analysis.py
│   ├── compaction-threshold-analysis.py
│   └── model-cost-optimization.py
│
├── maintenance/            # Regular maintenance tasks
│   ├── cleanup-idle-tmux.sh
│   ├── gcalcli-weekly-reauth.sh
│   ├── check-memory-size.sh
│   ├── check-openclaw-branch.sh
│   ├── morning-routine.sh
│   └── get_affirmation.sh
│
├── skills/                 # Skill-related utilities
│   ├── install-skill.sh
│   └── moltbook-spam-filter.sh
│
└── archive/                # Old/deprecated scripts (keep for reference)
    └── test-cron-list.sh
```

---

## Script Classification

### 🟢 KEEP - Active Production Scripts

#### Coding Agents (7 files)
- ✅ **claude-tmux-watcher.sh** - Active, comprehensive state detection (just updated today)
- ✅ **opencode-tmux-watcher.sh** - Active, comprehensive state detection (just updated today)
- ⚠️ **coding-agent-tmux-watcher.sh** - DEPRECATED wrapper, superseded by agent-specific watchers
- ✅ **launch-coding-agent-tmux.sh** - Active launcher (updated today)
- ⚠️ **launch-claude-tmux.sh** - DEPRECATED, use launch-coding-agent-tmux.sh instead
- ✅ **track_claude_usage.sh** - Active usage tracker with threshold alerts
- ✅ **CODING_AGENT_STATES.md** - Documentation (just created today)

**References to update:**
- `skills/claude-or-opencode/SKILL.md` - references launch-coding-agent-tmux.sh
- `TOOLS.md` - may reference coding agent scripts

#### Monitoring (8 files)
- ✅ **monitor-coolify-health.sh** - Active cron job
  - Referenced in: `cron/jobs.json` ("coolify-health-monitor" job)
- ✅ **coolify-monitor-exclusions.txt** - Config for above
- ✅ **container-uptime-report.py** - Container health reporting
- ✅ **container-uptime-docker-stats.py** - Docker stats wrapper
- ✅ **container-monitor-exclusions.txt** - Config for above
- ✅ **netdata-notify.sh** - Netdata alert bridge (may be replaced by .mjs version)
- ✅ **netdata-notify.mjs** - Node.js version of Netdata bridge
- ✅ **cron-health-probe.sh** - Diagnostics tool for cron system

**References to update:**
- `cron/jobs.json` - monitor-coolify-health.sh path
- Netdata config (if it references script path)
- Any monitoring documentation

#### Token Analysis (7 files)
- ✅ **token-usage-tracker.py** - Active, runs daily
  - Referenced in: `HEARTBEAT.md` morning routine
  - Referenced in: `cron/jobs.json` (if scheduled)
- ✅ **token-usage-grapher.py** - Active, generates daily graphs
  - Referenced in: `HEARTBEAT.md` morning routine
- ✅ **analyze-session-tokens.py** - Analysis tool (Feb 11)
- ✅ **token-budget-analyzer.py** - Analysis tool (Feb 13)
- ✅ **compaction-matrix-analysis.py** - Optimization analysis (today)
- ✅ **compaction-threshold-analysis.py** - Optimization analysis (today)
- ✅ **model-cost-optimization.py** - Cost analysis (yesterday)

**Status:** All are analysis/optimization tools. Some are one-time research, some are ongoing.
**Keep:** tracker.py and grapher.py (daily use). Others are valuable research tools - keep in token-analysis/.

**References to update:**
- `HEARTBEAT.md` - update morning routine paths
- Any cron jobs that run token tracking

#### Maintenance (6 files)
- ✅ **cleanup-idle-tmux.sh** - Active cron job
  - Referenced in: `cron/jobs.json` ("cleanup-idle-tmux" job)
- ✅ **gcalcli-weekly-reauth.sh** - Weekly OAuth refresh
- ✅ **check-memory-size.sh** - Monitors MEMORY.md size (user crontab)
- ✅ **check-openclaw-branch.sh** - Branch safety check
- ✅ **morning-routine.sh** - Morning summary (Feb 12)
- ✅ **get_affirmation.sh** - Random affirmation picker

**References to update:**
- `cron/jobs.json` - cleanup-idle-tmux.sh path
- User crontab (check-memory-size.sh)
- `HEARTBEAT.md` - may reference morning-routine.sh

#### Skills (2 files)
- ✅ **install-skill.sh** - Quarantine installer with SkillBouncer
  - Referenced in: `AGENTS.md` ("Installing Skills" section)
- ✅ **moltbook-spam-filter.sh** - Spam filtering for Moltbook
  - Referenced in: `skills/moltbook/SKILL.md`

**References to update:**
- `AGENTS.md` - update install-skill.sh path
- `skills/moltbook/SKILL.md` - update spam filter path

---

### 🟡 ARCHIVE - Old/Temporary Scripts

- ⚠️ **test-cron-list.sh** - Diagnostic tool from Feb 8 cron debugging
  - Purpose: Test cron.list reliability every 3 minutes
  - Status: One-time debugging, not needed anymore
  - Action: Move to archive/

---

## Migration Impact Analysis

### Files That Reference Script Paths

#### 1. Cron Jobs (`/home/rich/git/openclaw/config/cron/jobs.json`)
**Current references:**
```json
"coolify-health-monitor": "bash /home/rich/git/openclaw/workspace/scripts/monitor-coolify-health.sh"
"cleanup-idle-tmux": "bash /home/rich/git/openclaw/workspace/scripts/cleanup-idle-tmux.sh"
```

**After migration:**
```json
"coolify-health-monitor": "bash /home/rich/git/openclaw/workspace/scripts/monitoring/monitor-coolify-health.sh"
"cleanup-idle-tmux": "bash /home/rich/git/openclaw/workspace/scripts/maintenance/cleanup-idle-tmux.sh"
```

#### 2. HEARTBEAT.md Morning Routine
**Current references:**
```bash
python3 ~/git/openclaw/workspace/scripts/token-usage-tracker.py
python3 ~/git/openclaw/workspace/scripts/token-usage-grapher.py
```

**After migration:**
```bash
python3 ~/git/openclaw/workspace/scripts/token-analysis/token-usage-tracker.py
python3 ~/git/openclaw/workspace/scripts/token-analysis/token-usage-grapher.py
```

#### 3. AGENTS.md Installing Skills Section
**Current reference:**
```bash
bash $OPENCLAW_WORKSPACE/scripts/install-skill.sh <skill-name>
```

**After migration:**
```bash
bash $OPENCLAW_WORKSPACE/scripts/skills/install-skill.sh <skill-name>
```

#### 4. skills/claude-or-opencode/SKILL.md
**Current reference:**
```bash
bash ~/git/openclaw/workspace/scripts/launch-coding-agent-tmux.sh -a claude ...
```

**After migration:**
```bash
bash ~/git/openclaw/workspace/scripts/coding-agents/launch-coding-agent-tmux.sh -a claude ...
```

#### 5. skills/moltbook/SKILL.md
**Current reference:**
```bash
bash ~/git/openclaw/workspace/scripts/moltbook-spam-filter.sh
```

**After migration:**
```bash
bash ~/git/openclaw/workspace/scripts/skills/moltbook-spam-filter.sh
```

#### 6. TOOLS.md
**Potential references:**
- Coding agent watcher scripts
- Installation commands

**Action:** Search and update after migration.

#### 7. User Crontab
**Potential references:**
- `check-memory-size.sh` (mentioned in script header as "intended for user crontab")

**Action:** Check user crontab and update if present.

#### 8. Netdata Configuration
**Potential references:**
- `netdata-notify.sh` or `netdata-notify.mjs`

**Action:** Check Netdata config files for script paths.

#### 9. Internal Script References
Some scripts call other scripts:
- `morning-routine.sh` calls token-usage scripts
- `launch-coding-agent-tmux.sh` calls watchers

**Action:** Update internal paths in scripts after migration.

---

## Migration Steps (Detailed)

### Phase 1: Preparation
1. ✅ Create this plan document
2. Create backup branch: `git checkout -b scripts-reorg-backup`
3. Create subdirectories:
   ```bash
   cd /home/rich/git/openclaw/workspace/scripts
   mkdir -p coding-agents monitoring token-analysis maintenance skills archive
   ```

### Phase 2: Move Files
Use `git mv` to preserve history:
```bash
# Coding agents
git mv claude-tmux-watcher.sh coding-agents/
git mv opencode-tmux-watcher.sh coding-agents/
git mv coding-agent-tmux-watcher.sh coding-agents/
git mv launch-coding-agent-tmux.sh coding-agents/
git mv launch-claude-tmux.sh coding-agents/
git mv track_claude_usage.sh coding-agents/
git mv CODING_AGENT_STATES.md coding-agents/

# Monitoring
git mv monitor-coolify-health.sh monitoring/
git mv coolify-monitor-exclusions.txt monitoring/
git mv container-uptime-report.py monitoring/
git mv container-uptime-docker-stats.py monitoring/
git mv container-monitor-exclusions.txt monitoring/
git mv netdata-notify.sh monitoring/
git mv netdata-notify.mjs monitoring/
git mv cron-health-probe.sh monitoring/

# Token analysis
git mv token-usage-tracker.py token-analysis/
git mv token-usage-grapher.py token-analysis/
git mv analyze-session-tokens.py token-analysis/
git mv token-budget-analyzer.py token-analysis/
git mv compaction-matrix-analysis.py token-analysis/
git mv compaction-threshold-analysis.py token-analysis/
git mv model-cost-optimization.py token-analysis/

# Maintenance
git mv cleanup-idle-tmux.sh maintenance/
git mv gcalcli-weekly-reauth.sh maintenance/
git mv check-memory-size.sh maintenance/
git mv check-openclaw-branch.sh maintenance/
git mv morning-routine.sh maintenance/
git mv get_affirmation.sh maintenance/

# Skills
git mv install-skill.sh skills/
git mv moltbook-spam-filter.sh skills/

# Archive
git mv test-cron-list.sh archive/
```

### Phase 3: Update References

#### 3.1 Update cron jobs
```bash
# Edit /home/rich/git/openclaw/config/cron/jobs.json
# Find and replace:
#   scripts/monitor-coolify-health.sh → scripts/monitoring/monitor-coolify-health.sh
#   scripts/cleanup-idle-tmux.sh → scripts/maintenance/cleanup-idle-tmux.sh
```

#### 3.2 Update HEARTBEAT.md
```bash
# Find and replace:
#   scripts/token-usage-tracker.py → scripts/token-analysis/token-usage-tracker.py
#   scripts/token-usage-grapher.py → scripts/token-analysis/token-usage-grapher.py
```

#### 3.3 Update AGENTS.md
```bash
# Find and replace:
#   scripts/install-skill.sh → scripts/skills/install-skill.sh
```

#### 3.4 Update TOOLS.md
```bash
# Search for any script references and update paths
grep -n "scripts/" TOOLS.md
```

#### 3.5 Update skills
```bash
# Update claude-or-opencode/SKILL.md
sed -i 's|scripts/launch-coding-agent-tmux.sh|scripts/coding-agents/launch-coding-agent-tmux.sh|g' \
  skills/claude-or-opencode/SKILL.md

# Update moltbook/SKILL.md
sed -i 's|scripts/moltbook-spam-filter.sh|scripts/skills/moltbook-spam-filter.sh|g' \
  skills/moltbook/SKILL.md
```

#### 3.6 Update internal script references
```bash
# Update morning-routine.sh to call token scripts from new location
sed -i 's|scripts/token-usage|scripts/token-analysis/token-usage|g' \
  scripts/maintenance/morning-routine.sh

# Update launch-coding-agent-tmux.sh to call watchers from new location
sed -i 's|scripts/.*-tmux-watcher.sh|scripts/coding-agents/&|g' \
  scripts/coding-agents/launch-coding-agent-tmux.sh
```

#### 3.7 Update user crontab (REQUIRED)
**Current crontab entries:**
```cron
0 20 * * 0 /home/rich/git/openclaw/workspace/scripts/gcalcli-weekly-reauth.sh
0 */6 * * * /home/rich/git/openclaw/workspace/scripts/check-openclaw-branch.sh
*/10 * * * * /home/rich/git/openclaw/workspace/scripts/track_claude_usage.sh
0 8 * * * /home/rich/git/openclaw/workspace/scripts/check-memory-size.sh
```

**Update with:**
```bash
crontab -e
# Change:
#   scripts/gcalcli-weekly-reauth.sh → scripts/maintenance/gcalcli-weekly-reauth.sh
#   scripts/check-openclaw-branch.sh → scripts/maintenance/check-openclaw-branch.sh
#   scripts/track_claude_usage.sh → scripts/coding-agents/track_claude_usage.sh
#   scripts/check-memory-size.sh → scripts/maintenance/check-memory-size.sh
```

**Or use sed to update:**
```bash
crontab -l > /tmp/crontab.bak
sed -i 's|scripts/gcalcli-weekly-reauth.sh|scripts/maintenance/gcalcli-weekly-reauth.sh|g' /tmp/crontab.bak
sed -i 's|scripts/check-openclaw-branch.sh|scripts/maintenance/check-openclaw-branch.sh|g' /tmp/crontab.bak
sed -i 's|scripts/track_claude_usage.sh|scripts/coding-agents/track_claude_usage.sh|g' /tmp/crontab.bak
sed -i 's|scripts/check-memory-size.sh|scripts/maintenance/check-memory-size.sh|g' /tmp/crontab.bak
crontab /tmp/crontab.bak
crontab -l  # Verify
```

#### 3.8 Check Netdata config
```bash
# Find Netdata config references to notify scripts
grep -r "netdata-notify" /etc/netdata/ /opt/netdata/ 2>/dev/null || echo "Check Netdata config manually"
```

### Phase 4: Create README files
Create README.md in each subdirectory explaining purpose:
```bash
# scripts/coding-agents/README.md
# scripts/monitoring/README.md
# scripts/token-analysis/README.md
# scripts/maintenance/README.md
# scripts/skills/README.md
# scripts/archive/README.md
```

### Phase 5: Testing
1. Test cron jobs still work:
   ```bash
   # Manually trigger each cron job
   bash scripts/monitoring/monitor-coolify-health.sh
   bash scripts/maintenance/cleanup-idle-tmux.sh
   ```

2. Test token tracking:
   ```bash
   python3 scripts/token-analysis/token-usage-tracker.py
   python3 scripts/token-analysis/token-usage-grapher.py
   ```

3. Test coding agent launch:
   ```bash
   # Dry run
   bash scripts/coding-agents/launch-coding-agent-tmux.sh -a claude test-session /tmp "echo test"
   ```

4. Test skill install:
   ```bash
   # Dry run with fake skill
   bash scripts/skills/install-skill.sh --help
   ```

### Phase 6: Commit & Push
```bash
git add -A
git commit -m "refactor(scripts): reorganize into subdirectories by purpose

Moved 31 scripts from flat directory into organized subdirectories:
- coding-agents/ (7 files) - coding agent automation
- monitoring/ (8 files) - system & service monitoring
- token-analysis/ (7 files) - token tracking & optimization
- maintenance/ (6 files) - regular maintenance tasks
- skills/ (2 files) - skill utilities
- archive/ (1 file) - deprecated/one-time scripts

Updated all references in:
- cron/jobs.json
- HEARTBEAT.md
- AGENTS.md
- TOOLS.md
- skills/*/SKILL.md
- Internal script paths

Preserved git history with 'git mv'."

git push origin main
```

---

## Post-Migration Cleanup

### Add Deprecation Notices
For deprecated scripts that should eventually be removed:
```bash
# Add header to coding-agents/coding-agent-tmux-watcher.sh
echo "# DEPRECATED: Use agent-specific watchers instead (claude-tmux-watcher.sh or opencode-tmux-watcher.sh)"

# Add header to coding-agents/launch-claude-tmux.sh
echo "# DEPRECATED: Use launch-coding-agent-tmux.sh -a claude instead"
```

### Update Main Scripts README
Create `scripts/README.md`:
```markdown
# OpenClaw Scripts

Organized utilities and automation scripts for OpenClaw workspace.

## Directory Structure

- `coding-agents/` - Coding agent (Claude Code, OpenCode) automation
- `monitoring/` - System and service health monitoring
- `token-analysis/` - Token usage tracking and optimization
- `maintenance/` - Regular maintenance and cleanup tasks
- `skills/` - Skill installation and utilities
- `archive/` - Deprecated or one-time diagnostic scripts

## Quick Reference

### Daily Tasks
- Token tracking: `token-analysis/token-usage-tracker.py`
- Morning routine: `maintenance/morning-routine.sh`

### Monitoring
- Coolify health: `monitoring/monitor-coolify-health.sh`
- Container uptime: `monitoring/container-uptime-report.py`

### Coding Agents
- Launch agent: `coding-agents/launch-coding-agent-tmux.sh -a {claude|opencode}`
- Watch agent: `coding-agents/{claude|opencode}-tmux-watcher.sh`

### Skills
- Install skill: `skills/install-skill.sh <skill-name>`
```

---

## Risks & Mitigation

### Risk 1: Broken Cron Jobs
**Impact:** High - monitoring stops working
**Mitigation:** 
- Test each cron job manually after migration
- Monitor for failed cron job notifications (24 hours)
- Keep backup branch for quick rollback

### Risk 2: Broken Skills
**Impact:** Medium - skill features stop working
**Mitigation:**
- Test skill commands after migration
- Update skill documentation with new paths
- Verify with actual skill execution

### Risk 3: Missed References
**Impact:** Medium - some script calls fail
**Mitigation:**
- Grep entire workspace for "scripts/" references before migration
- Test morning routine and other automated flows
- Monitor logs for script not found errors

### Risk 4: User Crontab Issues
**Impact:** Low - personal cron jobs fail
**Mitigation:**
- Check user crontab before migration
- Update user crontab if needed
- Document in commit message

---

## Rollback Plan

If issues arise:
```bash
# Quick rollback
git reset --hard scripts-reorg-backup
git push -f origin main  # (if already pushed)

# Partial rollback (keep some changes)
git checkout scripts-reorg-backup -- scripts/
git checkout main -- scripts/coding-agents/  # Keep specific subdirs
```

---

## Timeline Estimate

- **Preparation:** 10 minutes
- **File moves:** 5 minutes
- **Update references:** 20 minutes
- **Create READMEs:** 15 minutes
- **Testing:** 20 minutes
- **Commit & push:** 5 minutes

**Total:** ~75 minutes (1.25 hours)

---

## Success Criteria

✅ All scripts moved to appropriate subdirectories
✅ All cron jobs still execute successfully
✅ Token tracking daily routine works
✅ Coding agent launch works from skills
✅ Skill installer works from AGENTS.md
✅ No broken references in logs (24h monitoring)
✅ README files created for each subdirectory
✅ Main scripts/README.md created
✅ Git history preserved (git mv used)

---

## Notes

- **Preserve git history:** Always use `git mv` instead of `mv`
- **Test before committing:** Run manual tests for critical scripts
- **Document in commit:** List all updated reference locations
- **Monitor after deployment:** Watch for script-not-found errors in logs
- **Cleanup deprecated scripts later:** Mark deprecated scripts now, remove after 1 month if unused

---

## Questions Answered Before Migration

1. ✅ **User crontab entries?** → YES - 4 scripts referenced:
   - `gcalcli-weekly-reauth.sh` (weekly at 20:00)
   - `check-openclaw-branch.sh` (every 6 hours)
   - `track_claude_usage.sh` (every 10 minutes)
   - `check-memory-size.sh` (daily at 08:00)
   - **ACTION REQUIRED:** Update user crontab after migration

2. ✅ **Netdata config references?** → NO
   - No config files found referencing netdata-notify scripts
   - May be configured via environment or webhook URL
   - **ACTION:** Safe to move, verify Netdata still works after

3. ✅ **Symlinks to scripts/?** → NO
   - No symlinks found pointing to scripts/
   - **ACTION:** Safe to proceed

4. ✅ **Docker containers mounting scripts/?** → NO
   - No docker-compose files found mounting scripts/
   - **ACTION:** Safe to proceed

5. ✅ **Systemd services referencing scripts/?** → NO
   - No systemd service files reference openclaw scripts
   - **ACTION:** Safe to proceed

**Migration Safety:** Medium risk - user crontab needs manual update after migration.
