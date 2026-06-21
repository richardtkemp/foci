#!/bin/bash
set -euo pipefail

CONFIG_DIR="/home/foci/config"
CONFIG_FILE="$CONFIG_DIR/foci.toml"
SECRETS_FILE="$CONFIG_DIR/secrets.toml"
COMMIT_FILE="/home/foci/data/.foci-commit"
IMAGE_COMMIT_FILE="/opt/foci-shared/.foci-commit"

# ── Helper: run a setup command as the foci user ──
# Uses --init-groups (which no longer includes foci-secrets, since foci is not a
# member in /etc/group) and grants no CAP_SETGID — these helper tasks neither
# read secrets nor spawn the gateway, so they get no access to the secrets group.
run_as_foci() {
	HOME=/home/foci setpriv --reuid=foci --regid=foci --init-groups -- "$@"
}

# Sync shared files (docs, skills, defaults) from the image into the
# persistent volume. This ensures updates are picked up on rebuild.
run_as_foci mkdir -p /home/foci/shared /home/foci/data /home/foci/logs
run_as_foci cp -r /opt/foci-shared/. /home/foci/shared/

# ── Generate changelog on update ──
# Compare the commit baked into the image with the one stored in the
# persistent volume from the previous run.
NEW_COMMIT="$(cat "$IMAGE_COMMIT_FILE" 2>/dev/null || echo unknown)"
OLD_COMMIT="$(cat "$COMMIT_FILE" 2>/dev/null || echo "")"

if [ -n "$OLD_COMMIT" ] && [ "$OLD_COMMIT" != "$NEW_COMMIT" ] && [ "$NEW_COMMIT" != "unknown" ]; then
	echo "[foci] Updated: $OLD_COMMIT → $NEW_COMMIT"
	# The WELCOME.md is picked up by foci-gw on startup and sent to the user.
	run_as_foci tee /home/foci/data/WELCOME.md > /dev/null << WELCOME
# Foci Updated

