#!/bin/bash
# rename-agent-id.sh — Rename an agent's ID across all stored state.
#
# Designed to be called FROM the agent (via exec). The script detaches itself
# into a background process (setsid), then stops clod (killing the caller),
# runs the migration, and starts clod again. All output goes to a log file
# the agent can read after restart.
#
# Dry-run by default. Pass --execute to apply changes.
#
# Usage: sudo ./scripts/rename-agent-id.sh [--execute] [-h|--help]
set -euo pipefail

OLD_ID="main"
NEW_ID="clutch"
CLOD_USER="clod"
CLOD_HOME="/home/clod"
DATA_DIR="$CLOD_HOME/data"
CONFIG_FILE="$CLOD_HOME/config/clod.toml"
LOG_FILE="/tmp/rename-agent-id.log"

# ============================================================================
# DETACH LOGIC
#
# When the agent calls this script, it runs inside clod's process tree.
# `systemctl stop clod` will kill the caller. To survive:
#   1. Outer invocation (no _DETACHED env) re-execs itself via setsid/nohup
#   2. Inner invocation (_DETACHED=1) does the actual work
#   3. Outer prints the log path and exits immediately
# ============================================================================

if [[ "${_DETACHED:-}" != "1" ]]; then
    # --- Outer invocation: parse args, detach, exit ---

    EXECUTE=false
    for arg in "$@"; do
        case "$arg" in
            --execute) EXECUTE=true ;;
            -h|--help)
                cat <<EOF
Usage: sudo $0 [--execute] [-h|--help]

Renames agent ID from "$OLD_ID" to "$NEW_ID" across all stored state:

  1. Detach from calling process (survives clod shutdown)
  2. Stop clod service (blocking)
  3. Rename session directory
  4. Rename memory database files
  5. Update state.json keys
  6. Update conversation.db session references
  7. Update clod.toml agent ID
  8. Fix file ownership
  9. Start clod service

Dry-run by default — shows what would change without doing it.
All output logged to: $LOG_FILE

Options:
  --execute   Actually apply changes (default: dry-run)
  -h, --help  Show this help
EOF
                exit 0
                ;;
            *) echo "Unknown option: $arg" >&2; exit 1 ;;
        esac
    done

    if [[ $EUID -ne 0 ]]; then
        echo "Error: must be run as root (sudo)." >&2
        exit 1
    fi

    # Pre-flight checks before detaching
    if ! id "$CLOD_USER" &>/dev/null; then
        echo "Error: user $CLOD_USER does not exist." >&2
        exit 1
    fi
    if [[ ! -f "$CONFIG_FILE" ]]; then
        echo "Error: config not found: $CONFIG_FILE" >&2
        exit 1
    fi

    # Clear previous log
    > "$LOG_FILE"

    # Re-exec detached: new session leader, all FDs redirected to log file
    _DETACHED=1 nohup setsid bash "$0" "$@" >> "$LOG_FILE" 2>&1 &
    DETACHED_PID=$!
    disown "$DETACHED_PID" 2>/dev/null || true

    if $EXECUTE; then
        echo "Migration launched (detached PID $DETACHED_PID)."
        echo "clod will stop, migrate, then restart."
        echo "Log file: $LOG_FILE"
        echo ""
        echo "After restart, read $LOG_FILE for results."
    else
        echo "Dry-run launched (detached PID $DETACHED_PID)."
        echo "Log file: $LOG_FILE"
        echo ""
        echo "NOTE: dry-run still detaches but does NOT stop clod."
        echo "After it finishes, read $LOG_FILE for what would change."
    fi
    exit 0
fi

# ============================================================================
# INNER (DETACHED) INVOCATION — does the actual work
# All output goes to $LOG_FILE via the redirect in the nohup line above.
# ============================================================================

EXECUTE=false
for arg in "$@"; do
    case "$arg" in
        --execute) EXECUTE=true ;;
    esac
done

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
info()  { echo "[$(ts)] [+] $*"; }
warn()  { echo "[$(ts)] [!] $*"; }
error() { echo "[$(ts)] [x] $*"; }

run() {
    if $EXECUTE; then
        "$@"
    else
        echo "[$(ts)]   (dry-run) $*"
    fi
}

info "=== rename-agent-id: $OLD_ID → $NEW_ID ==="
if $EXECUTE; then
    info "EXECUTE mode — changes will be applied."
else
    info "DRY-RUN mode — no changes will be made."
fi
echo ""

# --- 1. Stop service ---

info "Step 1: Stop clod service"
if $EXECUTE; then
    if systemctl is-active --quiet clod 2>/dev/null; then
        info "  Stopping clod (blocking)..."
        systemctl stop clod
        # Wait for process to fully exit
        sleep 3
        if systemctl is-active --quiet clod 2>/dev/null; then
            error "  Service still running after stop! Aborting."
            exit 1
        fi
        info "  Service stopped."
    else
        info "  Service not running."
    fi
else
    info "  (dry-run) would stop clod service"
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
    MATCHING_KEYS=$(python3 -c "
import json
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

info "Step 7: Fix file ownership"
if $EXECUTE; then
    chown -R "$CLOD_USER:$CLOD_USER" "$DATA_DIR"
    info "  Ownership set on $DATA_DIR"
else
    echo "[$(ts)]   (dry-run) chown -R $CLOD_USER:$CLOD_USER $DATA_DIR"
fi
echo ""

# --- 8. Start service ---

info "Step 8: Start clod service"
if $EXECUTE; then
    systemctl start clod
    sleep 2
    if systemctl is-active --quiet clod 2>/dev/null; then
        info "  Service started successfully."
    else
        error "  Service failed to start! Check: journalctl -u clod -n 20"
        exit 1
    fi
else
    echo "[$(ts)]   (dry-run) would start clod service"
fi
echo ""

# --- Done ---

if $EXECUTE; then
    info "=== Migration complete: agent \"$OLD_ID\" → \"$NEW_ID\" ==="
else
    info "=== Dry-run complete. Run with --execute to apply. ==="
fi
