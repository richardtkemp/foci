#!/bin/bash
# Quick update: build as foci user, install binary, restart services.
# Run as root.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="/usr/local/bin"

if [[ $EUID -ne 0 ]]; then
    echo "Run as root." >&2
    exit 1
fi

NEW_COMMIT="$(git -C "$SCRIPT_DIR" -c safe.directory="$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"

echo "Building..."
sudo -u foci bash -c "cd '$SCRIPT_DIR' && make all"

echo "Installing..."
install -m 755 "$SCRIPT_DIR/bin/foci-gw" "$INSTALL_DIR/foci-gw"
install -m 755 "$SCRIPT_DIR/bin/foci" "$INSTALL_DIR/foci"
install -m 755 "$SCRIPT_DIR/bin/foci-call" "$INSTALL_DIR/foci-call"
install -m 755 "$SCRIPT_DIR/bin/foci-cc-hook" "$INSTALL_DIR/foci-cc-hook"

echo "Migrating service files..."
SECRETS_GROUP="foci-secrets"
for svcfile in /etc/systemd/system/foci*.service; do
    [[ -f "$svcfile" ]] || continue

    # Binary rename (focigw → foci-gw)
    if grep -q "focigw" "$svcfile"; then
        echo "  Patching $svcfile: focigw → foci-gw"
        sed -i "s|${INSTALL_DIR}/focigw|${INSTALL_DIR}/foci-gw|g" "$svcfile"
    fi

    # Secrets-boundary hardening (P0-1). Ensure the secrets group is granted to
    # the process (so removing the /etc/group membership below is safe), plus the
    # ambient-capability and no-new-privs hardening. Idempotent — only inserts
    # directives that are missing.
    if ! grep -q "^SupplementaryGroups=" "$svcfile"; then
        echo "  Patching $svcfile: add SupplementaryGroups=$SECRETS_GROUP"
        sed -i "/^User=/a SupplementaryGroups=$SECRETS_GROUP" "$svcfile"
    fi
    if ! grep -q "^AmbientCapabilities=" "$svcfile"; then
        echo "  Patching $svcfile: add AmbientCapabilities=CAP_SETGID"
        sed -i "/^SupplementaryGroups=/a AmbientCapabilities=CAP_SETGID" "$svcfile"
    fi
    if ! grep -q "^CapabilityBoundingSet=" "$svcfile"; then
        echo "  Patching $svcfile: add CapabilityBoundingSet=CAP_SETGID"
        sed -i "/^AmbientCapabilities=/a CapabilityBoundingSet=CAP_SETGID" "$svcfile"
    fi
    if ! grep -q "^NoNewPrivileges=" "$svcfile"; then
        echo "  Patching $svcfile: add NoNewPrivileges=yes"
        sed -i "/^AmbientCapabilities=/a NoNewPrivileges=yes" "$svcfile"
    fi

    # Remove the service user's permanent foci-secrets /etc/group membership — it
    # is re-acquirable via setuid sg/newgrp (P0-1). The group is now granted to
    # the process only (SupplementaryGroups, above), so the gw keeps read+write
    # while children cannot regain it.
    SVC_USER="$(grep '^User=' "$svcfile" | cut -d= -f2 || true)"
    if [[ -n "$SVC_USER" ]] && id -nG "$SVC_USER" 2>/dev/null | grep -qw "$SECRETS_GROUP"; then
        echo "  Removing $SVC_USER from $SECRETS_GROUP (now granted per-process)"
        gpasswd -d "$SVC_USER" "$SECRETS_GROUP" >/dev/null
    fi
done
systemctl daemon-reload

