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
# Usage: sudo ./scripts/rename-agent-id.sh <old-id> <new-id> [--execute] [-h|--help]
#
# Environment overrides (optional):
#   CLOD_USER     — system user (default: clod)
#   CLOD_HOME     — home directory (default: /home/clod)
#   CLOD_DATA_DIR — data directory (default: $CLOD_HOME/data)
#   CLOD_CONFIG   — config file path (default: $CLOD_HOME/config/clod.toml)
#   CLOD_SRC      — source directory for go build (default: /home/rich/git/clod)
#   CLOD_BIN      — binary install path (default: /usr/local/bin/clodgw)
set -euo pipefail

CLOD_USER="${CLOD_USER:-clod}"
CLOD_HOME="${CLOD_HOME:-/home/clod}"
CLOD_DATA_DIR="${CLOD_DATA_DIR:-$CLOD_HOME/data}"
CLOD_CONFIG="${CLOD_CONFIG:-$CLOD_HOME/config/clod.toml}"
CLOD_SRC="${CLOD_SRC:-/home/rich/git/clod}"
CLOD_BIN="${CLOD_BIN:-/usr/local/bin/clodgw}"
LOG_FILE="$CLOD_HOME/logs/migration.log"

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
    POSITIONAL=()
    for arg in "$@"; do
        case "$arg" in
            --execute) EXECUTE=true ;;
            -h|--help)
                cat <<EOF
Usage: sudo $0 <old-id> <new-id> [--execute] [-h|--help]

Renames an agent ID across all stored state:

   1. Detach from calling process (survives clod shutdown)
   2. Stop clod service (blocking)
   3. Rename session directory
   4. Rename memory database files
   5. Update data/state.json keys
   6. Update data/sessions/state.json keys
   7. Update conversation.db session references
   8. Update clod.toml agent ID
   9. Fix file ownership
  10. Build new binary
  11. Start clod service

Dry-run by default — shows what would change without doing it.
All output logged to: $LOG_FILE

Arguments:
  <old-id>    Current agent ID to rename from
  <new-id>    New agent ID to rename to

Options:
  --execute   Actually apply changes (default: dry-run)
  -h, --help  Show this help

Environment overrides:
  CLOD_USER, CLOD_HOME, CLOD_DATA_DIR, CLOD_CONFIG, CLOD_SRC, CLOD_BIN

Example:
  sudo $0 main clutch              # dry-run
  sudo $0 main clutch --execute    # apply changes
EOF
                exit 0
                ;;
            -*) echo "Unknown option: $arg" >&2; exit 1 ;;
            *) POSITIONAL+=("$arg") ;;
        esac
    done

    if [[ ${#POSITIONAL[@]} -lt 2 ]]; then
        echo "Error: requires <old-id> and <new-id> arguments." >&2
        echo "Usage: sudo $0 <old-id> <new-id> [--execute]" >&2
        exit 1
    fi

    OLD_ID="${POSITIONAL[0]}"
    NEW_ID="${POSITIONAL[1]}"

    if [[ "$OLD_ID" == "$NEW_ID" ]]; then
        echo "Error: old-id and new-id are the same: $OLD_ID" >&2
        exit 1
    fi

    if [[ $EUID -ne 0 ]]; then
        echo "Error: must be run as root (sudo)." >&2
        exit 1
    fi

    # Pre-flight checks before detaching
    if ! id "$CLOD_USER" &>/dev/null; then
        echo "Error: user $CLOD_USER does not exist." >&2
        exit 1
    fi
    if [[ ! -f "$CLOD_CONFIG" ]]; then
        echo "Error: config not found: $CLOD_CONFIG" >&2
        exit 1
    fi

    # Clear previous log (ensure directory exists)
    mkdir -p "$(dirname "$LOG_FILE")"
    > "$LOG_FILE"

    # Re-exec detached: new session leader, all FDs redirected to log file
    _DETACHED=1 nohup setsid bash "$0" "$@" >> "$LOG_FILE" 2>&1 &
    DETACHED_PID=$!
    disown "$DETACHED_PID" 2>/dev/null || true

    if $EXECUTE; then
        echo "Migration launched (detached PID $DETACHED_PID)."
        echo "Renaming agent: $OLD_ID → $NEW_ID"
        echo "clod will stop, migrate, then restart."
        echo "Log file: $LOG_FILE"
        echo ""
        echo "After restart, read $LOG_FILE for results."
    else
        echo "Dry-run launched (detached PID $DETACHED_PID)."
        echo "Renaming agent: $OLD_ID → $NEW_ID"
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
POSITIONAL=()
for arg in "$@"; do
    case "$arg" in
        --execute) EXECUTE=true ;;
        -*) ;;
        *) POSITIONAL+=("$arg") ;;
    esac
