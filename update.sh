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
sudo -u foci bash -c "cd '$SCRIPT_DIR' && make -s all"

# ===== Config compatibility pre-check =====
# Validate every foci service's config with the FRESHLY-BUILT binary BEFORE we
# install it or restart anything. If the new binary can't load a service's
# config (parse/validate error, or — strict policy — any unknown/deprecated
# key such as a renamed setting), abort here with the running daemon untouched.
# Run as root (this script's user): root can read every service's config, and
# the check has no side effects (no log init, no DB, no file writes), so it
# never produces root-owned artefacts. config.Load reads only the TOML file —
# secrets and prompt files load later at real startup — so a root check is a
# faithful test of parse/validate compatibility.
echo "Checking config compatibility..."
NEW_GW="$SCRIPT_DIR/bin/foci-gw"
checked_any=false
for svcfile in /etc/systemd/system/foci*.service; do
    [[ -f "$svcfile" ]] || continue
    svcname="$(basename "$svcfile" .service)"
    # Extract the -config argument from ExecStart (services may omit it).
    SVC_CFG="$(grep '^ExecStart=' "$svcfile" | grep -oP '(?<=-config )\S+' || true)"
    if [[ -z "$SVC_CFG" ]]; then
        echo "  $svcname: SKIP (no -config in ExecStart — cannot locate config)"
        continue
    fi
    echo -n "  $svcname: $SVC_CFG ... "
    # Resolve relative config paths against the service's own home (as it would
    # at runtime), so the root-run check doesn't warn "$HOME is not defined".
    SVC_HOME="$(grep '^WorkingDirectory=' "$svcfile" | cut -d= -f2)"
    if HOME="$SVC_HOME" "$NEW_GW" -check-config -config "$SVC_CFG"; then
        checked_any=true
    else
        echo >&2
        echo "ABORT: the newly-built foci-gw cannot load $svcname's config ($SVC_CFG)." >&2
        echo "The running daemon has NOT been touched. Fix the config above and re-run update.sh." >&2
        exit 1
    fi
done
if [[ "$checked_any" != true ]]; then
    echo "  WARNING: no service configs were checked." >&2
fi

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
    # Grant the `crontab` group per-process so foci-gw and its CC-backend
    # children can read/write their own user crontab natively. NoNewPrivileges
    # blocks the setgid escalation /usr/bin/crontab would otherwise use, so
    # native group membership is the only sandbox-safe path (the hardening stays
    # fully in force). procx drops only foci-secrets from children, so the
    # crontab group propagates through to spawned agents. Idempotent — appends
    # crontab to the SupplementaryGroups line only if not already present.
    if grep -q "^SupplementaryGroups=" "$svcfile" && ! grep -qE "^SupplementaryGroups=.*\bcrontab\b" "$svcfile"; then
        echo "  Patching $svcfile: add crontab to SupplementaryGroups"
        sed -i "/^SupplementaryGroups=/s/$/ crontab/" "$svcfile"
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
    # Workload-safe sandbox hardening (P3 supply-chain). RestrictSUIDSGID
    # reinforces NoNewPrivileges by stripping setuid/setgid bits; the Protect*
    # and LockPersonality directives are inert for foci-gw's workload but shrink
    # the kernel attack surface. Inserted after NoNewPrivileges, idempotently.
    if ! grep -q "^RestrictSUIDSGID=" "$svcfile"; then
        echo "  Patching $svcfile: add RestrictSUIDSGID=yes"
        sed -i "/^NoNewPrivileges=/a RestrictSUIDSGID=yes" "$svcfile"
    fi
    if ! grep -q "^ProtectKernelTunables=" "$svcfile"; then
        echo "  Patching $svcfile: add ProtectKernelTunables=yes"
        sed -i "/^NoNewPrivileges=/a ProtectKernelTunables=yes" "$svcfile"
    fi
    if ! grep -q "^ProtectKernelModules=" "$svcfile"; then
        echo "  Patching $svcfile: add ProtectKernelModules=yes"
        sed -i "/^NoNewPrivileges=/a ProtectKernelModules=yes" "$svcfile"
    fi
    if ! grep -q "^LockPersonality=" "$svcfile"; then
        echo "  Patching $svcfile: add LockPersonality=yes"
        sed -i "/^NoNewPrivileges=/a LockPersonality=yes" "$svcfile"
    fi

    # Shutdown budget (#948 follow-up). foci's graceful shutdown can take up to
    # graceful_shutdown_timeout (30s, waiting for in-flight turns) + ~9s for the
    # opencode server close ladder — far longer than systemd's default 10s stop
    # timeout. Without headroom systemd SIGKILLs foci-gw mid-shutdown before it
    # reaps its opencode/CC children, orphaning them (observed: "Unit process N
    # remains running after unit stopped"). Give it room, and switch KillMode
    # process->mixed so systemd SIGKILLs the whole cgroup at the deadline as a
    # backstop (catches any children foci's own cleanup didn't reach).
    if ! grep -q "^TimeoutStopSec=" "$svcfile"; then
        echo "  Patching $svcfile: add TimeoutStopSec=45s"
        sed -i '/^KillMode=/a TimeoutStopSec=45s' "$svcfile"
    fi
    if grep -q "^KillMode=process" "$svcfile"; then
        echo "  Patching $svcfile: KillMode process -> mixed"
        sed -i 's/^KillMode=process$/KillMode=mixed/' "$svcfile"
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
# --no-block: a deploy is normally launched from inside an agent's OWN turn (the
# agent runs update.sh). A blocking restart would make systemctl wait for foci to
# stop, but foci's graceful shutdown waits for that same agent's in-flight turn to
# finish — which cannot finish until systemctl returns. That circular wait pins
# shutdown until the stop timeout and SIGKILLs foci mid-cleanup, orphaning its
# opencode/CC children. Queuing the job and returning immediately lets update.sh
# exit, the agent's turn complete and release foci, then systemd performs a clean
# stop with no in-flight turn to wait on.
s=$(systemctl list-units --type=service --plain --no-legend 'foci*' | awk '{print $1}')
for svc in $s; do
    echo "  $svc"
    systemctl restart --no-block "$svc"
done

# (Removed: a block that grepped the api_key out of secrets.toml and echoed it
# in cleartext to the terminal/scrollback/CI logs to suggest a crontab
# FOCI_API_KEY line. It leaked a live remote-access key and isn't needed — cron
# jobs reach the gateway over the auto-discovered unix socket, no key required.
# P2-9.)

echo "Done."
echo ""
echo ">>> AGENT: the foci restart is now queued (async, --no-block). END YOUR TURN NOW."
echo ">>> foci's graceful shutdown waits only briefly (~30s) for your in-flight turn before"
echo ">>> being force-stopped mid-cleanup. Do any post-deploy verification in a NEW turn."
