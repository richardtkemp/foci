# Plan: Rename clod → foci

## Status: Phases 1–2 DONE. Phase 3 manual. Phase 4 scripted.

---

## Phase 1: Code rename ✅

Single atomic commit. All Go source, imports, binary names, config defaults, system identity, tests updated.

## Phase 2: Documentation rename ✅

All docs, scripts, setup.sh, config example, task files updated.

## Phase 3: GitHub repo rename (manual)

1. Rename on GitHub: `richardtkemp/clod` → `richardtkemp/foci`
2. GitHub auto-redirects old URL
3. `git remote set-url origin https://github.com/richardtkemp/foci.git`

## Phase 4: Production migration

**Script:** `tasks/migrate-to-foci.sh`

Run as root: `sudo bash tasks/migrate-to-foci.sh` (supports `--dry-run`)

### What the script does

1. **Creates foci user** — `useradd --system`
2. **Copies all clod data** — `cp -a /home/clod/. /home/foci/` with `chown -R foci:foci`
3. **Renames config** — `clod.toml` → `foci.toml`
4. **Automated find-replace** — scans all text files under `/home/foci/` and replaces all `clod` references:
   - Paths: `/home/clod` → `/home/foci`
   - Config: `clod.toml` → `foci.toml`, `clod.log` → `foci.log`
   - Services: `clod.service` → `foci.service`, `clodgw` → `focigw`
   - Security: `clod-secrets` → `foci-secrets`
   - Env vars: `CLOD_*` → `FOCI_*`
   - CLI: `clod send` → `foci send`, etc.
   - Skips binary files (`.db`, `.gz`, `.mp3`, images, etc.)
   - "clod" is not a real word — no false positive concerns
5. **Security group** — creates `foci-secrets`, sets permissions on `secrets.toml`
6. **Systemd** — copies `clod.service` → `foci.service` with sed replacements
7. **Crontab** — migrates clod user crontab to foci user
8. **Polkit** — copies rules with rename

### What is NOT touched (fallback preserved)

- clod user and `/home/clod` — left intact
- `clod.service` — left installed (can be started to rollback)
- `/usr/local/bin/clod` and `clodgw` — left in place
- clod crontab — still exists

### Post-migration steps

```bash
# Build and install new binaries
cd /home/rich/git/clod && make all
sudo cp focigw /usr/local/bin/focigw
sudo cp foci /usr/local/bin/foci

# Swap services
sudo systemctl stop clod.service
sudo systemctl start foci.service

# Verify
sudo journalctl -u foci.service -f
foci ping
```

### Rollback

```bash
sudo systemctl stop foci.service
sudo systemctl start clod.service
```

---

## Things that should NOT be renamed

- **Telegram bot usernames** — registered with BotFather, independent of project name
- **Agent IDs** (`fotini`, `clutch`, etc.) — user-chosen identities
- **Session key format** (`agent:ID:TYPE:BRANCH`) — no "clod" in the format
