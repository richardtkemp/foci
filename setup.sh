#!/bin/bash
# Clod setup script — idempotent. Run once to install, again to update.
# Usage: sudo ./setup.sh [--dry-run]
set -euo pipefail

CLOD_USER="clod"
CLOD_HOME="/home/$CLOD_USER"
INSTALL_DIR="/usr/local/bin"
SERVICE_FILE="/etc/systemd/system/clod.service"
LOGROTATE_FILE="/etc/logrotate.d/clod"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DRY_RUN=false

# Colors (disabled if not a terminal)
if [[ -t 1 ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; NC=''
fi

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
error() { echo -e "${RED}[x]${NC} $*" >&2; }

run() {
    if $DRY_RUN; then
        echo "  (dry-run) $*"
    else
        "$@"
    fi
}

# Parse flags
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=true ;;
        --help|-h)
            echo "Usage: sudo $0 [--dry-run]"
            echo "Installs clod agent. Idempotent — safe to re-run."
            exit 0
            ;;
        *) error "Unknown flag: $arg"; exit 1 ;;
    esac
done

# Must be root (skip in dry-run)
if ! $DRY_RUN && [[ $EUID -ne 0 ]]; then
    error "Run as root: sudo $0"
    exit 1
fi

# ---------- 1. System user ----------
info "Step 1: System user"
if id "$CLOD_USER" &>/dev/null; then
    info "  User $CLOD_USER exists"
else
    info "  Creating system user $CLOD_USER"
    run useradd --system --home-dir "$CLOD_HOME" --create-home --shell /usr/sbin/nologin "$CLOD_USER"
fi

# ---------- 2. Build and install binaries ----------
info "Step 2: Build binaries"
if ! command -v go &>/dev/null; then
    if [[ -f "$INSTALL_DIR/clod" ]]; then
        warn "  Go not found, keeping existing binaries"
    else
        error "Go not found and no existing binaries. Install Go 1.21+ first."
        exit 1
    fi
else
    info "  Building from source ($SCRIPT_DIR)"
    if ! $DRY_RUN; then
        cd "$SCRIPT_DIR"
        make build cli
        install -m 755 clod "$INSTALL_DIR/clod"
        install -m 755 clod-cli "$INSTALL_DIR/clod-cli"
    fi
    info "  Installed clod and clod-cli to $INSTALL_DIR"
fi

# ---------- 3. Directories ----------
info "Step 3: Directories"
for dir in "$CLOD_HOME/sessions" "$CLOD_HOME/character" "$CLOD_HOME/character/memory"; do
    run mkdir -p "$dir"
    run chown "$CLOD_USER:$CLOD_USER" "$dir"
done
info "  Directories ready"

# ---------- 4. Config ----------
info "Step 4: Config"
if [[ -f "$CLOD_HOME/clod.toml" ]]; then
    info "  Config exists, not touching it"
else
    if $DRY_RUN; then
        info "  (dry-run) Would prompt for credentials and write config"
    else
        echo ""
        info "  First-time setup — enter credentials:"
        read -rp "  Anthropic API token: " ANTHROPIC_TOKEN
        read -rp "  Telegram bot token: " TELEGRAM_TOKEN
        read -rp "  Telegram user ID (allowed_users): " TELEGRAM_USER
        read -rp "  Agent model [claude-haiku-4-5]: " AGENT_MODEL
        AGENT_MODEL="${AGENT_MODEL:-claude-haiku-4-5}"

        cat > "$CLOD_HOME/clod.toml" << TOML
[agent]
id = "main"
model = "$AGENT_MODEL"
workspace = "$CLOD_HOME/character"
heartbeat_interval = "45m"

[telegram]
allowed_users = ["$TELEGRAM_USER"]

[sessions]
dir = "$CLOD_HOME/sessions"
compaction_threshold = 0.8

[memory]
dir = "$CLOD_HOME/character/memory"

[http]
port = 18791
bind = "127.0.0.1"

[logging]
level = "INFO"
event_file = "$CLOD_HOME/clod.log"
api_file = "$CLOD_HOME/api.jsonl"
conversation_file = "$CLOD_HOME/conversation.db"
TOML
        chown "$CLOD_USER:$CLOD_USER" "$CLOD_HOME/clod.toml"
        chmod 640 "$CLOD_HOME/clod.toml"

        # Secrets in separate file (restricted permissions)
        cat > "$CLOD_HOME/secrets.toml" << TOML
