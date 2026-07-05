#!/bin/bash
# download.sh — fetch the foci repo, then point you at `make setup`.
#
# This is the one step that must happen before a Makefile exists on the box:
# get the code. Everything else (build, provision, install, deploy) lives in the
# Makefile. Run it standalone (e.g. curl'd from GitHub) on a fresh host.
#
# Usage:
#   ./download.sh [DEST]     # clone into DEST (default: ./foci)
#
# Env:
#   FOCI_REPO_URL   git URL to clone (default: public GitHub)
set -euo pipefail

REPO_URL="${FOCI_REPO_URL:-https://github.com/richardtkemp/foci.git}"
DEST="${1:-$PWD/foci}"

if [[ -d "$DEST/.git" ]]; then
    echo "Repo already present at $DEST — pulling latest."
    git -C "$DEST" pull --ff-only
else
    echo "Cloning foci into $DEST ..."
    git clone "$REPO_URL" "$DEST"
fi

echo ""
echo "✅ foci fetched to $DEST"
echo ""
echo "Next step — install (first-time provision, builds + creates the service):"
echo "  sudo make -C $DEST setup"
echo ""
echo "To deploy updates later:"
echo "  sudo make -C $DEST update"