Updated from \`$OLD_COMMIT\` to \`$NEW_COMMIT\` on $(date -u '+%Y-%m-%d %H:%M UTC').

## Instructions

Tell your user that foci has been updated. The update was applied by rebuilding the Docker image.
WELCOME
fi

run_as_foci cp "$IMAGE_COMMIT_FILE" "$COMMIT_FILE"

# First run: run the first-run wizard to generate config
if [ ! -f "$CONFIG_FILE" ]; then
	echo "[foci] First run detected — launching first-run wizard..."
	run_as_foci mkdir -p "$CONFIG_DIR"

	SETUP_ARGS="--config-dir $CONFIG_DIR"

	# At least one platform must be configured
	HAS_TELEGRAM=false
	HAS_DISCORD=false
	[ -n "${FOCI_TELEGRAM_TOKEN:-}" ] && [ -n "${FOCI_TELEGRAM_USER:-}" ] && HAS_TELEGRAM=true
	[ -n "${FOCI_DISCORD_TOKEN:-}" ] && [ -n "${FOCI_DISCORD_USER:-}" ] && HAS_DISCORD=true

	if [ "$HAS_TELEGRAM" = true ] || [ "$HAS_DISCORD" = true ]; then
		SETUP_ARGS="$SETUP_ARGS --non-interactive"
		# Telegram credentials
		if [ "$HAS_TELEGRAM" = true ]; then
			SETUP_ARGS="$SETUP_ARGS --telegram-bot-token $FOCI_TELEGRAM_TOKEN"
			SETUP_ARGS="$SETUP_ARGS --telegram-user-id $FOCI_TELEGRAM_USER"
		fi
		# Discord credentials
		if [ "$HAS_DISCORD" = true ]; then
			SETUP_ARGS="$SETUP_ARGS --discord-bot-token $FOCI_DISCORD_TOKEN"
			SETUP_ARGS="$SETUP_ARGS --discord-user-id $FOCI_DISCORD_USER"
		fi
		[ -n "${FOCI_PROVIDER:-}" ] && SETUP_ARGS="$SETUP_ARGS --provider $FOCI_PROVIDER"
		[ -n "${FOCI_API_KEY:-}" ] && SETUP_ARGS="$SETUP_ARGS --api-key $FOCI_API_KEY"
		[ -n "${FOCI_AGENT_ID:-}" ] && SETUP_ARGS="$SETUP_ARGS --agent-id $FOCI_AGENT_ID"
		[ -n "${FOCI_AGENT_NAME:-}" ] && SETUP_ARGS="$SETUP_ARGS --display-name $FOCI_AGENT_NAME"
		[ -n "${FOCI_CHAR_MODE:-}" ] && SETUP_ARGS="$SETUP_ARGS --char-mode $FOCI_CHAR_MODE"
		# Import character/memory files baked into the image (docker/character/, docker/memory/)
		ls /opt/foci-import/character/*.md &>/dev/null && SETUP_ARGS="$SETUP_ARGS --char-import-dir /opt/foci-import/character"
		ls /opt/foci-import/memory/*.md &>/dev/null && SETUP_ARGS="$SETUP_ARGS --memory-import-dir /opt/foci-import/memory"
	else
		echo "[foci] ERROR: At least one platform must be configured."
		echo "[foci] Set FOCI_TELEGRAM_TOKEN + FOCI_TELEGRAM_USER for Telegram,"
		echo "[foci] and/or FOCI_DISCORD_TOKEN + FOCI_DISCORD_USER for Discord."
		echo "[foci] Copy docker/.env.example to docker/.env and fill in the values."
		exit 1
	fi

	echo "[foci] Running: foci first-run $SETUP_ARGS"
	# shellcheck disable=SC2086
	run_as_foci foci first-run $SETUP_ARGS
	echo "[foci] First-run complete — config written to $CONFIG_FILE"
fi

# ── Install Claude Code for the claude-code (ccstream) backend ──
# The ccstream backend shells out to the `claude` CLI. Install it on demand when
# any agent is configured with backend = "claude-code" and it isn't already on
# PATH. We use the native single-binary installer (https://claude.ai/install.sh),
# NOT npm: it pulls one platform binary with no Node.js/npm dependency tree —
# smaller, faster, and matches how a bare-metal foci host installs `claude`.
#
# The installer is HOME-based (it runs `claude install` → $HOME/.local/bin). We
# point HOME at /opt/claude (outside the /home/foci volume) and symlink the
# result into /usr/local/bin so the binary is on PATH for foci-gw and its
# children. After install, authenticate from chat with /login — no API key
# required.
if grep -q 'backend = "claude-code"' "$CONFIG_FILE" 2>/dev/null && ! command -v claude >/dev/null 2>&1; then
	echo "[foci] claude-code backend detected — installing Claude Code (native, no Node)..."
	CLAUDE_HOME=/opt/claude
	mkdir -p "$CLAUDE_HOME"
	if curl -fsSL https://claude.ai/install.sh | HOME="$CLAUDE_HOME" bash; then
		ln -sf "$CLAUDE_HOME/.local/bin/claude" /usr/local/bin/claude
		echo "[foci] Claude Code installed: $(command -v claude || echo '?') ($(/usr/local/bin/claude --version 2>/dev/null || echo 'version unknown'))"
	else
		echo "[foci] WARNING: Claude Code install failed — ccstream agents will not start until 'claude' is on PATH."
	fi
fi

# ── Harden secrets.toml ──
# Replicate the systemd security model: secrets.toml is owned by
# root:foci-secrets with mode 0660. The foci-gw process is granted the
# foci-secrets group explicitly (setpriv --groups, below), but child processes
# have it dropped (procx + CAP_SETGID), and the foci user is not a permanent
# group member, so sg/newgrp can't re-acquire it.
if [ -f "$SECRETS_FILE" ]; then
	chown root:foci-secrets "$SECRETS_FILE"
	chmod 0660 "$SECRETS_FILE"
fi

# ── Launch foci-gw ──
# Drop to foci user with:
#   --groups foci-secrets: grant the secrets group to THIS process only
#                          (not via /etc/group membership, which would be
#                          re-acquirable via sg/newgrp — P0-1)
#   --ambient-caps=+setgid: grants CAP_SETGID so procx can drop foci-secrets
#                           from child processes (procx then clears the ambient
#                           set so children can't inherit CAP_SETGID across exec)
#   --no-new-privs: block setuid sg/newgrp/sudo for the whole process tree
echo "[foci] Starting foci-gw..."
# A capability can only be raised into the ambient set if it is also in the
# inheritable set, so --inh-caps=+setgid must accompany --ambient-caps=+setgid.
# Without it setpriv silently leaves CapAmb empty (exit 0), foci-gw cannot drop
# foci-secrets from children, and the security self-check aborts startup.
exec env HOME=/home/foci setpriv --reuid=foci --regid=foci --groups foci-secrets \
	--inh-caps=+setgid --ambient-caps=+setgid --no-new-privs \
	-- foci-gw -config "$CONFIG_FILE"
