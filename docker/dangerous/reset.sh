#!/usr/bin/env bash
#
# Destroy and recreate the foci Docker container from scratch.
# This wipes the foci-home volume — all sessions, memory, and config are lost.
#
set -euo pipefail

cd "$(dirname "$0")/.."

compose="docker compose -f compose.yml"

echo "This will DESTROY the foci container and wipe all persistent data."
echo "Sessions, memory, character files, and config will be permanently deleted."
echo ""
read -rp "Type YES to confirm: " confirm
if [[ "$confirm" != "YES" ]]; then
	echo "Aborted."
	exit 1
fi

echo "Stopping and removing container..."
$compose down

echo "Removing foci-home volume..."
docker volume rm docker_foci-home 2>/dev/null || docker volume rm foci-home 2>/dev/null || true

echo "Rebuilding and starting fresh..."
$compose up --build -d

echo "Done. Container is running with a clean volume."
