#!/usr/bin/env bash
# migrate-to-foci.sh — Migrate the clod installation to foci
#
# Run as root. Creates the foci user, copies all data, and performs
# automated find-replace of "clod" references throughout /home/foci.
#
# Does NOT delete the clod user, binaries, or systemd service.
# Those are left intact as a fallback.
#
# Usage:
#   sudo bash tasks/migrate-to-foci.sh [--dry-run]

set -euo pipefail

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
    DRY_RUN=true
    echo "=== DRY RUN — no changes will be made ==="
fi

run() {
    echo "  → $*"
    if [[ "$DRY_RUN" == false ]]; then
        "$@"
    fi
}

section() {
    echo ""
    echo "━━━ $1 ━━━"
}

# ── Preflight checks ──────────────────────────────────────────────

if [[ "$DRY_RUN" == false && "$(id -u)" -ne 0 ]]; then
    echo "ERROR: Must run as root"
    exit 1
fi

if ! id clod &>/dev/null; then
    echo "ERROR: clod user does not exist — nothing to migrate"
    exit 1
fi

if [[ ! -d /home/clod ]]; then
    echo "ERROR: /home/clod does not exist"
    exit 1
fi

# ── Phase 1: Create foci user ─────────────────────────────────────

section "Phase 1: Create foci user"

if id foci &>/dev/null; then
    echo "  foci user already exists, skipping creation"
else
    run useradd --system --home-dir /home/foci --create-home --shell /bin/bash foci
fi

# Add foci to required groups (docker, aisudo, rich-readers, kvm)
for grp in docker aisudo rich-readers kvm; do
    if getent group "$grp" &>/dev/null; then
        run usermod -aG "$grp" foci
        echo "  Added foci to $grp"
    fi
done

# ── Phase 2: Copy all data ────────────────────────────────────────

section "Phase 2: Copy clod data to /home/foci"

if [[ -f /home/foci/config/foci.toml ]] || [[ -f /home/foci/config/clod.toml ]]; then
    echo "  Config already exists in /home/foci — skipping copy (already migrated?)"
    echo "  Ensuring ownership is correct..."
    run chown -R foci:foci /home/foci/
else
    run cp -a /home/clod/. /home/foci/
    run chown -R foci:foci /home/foci/
fi

# ── Phase 3: Rename config file ───────────────────────────────────

section "Phase 3: Rename config file"

if [[ -f /home/foci/config/clod.toml ]]; then
    run mv /home/foci/config/clod.toml /home/foci/config/foci.toml
elif [[ -f /home/foci/config/foci.toml ]]; then
    echo "  foci.toml already exists, skipping"
else
    echo "  WARNING: neither clod.toml nor foci.toml found in /home/foci/config/"
fi

# ── Phase 4: Automated find-replace ───────────────────────────────

PHASE4_START=$(date +%s)
section "Phase 4: Find-replace 'clod' → 'foci' in /home/foci"

echo "  Note: 'clod' is not a real word, so there are no false-positive concerns."
echo "  This replaces all case variants in text files."

# Build list of text files to process (skip bulk dirs, binaries, databases, images, archives)
TEXT_FILES=()
while IFS= read -r -d '' f; do
    # Skip binary/non-text files by extension
    case "$f" in
        *.db|*.db-wal|*.db-shm|*.gz|*.mp3|*.wav|*.png|*.jpg|*.jpeg|*.gif|*.pdf|*.zip|*.tar|*.whl|*.pyc|*.so|*.o|*.a|*.jar|*.class)
            continue ;;
    esac
    TEXT_FILES+=("$f")
done < <(find /home/foci -type f \
    -not -path '*/.git/*' \
    -not -path '*/.cache/*' \
    -not -path '*/.npm/*' \
    -not -path '*/.bun/*' \
    -not -path '*/.cargo/*' \
    -not -path '*/.rustup/*' \
    -not -path '*/.gradle/*' \
    -not -path '*/.android/*' \
    -not -path '*/.claude/*' \
    -not -path '*/.opencode/*' \
    -not -path '*/.config/*' \
    -not -path '*/.pki/*' \
    -not -path '*/.ssh/*' \
    -not -path '*/.zai/*' \
    -not -path '*/node_modules/*' \
    -not -path '*/.local/lib/*' \
    -not -path '*/.local/share/*' \
    -not -path '*/images_received/*' \
    -not -path '*/logs/archive/*' \
    -not -path '*/data/sessions/*' \
    -not -path '*/.claude.json' \
    -not -path '*/.bash_history' \
    -print0 \
    2>/dev/null)

echo "  Found ${#TEXT_FILES[@]} text files to scan"