# Stage changelog per service
echo "Staging changelogs..."
for svcfile in /etc/systemd/system/foci*.service; do
    [[ -f "$svcfile" ]] || continue
    SVC_HOME="$(grep '^WorkingDirectory=' "$svcfile" | cut -d= -f2)" || continue
    SVC_USER="$(grep '^User=' "$svcfile" | cut -d= -f2)" || continue
    [[ -n "$SVC_HOME" ]] || continue

    COMMIT_FILE="$SVC_HOME/data/.foci-commit"
    OLD_COMMIT=""
    if [[ -f "$COMMIT_FILE" ]] && [[ -r "$COMMIT_FILE" ]]; then
        OLD_COMMIT="$(cat "$COMMIT_FILE" 2>/dev/null || true)"
    fi

    if [[ -n "$OLD_COMMIT" ]] && [[ "$OLD_COMMIT" != "$NEW_COMMIT" ]]; then
        {
            echo "# Foci Updated"
            echo ""
            echo "Updated from \`$OLD_COMMIT\` to \`$NEW_COMMIT\` on $(date -u '+%Y-%m-%d %H:%M UTC')."
            echo ""
            echo "## Changes"
            echo ""
            git -C "$SCRIPT_DIR" -c safe.directory="$SCRIPT_DIR" log --format='- **%s**%n%n%w(0,2,2)%b' "$OLD_COMMIT..$NEW_COMMIT" 2>/dev/null || echo "(could not read git log)"
            echo ""
            echo "## Instructions"
            echo ""
            echo "Tell your user what just changed. Summarise the updates above in a brief, friendly message — highlight the most impactful changes and anything they'll notice."
        } > "$SVC_HOME/data/WELCOME.md"
        chown "$SVC_USER:$SVC_USER" "$SVC_HOME/data/WELCOME.md"
        echo "  $(basename "$svcfile" .service): changelog staged ($OLD_COMMIT → $NEW_COMMIT)"
    else
        echo "  $(basename "$svcfile" .service): no changelog (same commit or no previous record)"
    fi

    mkdir -p "$(dirname "$COMMIT_FILE")"
    echo "$NEW_COMMIT" > "$COMMIT_FILE"
    chown "$SVC_USER:$SVC_USER" "$COMMIT_FILE"
done

# Copy platform docs to each service's shared directory for agent access
echo "Copying platform docs..."
for svcfile in /etc/systemd/system/foci*.service; do
    [[ -f "$svcfile" ]] || continue
    SVC_HOME="$(grep '^WorkingDirectory=' "$svcfile" | cut -d= -f2)" || continue
    SVC_USER="$(grep '^User=' "$svcfile" | cut -d= -f2)" || continue
    [[ -n "$SVC_HOME" && -n "$SVC_USER" ]] || continue

    DOCS_TARGET="$SVC_HOME/shared/docs"
    mkdir -p "$DOCS_TARGET"
    rsync -a --delete "$SCRIPT_DIR/docs/" "$DOCS_TARGET/"
    cp "$SCRIPT_DIR/README.md" "$DOCS_TARGET/README.md"
    chown -R "$SVC_USER:$SVC_USER" "$DOCS_TARGET"
    echo "  $(basename "$svcfile" .service): docs copied to $DOCS_TARGET"
done

echo "Restarting services..."
s=$(systemctl list-units --type=service --plain --no-legend 'foci*' | awk '{print $1}')
for svc in $s; do
    echo "  $svc"
    systemctl restart "$svc"
done

# Check if FOCI_API_KEY is missing from crontab
if id foci &>/dev/null; then
    if ! crontab -u foci -l 2>/dev/null | grep -q 'FOCI_API_KEY'; then
        # Try to read the key from secrets.toml
        for svcfile in /etc/systemd/system/foci*.service; do
            [[ -f "$svcfile" ]] || continue
            SVC_HOME="$(grep '^WorkingDirectory=' "$svcfile" | cut -d= -f2)" || continue
            [[ -n "$SVC_HOME" ]] || continue
            SECRETS_FILE="$SVC_HOME/config/secrets.toml"
            if [[ -f "$SECRETS_FILE" ]]; then
                API_KEY="$(grep -A5 '^\[http\]' "$SECRETS_FILE" | grep 'api_key' | head -1 | sed 's/.*= *"\(.*\)"/\1/')"
                if [[ -n "$API_KEY" ]]; then
                    echo ""
                    echo "NOTE: FOCI_API_KEY is not set in foci's crontab."
                    echo "  Add this line near the top of the crontab (crontab -u foci -e):"
                    echo "  FOCI_API_KEY=$API_KEY"
                    break
                fi
            fi
        done
    fi
fi

echo "Done."