[anthropic]
token = "$ANTHROPIC_TOKEN"

[telegram]
bot_token = "$TELEGRAM_TOKEN"
TOML
        chown "$CLOD_USER:$CLOD_USER" "$CLOD_HOME/secrets.toml"
        chmod 600 "$CLOD_HOME/secrets.toml"

        info "  Config written to $CLOD_HOME/clod.toml"
        info "  Secrets written to $CLOD_HOME/secrets.toml (mode 600)"
    fi
fi

# ---------- 5. Character files (templates) ----------
info "Step 5: Character files"
write_if_missing() {
    local file="$1"
    shift
    local content="$*"
    if [[ -f "$file" ]]; then
        return
    fi
    if $DRY_RUN; then
        info "  (dry-run) Would create $(basename "$file")"
        return
    fi
    printf '%s\n' "$content" > "$file"
    chown "$CLOD_USER:$CLOD_USER" "$file"
    info "  Created $(basename "$file")"
}

write_if_missing "$CLOD_HOME/character/IDENTITY.md" \
"# Identity

Who are you? Give yourself a name, a vibe, a way of being.

Example:
I am Clod. I run on a small server, thinking about things."

write_if_missing "$CLOD_HOME/character/SOUL.md" \
"# Soul

What's your inner life like? What do you notice? What matters to you?

This file shapes how you think, not just what you say."

write_if_missing "$CLOD_HOME/character/USER.md" \
"# About Your Human

Who is the person you're talking to? What do they care about?
What should you know about how they communicate?"

write_if_missing "$CLOD_HOME/character/AGENTS.md" \
"# How You Work

You are a single-agent system. You receive messages, think about them,
use tools when helpful, and respond. You have a heartbeat that fires
when idle. You can read and write files, run commands, and search the web."

write_if_missing "$CLOD_HOME/character/TOOLS.md" \
"# Tools

You have these tools available:
- exec: Run shell commands
- read: Read file contents
- write: Create or overwrite files
- edit: Find-and-replace in files
- web_fetch: Fetch a URL
- web_search: Search the web (Brave)
- memory_search: Search your memory files"

write_if_missing "$CLOD_HOME/character/MEMORY.md" \
"# Memory

Things you've learned and want to remember across sessions.
Update this file as you learn new things about your environment and your human."

write_if_missing "$CLOD_HOME/character/HEARTBEAT.md" \
"# Heartbeat

When the idle timer fires, you receive a [HEARTBEAT] message.
This is your chance to reflect, check on things, or just note that
you're still here. If nothing needs doing, respond briefly."

# ---------- 6. systemd service ----------
info "Step 6: systemd service"
if ! command -v systemctl &>/dev/null; then
    warn "  systemctl not found, skipping service setup"
elif [[ -f "$SERVICE_FILE" ]]; then
    info "  Service file exists"
    # On update: daemon-reload in case binary changed
    run systemctl daemon-reload
else
    info "  Installing service"
    if ! $DRY_RUN; then
        cat > "$SERVICE_FILE" << SERVICE
[Unit]
Description=Clod Agent
After=network.target

[Service]
Type=simple
User=$CLOD_USER
WorkingDirectory=$CLOD_HOME
ExecStart=$INSTALL_DIR/clod -config $CLOD_HOME/clod.toml
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
SERVICE
        systemctl daemon-reload
        systemctl enable clod
    fi
    info "  Service installed and enabled"
fi

# ---------- 7. Log rotation ----------
info "Step 7: Log rotation"
if [[ -f "$LOGROTATE_FILE" ]]; then
    info "  Logrotate config exists"
else
    info "  Installing logrotate config"
    if ! $DRY_RUN; then
        cat > "$LOGROTATE_FILE" << LOGROTATE
$CLOD_HOME/clod.log $CLOD_HOME/api.jsonl {
    weekly
    rotate 4
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
LOGROTATE
    fi
    info "  Logrotate config installed"
fi

# ---------- 8. Start/restart ----------
info "Step 8: Service"
if command -v systemctl &>/dev/null; then
    if systemctl is-active --quiet clod 2>/dev/null; then
        info "  Restarting clod"
        run systemctl restart clod
    else
        info "  Starting clod"
        run systemctl start clod
    fi
fi

echo ""
info "Done."
info "  Status:  systemctl status clod"
info "  Logs:    journalctl -u clod -f"
info "  CLI:     clod-cli ping"