REPLACED=0
SCANNED=0
TOTAL=${#TEXT_FILES[@]}
for f in "${TEXT_FILES[@]}"; do
    ((SCANNED++)) || true
    if (( SCANNED % 100 == 0 )); then
        echo "  ... scanned $SCANNED/$TOTAL files ($REPLACED replaced so far)"
    fi
    if grep -ql 'clod\|Clod\|CLOD' "$f" 2>/dev/null; then
        echo "  Replacing in: ${f#/home/foci/}"
        if [[ "$DRY_RUN" == true ]]; then
            grep -n 'clod\|Clod\|CLOD' "$f" | head -5
        else
            # Perform replacements: paths, config refs, binary names, env vars
            sed -i \
                -e 's|/home/clod|/home/foci|g' \
                -e 's|clod\.toml|foci.toml|g' \
                -e 's|clod\.log|foci.log|g' \
                -e 's|clod\.service|foci.service|g' \
                -e 's|clod-secrets|foci-secrets|g' \
                -e 's|clod-tool-results|foci-tool-results|g' \
                -e 's|clod-tts-|foci-tts-|g' \
                -e 's|clod-commit|foci-commit|g' \
                -e 's|clodgw|focigw|g' \
                -e 's|/usr/local/bin/clod |/usr/local/bin/foci |g' \
                -e 's|/usr/local/bin/clod$|/usr/local/bin/foci|g' \
                -e 's|CLOD_USER|FOCI_USER|g' \
                -e 's|CLOD_HOME|FOCI_HOME|g' \
                -e 's|CLOD_ADDR|FOCI_ADDR|g' \
                -e 's|CLOD_AGENT|FOCI_AGENT|g' \
                -e 's|CLOD_ANTHROPIC_TOKEN|FOCI_ANTHROPIC_TOKEN|g' \
                -e 's|CLOD_TELEGRAM_TOKEN|FOCI_TELEGRAM_TOKEN|g' \
                -e 's|CLOD_|FOCI_|g' \
                -e 's|User=clod|User=foci|g' \
                -e 's|Group=clod|Group=foci|g' \
                -e 's|\bclod send\b|foci send|g' \
                -e 's|\bclod branch\b|foci branch|g' \
                -e 's|\bclod status\b|foci status|g' \
                -e 's|\bclod eval\b|foci eval|g' \
                -e 's|\bclod command\b|foci command|g' \
                -e 's|\bclod ping\b|foci ping|g' \
                -e 's|clod|foci|g' \
                -e 's|Clod|Foci|g' \
                -e 's|CLOD|FOCI|g' \
                "$f"
        fi
        ((REPLACED++)) || true
    fi
done

PHASE4_END=$(date +%s)
echo "  Updated $REPLACED files out of $TOTAL scanned in $((PHASE4_END - PHASE4_START))s"

# Change HTTP port so foci can run in parallel with clod
if [[ -f /home/foci/config/foci.toml ]]; then
    if grep -q 'port = 18791' /home/foci/config/foci.toml 2>/dev/null; then
        if [[ "$DRY_RUN" == false ]]; then
            sed -i 's|port = 18791|port = 18792|g' /home/foci/config/foci.toml
        fi
        echo "  Changed HTTP port from 18791 → 18792 (parallel with clod)"
    fi
fi

# ── Phase 4b: Set up rich-readers access ──────────────────────────

section "Phase 4b: rich-readers group access"

if getent group rich-readers &>/dev/null; then
    # Match the setgid setup from /home/clod
    run chgrp -R rich-readers /home/foci
    run find /home/foci -type d -exec chmod g+rxs {} +
    run find /home/foci -type f -exec chmod g+r {} +
    echo "  Set rich-readers group with setgid on /home/foci"
else
    echo "  WARNING: rich-readers group not found"
fi

# ── Phase 5: Security group ───────────────────────────────────────

section "Phase 5: Security group"

if getent group foci-secrets &>/dev/null; then
    echo "  foci-secrets group already exists"
else
    run groupadd foci-secrets
fi

# Add foci user to the group
run usermod -aG foci-secrets foci

# Update secrets file permissions
if [[ -f /home/foci/config/secrets.toml ]]; then
    run chgrp foci-secrets /home/foci/config/secrets.toml
    run chmod 640 /home/foci/config/secrets.toml
    run chown root:foci-secrets /home/foci/config/secrets.toml
fi

# ── Phase 6: Install new binaries ─────────────────────────────────

section "Phase 6: Install binaries"

for bin in focigw foci; do
    if [[ -f "/usr/local/bin/$bin" ]]; then
        echo "  $bin already installed"
    else
        echo "  WARNING: /usr/local/bin/$bin not found."
        echo "  Build and install with:"
        echo "    make all && sudo cp focigw foci /usr/local/bin/"
    fi
done

# ── Phase 7: Systemd service ──────────────────────────────────────

section "Phase 7: Systemd service"

if [[ -f /etc/systemd/system/foci.service ]]; then
    echo "  foci.service already exists"
else
    if [[ -f /etc/systemd/system/clod.service ]]; then
        run cp /etc/systemd/system/clod.service /etc/systemd/system/foci.service
        if [[ "$DRY_RUN" == false ]]; then
            sed -i \
                -e 's|clodgw|focigw|g' \
                -e 's|/home/clod/|/home/foci/|g' \
                -e 's|clod\.toml|foci.toml|g' \
                -e 's|User=clod|User=foci|g' \
                -e 's|Group=clod|Group=foci|g' \
                -e 's|clod\.service|foci.service|g' \
                /etc/systemd/system/foci.service
        fi
        run systemctl daemon-reload
        run systemctl enable foci.service
        echo "  Created foci.service (clod.service left intact)"
    else
        echo "  WARNING: clod.service not found, cannot create foci.service"
    fi
fi

# ── Phase 8: Crontab ──────────────────────────────────────────────

section "Phase 8: Crontab migration"

if crontab -u clod -l &>/dev/null 2>&1; then
    echo "  Migrating clod crontab to foci user:"
    if [[ "$DRY_RUN" == true ]]; then
        crontab -u clod -l | sed \
            -e 's|/usr/local/bin/clod |/usr/local/bin/foci |g' \
            -e 's|/home/clod/|/home/foci/|g'
        echo "  (would install above as foci crontab)"
    else
        crontab -u clod -l | sed \
            -e 's|/usr/local/bin/clod |/usr/local/bin/foci |g' \
            -e 's|/home/clod/|/home/foci/|g' \
            | crontab -u foci -
        echo "  Done. clod crontab left intact."
    fi
else
    echo "  No crontab found for clod user"
fi

# ── Phase 9: Polkit rules ─────────────────────────────────────────

section "Phase 9: Polkit rules"

if [[ -f /etc/polkit-1/rules.d/49-clod.rules ]]; then
    run cp /etc/polkit-1/rules.d/49-clod.rules /etc/polkit-1/rules.d/49-foci.rules
    if [[ "$DRY_RUN" == false ]]; then
        sed -i 's/clod/foci/g' /etc/polkit-1/rules.d/49-foci.rules
    fi
    echo "  Created 49-foci.rules (49-clod.rules left intact)"
elif [[ -f /etc/polkit-1/rules.d/49-foci.rules ]]; then
    echo "  49-foci.rules already exists"
else
    echo "  No polkit rules found (OK if not using polkit)"
fi

# ── Summary ────────────────────────────────────────────────────────

section "Migration complete"

echo ""
echo "What was done:"
echo "  ✓ Created foci user"
echo "  ✓ Copied /home/clod → /home/foci"
echo "  ✓ Renamed config: clod.toml → foci.toml"
echo "  ✓ Find-replaced 'clod' → 'foci' in all text files under /home/foci"
echo "  ✓ Created foci-secrets group and set permissions"
echo "  ✓ Created foci.service systemd unit"
echo "  ✓ Migrated crontab"
echo "  ✓ Copied polkit rules"
echo ""
echo "What was NOT touched (left as fallback):"
echo "  • clod user and /home/clod — intact"
echo "  • clod.service — still enabled and present"
echo "  • /usr/local/bin/clod and clodgw — still present"
echo "  • clod crontab — still active"
echo ""
echo "Next steps (MANUAL):"
echo "  1. Review /home/foci/ contents — spot-check the find-replace"
echo "  2. Build and install new binaries:"
echo "     cd /home/rich/git/clod && make all"
echo "     sudo cp focigw /usr/local/bin/focigw"
echo "     sudo cp foci /usr/local/bin/foci"
echo "  3. Rename GitHub repo: richardtkemp/clod → richardtkemp/foci"
echo "  4. Update git remote: git remote set-url origin https://github.com/richardtkemp/foci.git"
echo "  5. When ready to switch:"
echo "     sudo systemctl stop clod.service"
echo "     sudo systemctl start foci.service"
echo "     sudo journalctl -u foci.service -f"
echo "     foci ping"
echo ""
echo "foci HTTP port: 18792 (clod stays on 18791 — can run in parallel)"
echo ""
echo "To roll back: sudo systemctl stop foci.service && sudo systemctl start clod.service"
