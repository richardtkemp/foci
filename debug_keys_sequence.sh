#!/bin/bash

# This script replicates the executeKeysSequence logic from tools/tmux.go
# with extensive logging to debug why keys aren't making it to the prompt

set -e

# Parameters
SESSION_NAME="${1:-debug-claude-session}"
COMMAND="${2:-claude --dangerously-skip-permissions}"
KEYS="${3:-1+1}"

LOG_FILE="/tmp/debug_keys_${SESSION_NAME}.log"
CAPTURE_DIR="/tmp/debug_captures"

# Create directory for captures
mkdir -p "$CAPTURE_DIR"

# Logging functions
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S.%3N')] $@" | tee -a "$LOG_FILE"
}

log_pane_content() {
    local label="$1"
    local file="$2"
    log "--- PANE CAPTURE: $label ---"
    log "File: $file"
    if [ -f "$file" ]; then
        log "Content length: $(wc -c < "$file") bytes, $(wc -l < "$file") lines"
        log "Raw content:"
        head -20 "$file" | sed 's/^/  /' | tee -a "$LOG_FILE"
        if [ $(wc -l < "$file") -gt 20 ]; then
            log "  ... (truncated)"
        fi
    else
        log "File not found!"
    fi
}

normalize_pane_content() {
    local content="$1"
    # Remove TUI noise patterns using sed instead of Go regex
    # Patterns to remove:
    # - Elapsed timers: digits[hm] whitespace digits[ms]
    # - Clocks: digits:digits or digits:digits:digits with optional AM/PM
    # - Durations: digits.digits followed by 's'
    echo "$content" | sed -E \
        -e 's/[0-9]+[hm]\s+[0-9]+[ms]//g' \
        -e 's/[0-9]+:[0-9]{2}(:[0-9]{2})?(\s*[AP]M)?//g' \
        -e 's/[0-9]+\.[0-9]+s//g'
}

compare_normalized() {
    local file1="$1"
    local file2="$2"
    local label1="${3:-Content1}"
    local label2="${4:-Content2}"

    if [ ! -f "$file1" ] || [ ! -f "$file2" ]; then
        log "ERROR: Missing comparison file"
        return 1
    fi

    local content1=$(cat "$file1")
    local content2=$(cat "$file2")

    local norm1=$(normalize_pane_content "$content1")
    local norm2=$(normalize_pane_content "$content2")

    log "Comparing $label1 vs $label2 (normalized)"
    log "  Normalized content1 length: ${#norm1} chars"
    log "  Normalized content2 length: ${#norm2} chars"

    if [ "$norm1" = "$norm2" ]; then
        log "  RESULT: IDENTICAL"
        return 0
    else
        log "  RESULT: DIFFERENT"
        return 1
    fi
}

# Main script
log "============================================"
log "DEBUG: executeKeysSequence Logic"
log "============================================"
log "SESSION_NAME: $SESSION_NAME"
log "COMMAND: $COMMAND"
log "KEYS: $KEYS"
log ""

# Clean up any existing session with the same name
if tmux has-session -t "$SESSION_NAME" 2>/dev/null; then
    log "Killing existing session: $SESSION_NAME"
    tmux kill-session -t "$SESSION_NAME"
    sleep 1
fi

# Step 1: Create new tmux session with sh shell
log "Step 1: Creating new tmux session '$SESSION_NAME'"
tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50 sh
sleep 0.5
log "Session created successfully"

# Step 2: Send the command string to the pane (WITHOUT pressing enter yet)
log ""
log "Step 2: Sending command to pane (no Enter): '$COMMAND'"
tmux send-keys -t "$SESSION_NAME" "$COMMAND"
sleep 0.1
log "Command sent to pane"

# Step 3: Capture baseline pane content
log ""
log "Step 3: Capturing baseline pane content"
BASELINE_FILE="$CAPTURE_DIR/baseline.txt"
tmux capture-pane -t "$SESSION_NAME" -p > "$BASELINE_FILE"
log_pane_content "BASELINE (after sending command, before Enter)" "$BASELINE_FILE"
BASELINE_NORMALIZED=$(normalize_pane_content "$(cat "$BASELINE_FILE")")
log "Baseline normalized length: ${#BASELINE_NORMALIZED} chars"

# Step 4: Wait briefly, then press Enter (to actually run the command)
log ""
log "Step 4: Waiting 200ms then pressing Enter"
sleep 0.2
tmux send-keys -t "$SESSION_NAME" "Enter"
log "Enter pressed"

