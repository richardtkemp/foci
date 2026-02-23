#!/bin/bash
# Clod setup script — idempotent. Run once to install, again to update.
# Usage: sudo ./setup.sh [-u USER] [--dry-run]
set -euo pipefail

INSTALL_DIR="/usr/local/bin"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DRY_RUN=false
CLOD_USER=""
SERVICE_FILE="/etc/systemd/system/clod.service"

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
while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=true; shift ;;
        -u)
            [[ $# -lt 2 ]] && { error "-u requires a username"; exit 1; }
            CLOD_USER="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: sudo $0 [-u USER] [--dry-run]"
            echo "Installs clod as a system service. Idempotent — safe to re-run."
            echo ""
            echo "Options:"
            echo "  -u USER    System user to run as (default: clod)"
            echo "  --dry-run  Show what would be done without doing it"
            echo ""
            echo "Configuration can be provided via environment variables:"
            echo "  CLOD_ANTHROPIC_TOKEN  Anthropic API token"
            echo "  CLOD_TELEGRAM_TOKEN   Telegram bot token"
            echo "  CLOD_TELEGRAM_USER    Telegram user ID for allowed_users"
            echo "  CLOD_MODEL            Agent model (default: claude-haiku-4-5)"
            echo ""
            echo "If env vars are not set, setup prompts interactively (requires TTY)."
            exit 0
            ;;
        *) error "Unknown flag: $1"; exit 1 ;;
    esac
done

# Must be root (skip in dry-run)
if ! $DRY_RUN && [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    error "Run as root: sudo $0"
    exit 1
fi

# Resolve target user and home directory
CLOD_USER="${CLOD_USER:-clod}"
if command -v getent &>/dev/null && getent passwd "$CLOD_USER" &>/dev/null; then
    CLOD_HOME="$(getent passwd "$CLOD_USER" | cut -d: -f6)"
else
    CLOD_HOME="/home/$CLOD_USER"
fi

info "Installing for user: $CLOD_USER (home: $CLOD_HOME)"

# ---------- 1. System user ----------
info "Step 1: System user"
if id "$CLOD_USER" &>/dev/null; then
    info "  User $CLOD_USER exists"
else
    info "  Creating system user $CLOD_USER"
    run useradd --system --home-dir "$CLOD_HOME" --create-home --shell /bin/bash "$CLOD_USER"
fi

# ---------- 2. Build binaries from source ----------
info "Step 2: Build binaries from source"
if ! command -v go &>/dev/null; then
    error "Go not found. Install Go 1.19+ first: https://golang.org/doc/install"
    exit 1
fi

# Capture the currently-deployed commit hash (for changelog on update)
OLD_COMMIT=""
IS_UPDATE=false
COMMIT_FILE="$CLOD_HOME/data/.clod-commit"
if [[ -f "$INSTALL_DIR/clodgw" ]]; then
    IS_UPDATE=true
    # Check new location first, fall back to legacy
    if [[ -f "$COMMIT_FILE" ]]; then
        OLD_COMMIT="$(cat "$COMMIT_FILE" 2>/dev/null || true)"
    elif [[ -f "$CLOD_HOME/.clod-commit" ]]; then
        OLD_COMMIT="$(cat "$CLOD_HOME/.clod-commit" 2>/dev/null || true)"
    fi
fi

# Ensure data dir exists early (needed for commit file and welcome file)
if ! $DRY_RUN; then
    mkdir -p "$CLOD_HOME/data"
    chown "$CLOD_USER:$CLOD_USER" "$CLOD_HOME/data" 2>/dev/null || true
fi

# Ensure Go env vars are set (sudo strips HOME and caches)
# Default to /var/cache/go — keeps build artifacts out of home dirs and repos
export GOPATH="${GOPATH:-/var/cache/go}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
export GOCACHE="${GOCACHE:-/var/cache/go-build}"
if ! $DRY_RUN; then
    mkdir -p "$GOPATH" "$GOCACHE" 2>/dev/null || true
fi
export GOFLAGS="${GOFLAGS:--buildvcs=false}"

# Build info for ldflags
NEW_COMMIT="$(git -C "$SCRIPT_DIR" -c safe.directory="$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-X main.gitCommit=$NEW_COMMIT -X main.buildTime=$BUILD_TIME"

info "  Building clodgw (gateway/main binary)..."
if ! $DRY_RUN; then
    cd "$SCRIPT_DIR"
    go build -ldflags "$LDFLAGS" -o clodgw . || { error "Failed to build clodgw"; exit 1; }
fi

info "  Building clod (CLI tool)..."
if ! $DRY_RUN; then
    cd "$SCRIPT_DIR"
    go build -ldflags "$LDFLAGS" -o clod ./cmd/clod/ || { error "Failed to build clod"; exit 1; }
fi

# Install built binaries
if ! $DRY_RUN; then
    install -m 755 "$SCRIPT_DIR/clodgw" "$INSTALL_DIR/clodgw"
    install -m 755 "$SCRIPT_DIR/clod" "$INSTALL_DIR/clod"
fi
info "  Installed clodgw and clod to $INSTALL_DIR"

# Write changelog (WELCOME.md) on update — not fresh install
if $IS_UPDATE && [[ -n "$OLD_COMMIT" ]] && [[ "$OLD_COMMIT" != "$NEW_COMMIT" ]]; then
    WELCOME_FILE="$CLOD_HOME/data/WELCOME.md"
    if ! $DRY_RUN; then
        {
            echo "# Clod Updated"
            echo ""
            echo "Updated from \`$OLD_COMMIT\` to \`$NEW_COMMIT\` on $(date -u '+%Y-%m-%d %H:%M UTC')."
            echo ""
            echo "## Changes"
            echo ""
            git -C "$SCRIPT_DIR" -c safe.directory="$SCRIPT_DIR" log --oneline "$OLD_COMMIT..$NEW_COMMIT" 2>/dev/null || echo "(could not read git log)"
            echo ""
            echo "## Instructions"
            echo ""
            echo "Tell your user what just changed. Summarise the updates above in a brief, friendly message — highlight the most impactful changes and anything they'll notice. Send it via Telegram."
        } > "$WELCOME_FILE"
        chown "$CLOD_USER:$CLOD_USER" "$WELCOME_FILE"
        info "  Wrote changelog to $WELCOME_FILE"
    else
        info "  (dry-run) Would write changelog to $CLOD_HOME/WELCOME.md"
    fi
elif $IS_UPDATE; then
    info "  Update detected but no previous commit recorded — skipping changelog"
fi

# Save current commit for next update
if ! $DRY_RUN; then
    echo "$NEW_COMMIT" > "$COMMIT_FILE"
    chown "$CLOD_USER:$CLOD_USER" "$COMMIT_FILE"
fi

# ---------- 3. Directories ----------
info "Step 3: Directories"
for dir in "$CLOD_HOME/config" "$CLOD_HOME/data" "$CLOD_HOME/data/sessions" "$CLOD_HOME/logs" "$CLOD_HOME/shared/skills" "$CLOD_HOME/character" "$CLOD_HOME/character/memory"; do
    run mkdir -p "$dir"
    run chown "$CLOD_USER:$CLOD_USER" "$dir"
done
info "  Directories ready"

# ---------- 4. Config ----------
info "Step 4: Config"
if [[ -f "$CLOD_HOME/config/clod.toml" ]] || [[ -f "$CLOD_HOME/clod.toml" ]]; then
    info "  Config exists, not touching it"
else
    # Resolve credentials: env vars → interactive prompts → error
    ANTHROPIC_TOKEN="${CLOD_ANTHROPIC_TOKEN:-}"
    TELEGRAM_TOKEN="${CLOD_TELEGRAM_TOKEN:-}"
    TELEGRAM_USER="${CLOD_TELEGRAM_USER:-}"
    AGENT_MODEL="${CLOD_MODEL:-}"

    need_prompt=false
    [[ -z "$ANTHROPIC_TOKEN" || -z "$TELEGRAM_TOKEN" || -z "$TELEGRAM_USER" ]] && need_prompt=true

    if $need_prompt; then
        if $DRY_RUN; then
            : # handled below
        elif [[ -t 0 ]]; then
            # Interactive: prompt for missing values
            echo ""
            info "  First-time setup — enter credentials (or set CLOD_ANTHROPIC_TOKEN, CLOD_TELEGRAM_TOKEN, CLOD_TELEGRAM_USER env vars):"
            [[ -z "$ANTHROPIC_TOKEN" ]] && read -rp "  Anthropic API token: " ANTHROPIC_TOKEN
            [[ -z "$TELEGRAM_TOKEN" ]] && read -rp "  Telegram bot token: " TELEGRAM_TOKEN
            [[ -z "$TELEGRAM_USER" ]]  && read -rp "  Telegram user ID (allowed_users): " TELEGRAM_USER
            [[ -z "$AGENT_MODEL" ]]    && read -rp "  Agent model [claude-haiku-4-5]: " AGENT_MODEL
        else
            # Non-interactive (no TTY): error with instructions
            error "No config found and stdin is not a terminal."
            error "Set credentials via environment variables:"
            error "  CLOD_ANTHROPIC_TOKEN  — Anthropic API token (required)"
            error "  CLOD_TELEGRAM_TOKEN   — Telegram bot token (required)"
            error "  CLOD_TELEGRAM_USER    — Telegram user ID for allowed_users (required)"
            error "  CLOD_MODEL            — Agent model (optional, default: claude-haiku-4-5)"
            error ""
            error "Example:"
            error "  sudo CLOD_ANTHROPIC_TOKEN=sk-ant-... CLOD_TELEGRAM_TOKEN=123:ABC CLOD_TELEGRAM_USER=5970082313 ./setup.sh"
            exit 1
        fi
    fi
    AGENT_MODEL="${AGENT_MODEL:-claude-haiku-4-5}"

    if $DRY_RUN; then
        if $need_prompt; then
            info "  (dry-run) Would prompt for missing credentials and write config"
        else
            info "  (dry-run) Would write config using env vars"
        fi
    else
        cat > "$CLOD_HOME/config/clod.toml" << TOML
data_dir = "$CLOD_HOME/data"

[agent]
id = "main"
model = "$AGENT_MODEL"
workspace = "$CLOD_HOME/character"
heartbeat_interval = "45m"

[telegram]
allowed_users = ["$TELEGRAM_USER"]

[sessions]
dir = "$CLOD_HOME/data/sessions"
compaction_threshold = 0.8

[memory]
dir = "$CLOD_HOME/character/memory"

[http]
port = 18791
bind = "127.0.0.1"

[logging]
level = "INFO"
event_file = "$CLOD_HOME/logs/clod.log"
api_file = "$CLOD_HOME/logs/api.jsonl"
conversation_file = "$CLOD_HOME/data/conversation.db"

[skills]
dirs = ["$CLOD_HOME/shared/skills"]

welcome_file = "$CLOD_HOME/data/WELCOME.md"
TOML
        chown "$CLOD_USER:$CLOD_USER" "$CLOD_HOME/config/clod.toml"
        chmod 640 "$CLOD_HOME/config/clod.toml"

        # Secrets in separate file (restricted permissions)
        cat > "$CLOD_HOME/config/secrets.toml" << TOML
[anthropic]
token = "$ANTHROPIC_TOKEN"

[telegram]
bot_token = "$TELEGRAM_TOKEN"
TOML
        chown "$CLOD_USER:$CLOD_USER" "$CLOD_HOME/config/secrets.toml"
        chmod 600 "$CLOD_HOME/config/secrets.toml"

        info "  Config written to $CLOD_HOME/config/clod.toml"
        info "  Secrets written to $CLOD_HOME/config/secrets.toml (mode 600)"
    fi
fi

# ---------- 5. Character files (templates — fresh install only) ----------
info "Step 5: Character files"
if [[ -f "$CLOD_HOME/config/clod.toml" ]] || [[ -f "$CLOD_HOME/clod.toml" ]]; then
    info "  Config exists — skipping character file templates (existing install)"
else
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
fi

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
Environment="PATH=$CLOD_HOME/bin:$CLOD_HOME/.local/bin:$CLOD_HOME/.cargo/bin:$CLOD_HOME/.npm-global/bin:$CLOD_HOME/.bun/bin:/usr/local/bin:/usr/bin:/bin"
ExecStart=$INSTALL_DIR/clodgw -config $CLOD_HOME/config/clod.toml
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

# ---------- 7. Polkit rule (lets clod user manage its own service) ----------
POLKIT_FILE="/etc/polkit-1/rules.d/49-clod.rules"
info "Step 7: Polkit rule"
if ! command -v pkaction &>/dev/null; then
    warn "  polkit not found — $CLOD_USER won't be able to restart clod without sudo"
elif [[ -f "$POLKIT_FILE" ]]; then
    info "  Polkit rule exists"
else
    info "  Installing polkit rule"
    if ! $DRY_RUN; then
        cat > "$POLKIT_FILE" << POLKIT
// Allow $CLOD_USER to manage the clod.service unit without a password.
polkit.addRule(function(action, subject) {
    if (action.id === "org.freedesktop.systemd1.manage-units" &&
        action.lookup("unit") === "clod.service" &&
        subject.user === "$CLOD_USER") {
        return polkit.Result.YES;
    }
});
POLKIT
    fi
    info "  $CLOD_USER can now: systemctl restart clod"
fi

# ---------- 8. Start/restart ----------
info "Step 8: Service"
if command -v systemctl &>/dev/null; then
    if systemctl is-active --quiet clod 2>/dev/null; then
        info "  Restarting clod"
        run systemctl restart clod --no-block
    else
        info "  Starting clod"
        run systemctl start clod
    fi
fi

echo ""
info "Done."
info "  Status:  systemctl status clod"
info "  Logs:    journalctl -u clod -f"
info "  CLI:     clod ping"
