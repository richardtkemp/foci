#!/bin/bash
set -euo pipefail

CONFIG_DIR="/home/foci/config"
CONFIG_FILE="$CONFIG_DIR/foci.toml"

# Sync shared files (docs, skills, defaults) from the image into the
# persistent volume. This ensures updates are picked up on rebuild.
mkdir -p /home/foci/shared /home/foci/data /home/foci/logs
cp -r /opt/foci-shared/* /home/foci/shared/
cp /opt/foci-shared/.foci-commit /home/foci/data/.foci-commit

# First run: run the setup wizard to generate config
if [ ! -f "$CONFIG_FILE" ]; then
	echo "[foci] First run detected — launching setup wizard..."
	mkdir -p "$CONFIG_DIR"

	SETUP_ARGS="--config-dir $CONFIG_DIR"

	# If env vars are set, run non-interactively
	if [ -n "${FOCI_TELEGRAM_TOKEN:-}" ] && [ -n "${FOCI_TELEGRAM_USER:-}" ]; then
		SETUP_ARGS="$SETUP_ARGS --non-interactive"
		SETUP_ARGS="$SETUP_ARGS --bot-token $FOCI_TELEGRAM_TOKEN"
		SETUP_ARGS="$SETUP_ARGS --user-id $FOCI_TELEGRAM_USER"
		[ -n "${FOCI_AUTH_METHOD:-}" ] && SETUP_ARGS="$SETUP_ARGS --auth-method $FOCI_AUTH_METHOD"
		[ -n "${FOCI_AUTH_TOKEN:-}" ] && SETUP_ARGS="$SETUP_ARGS --auth-token $FOCI_AUTH_TOKEN"
		[ -n "${FOCI_AGENT_ID:-}" ] && SETUP_ARGS="$SETUP_ARGS --agent-id $FOCI_AGENT_ID"
		[ -n "${FOCI_CHAR_MODE:-}" ] && SETUP_ARGS="$SETUP_ARGS --char-mode $FOCI_CHAR_MODE"
	else
		echo "[foci] ERROR: FOCI_TELEGRAM_TOKEN and FOCI_TELEGRAM_USER must be set."
		echo "[foci] Copy docker/.env.example to docker/.env and fill in the values."
		exit 1
	fi

	echo "[foci] Running: foci setup $SETUP_ARGS"
	# shellcheck disable=SC2086
	foci setup $SETUP_ARGS
	echo "[foci] Setup complete — config written to $CONFIG_FILE"
fi

echo "[foci] Starting foci-gw..."
exec foci-gw -config "$CONFIG_FILE"