# Steps 5: Poll until output differs from baseline (command has started)
log ""
log "Step 5: Polling until command starts producing output (30s timeout, 200ms interval)"
START_TIMEOUT=$(($(date +%s%N) + 30000000000)) # 30 seconds in nanoseconds
POLL_COUNT=0
while true; do
    CURRENT_TIME=$(date +%s%N)
    if [ $CURRENT_TIME -gt $START_TIMEOUT ]; then
        log "ERROR: Timeout waiting for command to start producing output"
        exit 1
    fi

    sleep 0.2
    POLL_COUNT=$((POLL_COUNT + 1))

    CURRENT_FILE="$CAPTURE_DIR/current_${POLL_COUNT}.txt"
    tmux capture-pane -t "$SESSION_NAME" -p > "$CURRENT_FILE"

    CURRENT_NORMALIZED=$(normalize_pane_content "$(cat "$CURRENT_FILE")")

    log "Poll #$POLL_COUNT: Normalized content length: ${#CURRENT_NORMALIZED} chars"

    if [ "$CURRENT_NORMALIZED" != "$BASELINE_NORMALIZED" ]; then
        log "✓ OUTPUT DIFFERS FROM BASELINE - Command has started!"
        log_pane_content "CURRENT (after output differs)" "$CURRENT_FILE"
        break
    fi
done

# Step 6: Poll until output is stable for 5 consecutive polls (1 second)
log ""
log "Step 6: Polling until output stabilizes (60s timeout, 200ms interval, need 5 identical)"
STABLE_TIMEOUT=$(($(date +%s%N) + 60000000000)) # 60 seconds in nanoseconds
STABLE_COUNT=0
LAST_CONTENT=""
STABILITY_POLL_COUNT=0

# Initialize with current content
LAST_CONTENT=$(normalize_pane_content "$(cat "$CURRENT_FILE")")

while true; do
    CURRENT_TIME=$(date +%s%N)
    if [ $CURRENT_TIME -gt $STABLE_TIMEOUT ]; then
        log "ERROR: Timeout waiting for output to stabilize"
        exit 1
    fi

    sleep 0.2
    STABILITY_POLL_COUNT=$((STABILITY_POLL_COUNT + 1))

    STABLE_FILE="$CAPTURE_DIR/stable_${STABILITY_POLL_COUNT}.txt"
    tmux capture-pane -t "$SESSION_NAME" -p > "$STABLE_FILE"

    CURRENT_NORMALIZED=$(normalize_pane_content "$(cat "$STABLE_FILE")")

    log "Stability poll #$STABILITY_POLL_COUNT: Content length: ${#CURRENT_NORMALIZED} chars"

    if [ "$CURRENT_NORMALIZED" = "$LAST_CONTENT" ]; then
        STABLE_COUNT=$((STABLE_COUNT + 1))
        log "  → Stable count: $STABLE_COUNT/5"
        if [ $STABLE_COUNT -ge 5 ]; then
            log "✓ OUTPUT IS STABLE - Ready to send keys!"
            log_pane_content "FINAL STABLE STATE" "$STABLE_FILE"
            break
        fi
    else
        log "  → Content changed, resetting stable count from $STABLE_COUNT to 0"
        STABLE_COUNT=0
        LAST_CONTENT="$CURRENT_NORMALIZED"
    fi
done

# Step 7: Send the keys string to the pane
log ""
log "Step 7: Sending keys to pane: '$KEYS'"
tmux send-keys -t "$SESSION_NAME" "$KEYS"
log "Keys sent"

# Step 8: Wait 200ms
log ""
log "Step 8: Waiting 200ms"
sleep 0.2
BEFORE_FINAL_ENTER="$CAPTURE_DIR/before_final_enter.txt"
tmux capture-pane -t "$SESSION_NAME" -p > "$BEFORE_FINAL_ENTER"
log_pane_content "AFTER KEYS, BEFORE FINAL ENTER" "$BEFORE_FINAL_ENTER"

# Step 9: Press enter
log ""
log "Step 9: Pressing final Enter"
tmux send-keys -t "$SESSION_NAME" "Enter"
log "Final Enter pressed"

# Give it a moment and capture final state
sleep 0.5
FINAL_FILE="$CAPTURE_DIR/final.txt"
tmux capture-pane -t "$SESSION_NAME" -p > "$FINAL_FILE"
log_pane_content "FINAL STATE (after Enter)" "$FINAL_FILE"

log ""
log "============================================"
log "DEBUG COMPLETE"
log "============================================"
log "Log file: $LOG_FILE"
log "Capture directory: $CAPTURE_DIR"
log ""
log "Next steps:"
log "1. Review the captures in $CAPTURE_DIR"
log "2. Check if keys appear in 'before_final_enter.txt' or 'final.txt'"
log "3. Look for the keys text in the session with: tmux capture-pane -t $SESSION_NAME -p"
log ""
log "To inspect the session live:"
log "  tmux attach-session -t $SESSION_NAME"
log ""
log "To kill the session:"
log "  tmux kill-session -t $SESSION_NAME"
