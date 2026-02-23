#!/bin/bash
# rename-agent-id.sh — Rename an agent's ID across all stored state.
# Dry-run by default. Pass --execute to apply changes.
#
# Usage: sudo ./scripts/rename-agent-id.sh [--execute] [-h|--help]
set -euo pipefail

OLD_ID="main"
NEW_ID="clutch"
EXECUTE=false
CLOD_USER="clod"
CLOD_HOME="/home/clod"
DATA_DIR="$CLOD_HOME/data"
CONFIG_FILE="$CLOD_HOME/config/clod.toml"

# Colors (disabled if not a terminal)
if [[ -t 1 ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; NC=''
fi

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
error() { echo -e "${RED}[x]${NC} $*" >&2; }
dry()   { echo -e "${BLUE}  (dry-run)${NC} $*"; }

usage() {
    cat <<EOF
Usage: sudo $0 [--execute] [-h|--help]

Renames agent ID from "$OLD_ID" to "$NEW_ID" across all stored state:

  1. Stop clod service
  2. Rename session directory:       data/sessions/agent/$OLD_ID/ → …/$NEW_ID/
  3. Rename memory database:         data/memory-$OLD_ID.db → data/memory-$NEW_ID.db
  4. Update state.json keys:         tmux:$OLD_ID, voice:agent:$OLD_ID:*, bot:$OLD_ID
  5. Update conversation.db:         session column (agent:$OLD_ID:… → agent:$NEW_ID:…)
  6. Update clod.toml:               id = "$OLD_ID" → id = "$NEW_ID"
  7. Start clod service

Dry-run by default — shows what would change without doing it.

Options:
  --execute   Actually apply changes (default: dry-run)
  -h, --help  Show this help
EOF
    exit 0
}

for arg in "$@"; do
    case "$arg" in
        --execute) EXECUTE=true ;;
        -h|--help) usage ;;
        *) error "Unknown option: $arg"; usage ;;
    esac
done

run() {
    if $EXECUTE; then
        "$@"
    else
        dry "$*"
    fi
}

# --- Pre-checks ---

if [[ $EUID -ne 0 ]]; then
    error "This script must be run as root (sudo)."
    exit 1
fi

if ! id "$CLOD_USER" &>/dev/null; then
    error "User $CLOD_USER does not exist."
    exit 1
fi

if [[ ! -f "$CONFIG_FILE" ]]; then
    error "Config file not found: $CONFIG_FILE"
    exit 1
fi

if $EXECUTE; then
    info "EXECUTE mode — changes will be applied."
else
    info "DRY-RUN mode — no changes will be made. Pass --execute to apply."
fi
echo ""

# --- 1. Stop service ---

info "Step 1: Stop clod service"
if systemctl is-active --quiet clod 2>/dev/null; then
    run systemctl stop clod
    if $EXECUTE; then
        # Wait for clean shutdown
        sleep 2
        info "  Service stopped."
    fi
else
    info "  Service not running, skipping."
fi
echo ""

# --- 2. Rename session directory ---

info "Step 2: Rename session directory"
SESSION_OLD="$DATA_DIR/sessions/agent/$OLD_ID"
SESSION_NEW="$DATA_DIR/sessions/agent/$NEW_ID"

if [[ -d "$SESSION_OLD" ]]; then
    if [[ -d "$SESSION_NEW" ]]; then
        error "  Target already exists: $SESSION_NEW — aborting to avoid data loss."
        exit 1
    fi
    info "  $SESSION_OLD → $SESSION_NEW"
    run mv "$SESSION_OLD" "$SESSION_NEW"
else
    warn "  Source not found: $SESSION_OLD — skipping."
fi
echo ""

# --- 3. Rename memory database ---

info "Step 3: Rename memory database files"
for ext in "" "-shm" "-wal"; do
    MEM_OLD="$DATA_DIR/memory-${OLD_ID}${ext}.db"
    # Handle the base .db and .db-shm/.db-wal naming
    if [[ "$ext" == "" ]]; then
        MEM_OLD="$DATA_DIR/memory-${OLD_ID}.db"
        MEM_NEW="$DATA_DIR/memory-${NEW_ID}.db"
    else
        MEM_OLD="$DATA_DIR/memory-${OLD_ID}.db${ext}"
        MEM_NEW="$DATA_DIR/memory-${NEW_ID}.db${ext}"
    fi

    if [[ -f "$MEM_OLD" ]]; then
        if [[ -f "$MEM_NEW" ]]; then
            error "  Target already exists: $MEM_NEW — aborting."
            exit 1
        fi
        info "  $MEM_OLD → $MEM_NEW"
        run mv "$MEM_OLD" "$MEM_NEW"
    else
        if [[ "$ext" == "" ]]; then
            warn "  Not found: $MEM_OLD — skipping."
        fi
    fi
