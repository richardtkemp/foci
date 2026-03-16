#!/bin/bash
set -euo pipefail

CONFIG_DIR="/home/foci/config"
CONFIG_FILE="$CONFIG_DIR/foci.toml"
SECRETS_FILE="$CONFIG_DIR/secrets.toml"
COMMIT_FILE="/home/foci/data/.foci-commit"
IMAGE_COMMIT_FILE="/opt/foci-shared/.foci-commit"

# ── Helper: run a command as the foci user ──
run_as_foci() {
	HOME=/home/foci setpriv --reuid=foci --regid=foci --init-groups \
		--ambient-caps=+setgid -- "$@"
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
		[ -n "${FOCI_AUTH_METHOD:-}" ] && SETUP_ARGS="$SETUP_ARGS --auth-method $FOCI_AUTH_METHOD"
		[ -n "${FOCI_AUTH_TOKEN:-}" ] && SETUP_ARGS="$SETUP_ARGS --auth-token $FOCI_AUTH_TOKEN"
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

# ── Harden secrets.toml ──
# Replicate the systemd security model: secrets.toml is owned by
# root:foci-secrets with mode 0660. The main foci-gw process has
# foci-secrets in its supplementary groups (via setpriv --init-groups),
# but child processes have it dropped (via procattr.go + CAP_SETGID).
if [ -f "$SECRETS_FILE" ]; then
	chown root:foci-secrets "$SECRETS_FILE"
	chmod 0660 "$SECRETS_FILE"
fi

# ── Launch foci-gw ──
# Drop to foci user with:
#   --init-groups: sets supplementary groups from /etc/group (includes foci-secrets)
#   --ambient-caps=+setgid: grants CAP_SETGID so procattr.go can drop
#                           foci-secrets from child processes
echo "[foci] Starting foci-gw..."
exec env HOME=/home/foci setpriv --reuid=foci --regid=foci --init-groups \
	--ambient-caps=+setgid \
	-- foci-gw -config "$CONFIG_FILE"