done

OLD_ID="${POSITIONAL[0]}"
NEW_ID="${POSITIONAL[1]}"

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
SESSION_OLD="$CLOD_DATA_DIR/sessions/agent/$OLD_ID"
SESSION_NEW="$CLOD_DATA_DIR/sessions/agent/$NEW_ID"

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
        MEM_OLD="$CLOD_DATA_DIR/memory-${OLD_ID}.db"
        MEM_NEW="$CLOD_DATA_DIR/memory-${NEW_ID}.db"
    else
        MEM_OLD="$CLOD_DATA_DIR/memory-${OLD_ID}.db${ext}"
        MEM_NEW="$CLOD_DATA_DIR/memory-${NEW_ID}.db${ext}"
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

update_state_json() {
    local file="$1"
    local label="$2"

    if [[ ! -f "$file" ]]; then
        warn "  Not found: $file — skipping."
        return
    fi

    local matching
    matching=$(python3 -c "
import json
with open('$file') as f:
    data = json.load(f)
keys = [k for k in data if '$OLD_ID' in k]
for k in keys:
    print(k)
" 2>/dev/null || true)

    if [[ -n "$matching" ]]; then
        while IFS= read -r key; do
            local new_key="${key//$OLD_ID/$NEW_ID}"
            info "  Key: \"$key\" → \"$new_key\""
        done <<< "$matching"

        if $EXECUTE; then
            python3 -c "
import json
with open('$file') as f:
    data = json.load(f)
new_data = {}
for k, v in data.items():
    new_key = k.replace('$OLD_ID', '$NEW_ID')
    new_data[new_key] = v
with open('$file', 'w') as f:
    json.dump(new_data, f, indent=2)
    f.write('\n')
"
            info "  $label updated."
        fi
    else
        info "  No keys contain \"$OLD_ID\" — skipping."
    fi
}

info "Step 4: Update data/state.json keys"
update_state_json "$CLOD_DATA_DIR/state.json" "state.json"
echo ""

# --- 5. Update sessions/state.json (second state file) ---

info "Step 5: Update data/sessions/state.json keys"
update_state_json "$CLOD_DATA_DIR/sessions/state.json" "sessions/state.json"
echo ""

# --- 6. Update conversation.db ---

info "Step 6: Update conversation.db session references"
CONV_DB="$CLOD_DATA_DIR/conversation.db"

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

# --- 7. Update clod.toml ---

info "Step 7: Update agent ID in clod.toml"
if grep -q "id = \"$OLD_ID\"" "$CLOD_CONFIG"; then
    info "  id = \"$OLD_ID\" → id = \"$NEW_ID\""
    if $EXECUTE; then
        sed -i "s/^id = \"$OLD_ID\"/id = \"$NEW_ID\"/" "$CLOD_CONFIG"
        info "  clod.toml updated."
    fi
else
    warn "  No 'id = \"$OLD_ID\"' found in $CLOD_CONFIG — skipping."
fi
echo ""

# --- 8. Fix ownership ---

info "Step 8: Fix file ownership"
if $EXECUTE; then
    chown -R "$CLOD_USER:$CLOD_USER" "$CLOD_DATA_DIR"
    info "  Ownership set on $CLOD_DATA_DIR"
else
    echo "[$(ts)]   (dry-run) chown -R $CLOD_USER:$CLOD_USER $CLOD_DATA_DIR"
fi
echo ""

# --- 9. Build binary ---

info "Step 9: Build clod binary"
if $EXECUTE; then
    info "  Building $CLOD_BIN from $CLOD_SRC..."
    export GOCACHE=/var/cache/go-build
    export GOPATH=/var/cache/go
    if (cd "$CLOD_SRC" && go build -o "$CLOD_BIN" .); then
        info "  Build successful."
    else
        error "  Build failed! Starting service with existing binary."
    fi
else
    echo "[$(ts)]   (dry-run) cd $CLOD_SRC && go build -o $CLOD_BIN ."
fi
echo ""

# --- 10. Start service ---

info "Step 10: Start clod service"
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