done
echo ""

# --- 4. Update state.json ---

info "Step 4: Update state.json keys"
STATE_FILE="$DATA_DIR/state.json"

if [[ -f "$STATE_FILE" ]]; then
    # Show current keys containing the old ID
    MATCHING_KEYS=$(python3 -c "
import json, sys
with open('$STATE_FILE') as f:
    data = json.load(f)
keys = [k for k in data if '$OLD_ID' in k]
for k in keys:
    print(k)
" 2>/dev/null || true)

    if [[ -n "$MATCHING_KEYS" ]]; then
        while IFS= read -r key; do
            new_key="${key//$OLD_ID/$NEW_ID}"
            info "  Key: \"$key\" → \"$new_key\""
        done <<< "$MATCHING_KEYS"

        if $EXECUTE; then
            python3 -c "
import json
with open('$STATE_FILE') as f:
    data = json.load(f)
new_data = {}
for k, v in data.items():
    new_key = k.replace('$OLD_ID', '$NEW_ID')
    new_data[new_key] = v
with open('$STATE_FILE', 'w') as f:
    json.dump(new_data, f, indent=2)
    f.write('\n')
"
            info "  state.json updated."
        fi
    else
        info "  No keys contain \"$OLD_ID\" — skipping."
    fi
else
    warn "  Not found: $STATE_FILE — skipping."
fi
echo ""

# --- 5. Update conversation.db ---

info "Step 5: Update conversation.db session references"
CONV_DB="$DATA_DIR/conversation.db"

if [[ -f "$CONV_DB" ]]; then
    COUNT=$(sqlite3 "$CONV_DB" "SELECT COUNT(*) FROM messages WHERE session LIKE 'agent:${OLD_ID}:%';" 2>/dev/null || echo "0")
    if [[ "$COUNT" -gt 0 ]]; then
        info "  $COUNT rows with session 'agent:${OLD_ID}:*'"
        info "  UPDATE messages SET session = replace(session, 'agent:${OLD_ID}:', 'agent:${NEW_ID}:')"
        if $EXECUTE; then
            sqlite3 "$CONV_DB" "UPDATE messages SET session = replace(session, 'agent:${OLD_ID}:', 'agent:${NEW_ID}:') WHERE session LIKE 'agent:${OLD_ID}:%';"
            info "  conversation.db updated."
        fi
    else
        info "  No matching rows — skipping."
    fi
else
    warn "  Not found: $CONV_DB — skipping."
fi
echo ""

# --- 6. Update clod.toml ---

info "Step 6: Update agent ID in clod.toml"
if grep -q "id = \"$OLD_ID\"" "$CONFIG_FILE"; then
    info "  id = \"$OLD_ID\" → id = \"$NEW_ID\""
    if $EXECUTE; then
        sed -i "s/^id = \"$OLD_ID\"/id = \"$NEW_ID\"/" "$CONFIG_FILE"
        info "  clod.toml updated."
    fi
else
    warn "  No 'id = \"$OLD_ID\"' found in $CONFIG_FILE — skipping."
fi
echo ""

# --- 7. Fix ownership ---

if $EXECUTE; then
    info "Step 7: Fix file ownership"
    chown -R "$CLOD_USER:$CLOD_USER" "$DATA_DIR"
    info "  Ownership set on $DATA_DIR"
else
    info "Step 7: Fix file ownership"
    dry "chown -R $CLOD_USER:$CLOD_USER $DATA_DIR"
fi
echo ""

# --- 8. Start service ---

info "Step 8: Start clod service"
run systemctl start clod
if $EXECUTE; then
    sleep 1
    if systemctl is-active --quiet clod 2>/dev/null; then
        info "  Service started successfully."
    else
        error "  Service failed to start! Check: journalctl -u clod -n 20"
    fi
fi
echo ""

# --- Summary ---

if $EXECUTE; then
    info "Migration complete: agent \"$OLD_ID\" → \"$NEW_ID\""
else
    info "Dry-run complete. Run with --execute to apply changes."
fi
