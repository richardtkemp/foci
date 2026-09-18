#!/usr/bin/env bash
# Build the secret-redaction list for langfuse-etl WITHOUT copying any secret value anywhere.
#
# Reads secrets.toml (which the foci agent user cannot read), takes every value, and writes the SHA-256
# of each one to a file the ETL can read. The ETL hashes every token-like substring of the text it is about
# to send and replaces any whose hash is on the list with [REDACTED]. Secrets never leave this script.
#
# Run as a user that can read secrets.toml (root):
#   sudo scripts/langfuse-etl/build-redactions.sh
# Re-run whenever secrets.toml changes. Idempotent.
set -euo pipefail
SRC="${1:-/home/foci/config/secrets.toml}"
OUT="${2:-/home/foci/.config/langfuse-etl.redact-hashes}"
OWNER="${3:-foci}"

[ -r "$SRC" ] || { echo "cannot read $SRC — run with sudo" >&2; exit 1; }
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT

grep '=' "$SRC" | grep -Ev '(allowed_hosts|richardtkemp|denied_agents)' \
  | sed -E 's/^[^=]*=[[:space:]]*//' \
  | sed -E 's/^"(.*)"[[:space:]]*$/\1/; s/^'"'"'(.*)'"'"'[[:space:]]*$/\1/' \
  | while IFS= read -r v; do
      # the value itself, and each \n-separated segment of multi-line values (PEM keys etc.)
      printf '%s\n' "$v"
      printf '%s' "$v" | sed 's/\\n/\n/g'
    done \
  | awk 'length($0) >= 8' \
  | while IFS= read -r v; do printf '%s' "$v" | sha256sum | cut -d' ' -f1; done \
  | sort -u > "$tmp"

install -o "$OWNER" -g "$OWNER" -m 600 "$tmp" "$OUT"
echo "$(wc -l < "$OUT") secret hashes written to $OUT (owner $OWNER, mode 600)"
