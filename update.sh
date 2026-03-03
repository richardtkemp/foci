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
sudo -u foci bash -c "cd '$SCRIPT_DIR' && go build -o foci-gw . && go build -o foci ./cmd/foci && go build -o foci-call ./cmd/foci-call"

echo "Installing..."
install -m 755 "$SCRIPT_DIR/foci-gw" "$INSTALL_DIR/foci-gw"
install -m 755 "$SCRIPT_DIR/foci" "$INSTALL_DIR/foci"
install -m 755 "$SCRIPT_DIR/foci-call" "$INSTALL_DIR/foci-call"

echo "Migrating service files..."
for svcfile in /etc/systemd/system/foci*.service; do
    if [[ -f "$svcfile" ]] && grep -q "focigw" "$svcfile"; then
        echo "  Patching $svcfile: focigw → foci-gw"
        sed -i "s|${INSTALL_DIR}/focigw|${INSTALL_DIR}/foci-gw|g" "$svcfile"
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
            echo "Tell your user what just changed. Summarise the updates above in a brief, friendly message — highlight the most impactful changes and anything they'll notice. Send it via Telegram."
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
