#!/bin/bash
# Foci setup script — idempotent. Run once to install, again to update.
#
# Usage:
#   ./setup.sh                 Build + print root commands for review
#   ./setup.sh --install       Build + run root commands via sudo
#   ./setup.sh --dry-run       Show everything, execute nothing
#
# The build runs unprivileged as the invoking user. Only the install
# step (user creation, file ownership, systemd) needs root.
set -euo pipefail

INSTALL_DIR="/usr/local/bin"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DRY_RUN=false
DO_INSTALL=false
FOCI_USER=""
INSTALL_SCRIPT="$SCRIPT_DIR/.setup-install.sh"

# Colors (disabled if not a terminal)
if [[ -t 1 ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; NC=''
fi

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
error() { echo -e "${RED}[x]${NC} $*" >&2; }

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=true; shift ;;
        --install) DO_INSTALL=true; shift ;;
        -u)
            [[ $# -lt 2 ]] && { error "-u requires a username"; exit 1; }
            FOCI_USER="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: $0 [--install] [-u USER] [--dry-run]"
            echo "Builds foci from source and installs as a system service."
            echo "Idempotent — safe to re-run."
            echo ""
            echo "Modes:"
            echo "  (default)    Build binaries, then print root commands for review"
            echo "  --install    Build binaries, then run root commands via sudo"
            echo "  --dry-run    Show what would be done without doing anything"
            echo ""
            echo "Options:"
            echo "  -u USER    System user to run as (default: foci)"
            echo ""
            echo "The build step runs as your current user — no root needed."
            echo "Only the install step (user creation, systemd, file ownership)"
            echo "requires root, and you can review the commands before running them."
            echo ""
            echo "Configuration is handled by the 'foci setup' wizard, which runs"
            echo "interactively unless env vars are set for non-interactive mode:"
            echo "  FOCI_TELEGRAM_TOKEN   Telegram bot token"
            echo "  FOCI_TELEGRAM_USER    Telegram user ID for allowed_users"
            echo "  FOCI_AUTH_METHOD      Auth method: oauth, apikey, skip"
            echo "  FOCI_AUTH_TOKEN       API key (for apikey auth method)"
            echo "  FOCI_AGENT_ID         Agent ID (default: main)"
            echo ""
            echo "If env vars are not set, setup prompts interactively (requires TTY)."
            exit 0
            ;;
        *) error "Unknown flag: $1"; exit 1 ;;
    esac
done

# ============================================================
# Phase 1: Unprivileged — build binaries and probe system state
# ============================================================

# Resolve target user and home directory
FOCI_USER="${FOCI_USER:-foci}"
if command -v getent &>/dev/null && getent passwd "$FOCI_USER" &>/dev/null; then
    FOCI_HOME="$(getent passwd "$FOCI_USER" | cut -d: -f6)"
else
    FOCI_HOME="/home/$FOCI_USER"
fi

if [[ "$FOCI_USER" == foci* ]]; then
    SERVICE_NAME="$FOCI_USER"
else
    SERVICE_NAME="foci-$FOCI_USER"
fi
SERVICE_FILE="/etc/systemd/system/$SERVICE_NAME.service"
SECRETS_GROUP="foci-secrets"
COMMIT_FILE="$FOCI_HOME/data/.foci-commit"

info "Target user: $FOCI_USER (home: $FOCI_HOME)"

# ---------- Check Go ----------
info "Checking Go installation"
if ! command -v go &>/dev/null; then
    error "Go not found. Install Go 1.23+ first."
    error "Run: sudo ./prerequisites.sh"
    error "Or download from https://go.dev/dl/"
    exit 1
fi

GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
if [[ "$GO_MAJOR" -lt 1 ]] || [[ "$GO_MAJOR" -eq 1 && "$GO_MINOR" -lt 23 ]]; then
    error "Go $GO_VERSION found, but Go 1.23+ is required (go.mod declares go 1.23)."
    error "Run: sudo ./prerequisites.sh"
    error "Or download from https://go.dev/dl/"
    exit 1
fi
info "  Go $GO_VERSION — OK"

# ---------- Detect update ----------
OLD_COMMIT=""
IS_UPDATE=false
if [[ -f "$INSTALL_DIR/foci-gw" ]] || [[ -f "$INSTALL_DIR/focigw" ]]; then
    IS_UPDATE=true
    if [[ -f "$COMMIT_FILE" ]] && [[ -r "$COMMIT_FILE" ]]; then
        OLD_COMMIT="$(cat "$COMMIT_FILE" 2>/dev/null || true)"
    elif [[ -f "$FOCI_HOME/.foci-commit" ]] && [[ -r "$FOCI_HOME/.foci-commit" ]]; then
        OLD_COMMIT="$(cat "$FOCI_HOME/.foci-commit" 2>/dev/null || true)"
    fi
fi

# ---------- Build binaries ----------
info "Building binaries"

NEW_COMMIT="$(git -C "$SCRIPT_DIR" -c safe.directory="$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-X main.gitCommit=$NEW_COMMIT -X main.buildTime=$BUILD_TIME"

if ! $DRY_RUN; then
    cd "$SCRIPT_DIR"

    info "  Building all binaries..."
    make all || { error "Failed to build"; exit 1; }

    info "  Binaries built in $SCRIPT_DIR/bin/"
else
    info "  (dry-run) Would build foci-gw, foci, foci-call in $SCRIPT_DIR/bin/"
fi

# ---------- Stage changelog ----------
STAGED_WELCOME=""
if $IS_UPDATE && [[ -n "$OLD_COMMIT" ]] && [[ "$OLD_COMMIT" != "$NEW_COMMIT" ]]; then
    STAGED_WELCOME="$SCRIPT_DIR/.staged-WELCOME.md"
    if ! $DRY_RUN; then
        {
            echo "# Foci Updated"
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
        } > "$STAGED_WELCOME"
        info "  Staged changelog ($OLD_COMMIT → $NEW_COMMIT)"
    else
        info "  (dry-run) Would stage changelog ($OLD_COMMIT → $NEW_COMMIT)"
    fi
elif $IS_UPDATE; then
    info "  Update detected but no previous commit recorded — skipping changelog"
fi

# ---------- Detect system state ----------
info "Detecting system state"

CURRENT_USER="$(id -un)"
IS_SELF=false
[[ "$CURRENT_USER" == "$FOCI_USER" ]] && IS_SELF=true

NEED_USERADD=false
id "$FOCI_USER" &>/dev/null || NEED_USERADD=true

NEED_GROUPADD=false
getent group "$SECRETS_GROUP" &>/dev/null || NEED_GROUPADD=true

NEED_GROUPMEMBER=false
if $NEED_USERADD || $NEED_GROUPADD; then
    NEED_GROUPMEMBER=true
elif ! id -nG "$FOCI_USER" 2>/dev/null | grep -qw "$SECRETS_GROUP"; then
    NEED_GROUPMEMBER=true
fi

HAS_CONFIG=false
if [[ -f "$FOCI_HOME/config/foci.toml" ]] || [[ -f "$FOCI_HOME/foci.toml" ]]; then
    HAS_CONFIG=true
fi

HAS_SYSTEMCTL=false
command -v systemctl &>/dev/null && HAS_SYSTEMCTL=true

SERVICE_ACTIVE=false
$HAS_SYSTEMCTL && systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null && SERVICE_ACTIVE=true

HAS_POLKIT=false
command -v pkaction &>/dev/null && HAS_POLKIT=true

HAS_POLKIT_RULE=false
[[ -f "/etc/polkit-1/rules.d/49-${SERVICE_NAME}.rules" ]] && HAS_POLKIT_RULE=true

NEED_DIRS=false
if ! $IS_SELF; then
    for dir in "$FOCI_HOME/config" "$FOCI_HOME/data"; do
        if [[ ! -d "$dir" ]]; then
            NEED_DIRS=true
            break
        fi
    done
fi

SECRETS_FILE="$FOCI_HOME/config/secrets.toml"
NEED_SECRETS_HARDEN=false
if [[ -f "$SECRETS_FILE" ]]; then
    CURRENT_OWNER="$(stat -c '%u:%G' "$SECRETS_FILE" 2>/dev/null || true)"
    CURRENT_PERMS="$(stat -c '%a' "$SECRETS_FILE" 2>/dev/null || true)"
    if [[ "$CURRENT_OWNER" != "0:$SECRETS_GROUP" ]] || [[ "$CURRENT_PERMS" != "660" ]]; then
        NEED_SECRETS_HARDEN=true
    fi
fi

NEED_SERVICE_INSTALL=false
NEED_SERVICE_PATCH=false
if $HAS_SYSTEMCTL; then
    if [[ ! -f "$SERVICE_FILE" ]]; then
        NEED_SERVICE_INSTALL=true
    else
        grep -q "SupplementaryGroups=$SECRETS_GROUP" "$SERVICE_FILE" 2>/dev/null || NEED_SERVICE_PATCH=true
        grep -q "AmbientCapabilities=CAP_SETGID" "$SERVICE_FILE" 2>/dev/null || NEED_SERVICE_PATCH=true
        grep -q "focigw" "$SERVICE_FILE" 2>/dev/null && NEED_SERVICE_PATCH=true
    fi
fi

$IS_SELF && info "  Running as $FOCI_USER (self-mode)"
$NEED_USERADD && info "  User $FOCI_USER needs to be created"
$NEED_GROUPADD && info "  Group $SECRETS_GROUP needs to be created"
$NEED_GROUPMEMBER && info "  $FOCI_USER needs to be added to $SECRETS_GROUP"
$NEED_DIRS && info "  Directories need to be created"
$HAS_CONFIG && info "  Config exists" || info "  Config needs to be created"
$NEED_SECRETS_HARDEN && info "  secrets.toml needs hardening"
$NEED_SERVICE_INSTALL && info "  Systemd service needs to be installed"
$NEED_SERVICE_PATCH && info "  Systemd service needs patching"

# ---------- Wizard mode ----------
SETUP_WIZARD_ARGS="--config-dir \"$FOCI_HOME/config\""

TELEGRAM_TOKEN="${FOCI_TELEGRAM_TOKEN:-}"
TELEGRAM_USER="${FOCI_TELEGRAM_USER:-}"
AUTH_METHOD="${FOCI_AUTH_METHOD:-}"
AUTH_TOKEN="${FOCI_AUTH_TOKEN:-}"
AGENT_ID="${FOCI_AGENT_ID:-}"

if [[ -n "$TELEGRAM_TOKEN" && -n "$TELEGRAM_USER" ]]; then
    SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --non-interactive"
    SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --bot-token \"$TELEGRAM_TOKEN\""
    SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --user-id \"$TELEGRAM_USER\""
    [[ -n "$AUTH_METHOD" ]] && SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --auth-method \"$AUTH_METHOD\""
    [[ -n "$AUTH_TOKEN" ]] && SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --auth-token \"$AUTH_TOKEN\""
    [[ -n "$AGENT_ID" ]] && SETUP_WIZARD_ARGS="$SETUP_WIZARD_ARGS --agent-id \"$AGENT_ID\""
    WIZARD_MODE="non-interactive"
elif [[ -t 0 ]]; then
    WIZARD_MODE="interactive"
else
    WIZARD_MODE="no-tty"
fi

# Fail-fast: need config but can't get it
if ! $HAS_CONFIG && [[ "$WIZARD_MODE" == "no-tty" ]]; then
    error "No config found and stdin is not a terminal."
    error "Set credentials via environment variables and re-run:"
    error "  FOCI_TELEGRAM_TOKEN   — Telegram bot token (required)"
    error "  FOCI_TELEGRAM_USER    — Telegram user ID (required)"
    error "  FOCI_AUTH_METHOD      — Auth method: oauth, apikey, skip (default: skip)"
    error "  FOCI_AUTH_TOKEN       — API key (required if auth method is apikey)"
    error "  FOCI_AGENT_ID         — Agent ID (default: main)"
    exit 1
fi

# ---------- Self-mode: handle unprivileged work directly ----------
if $IS_SELF && ! $DRY_RUN; then
    info "Self-mode: handling directories, config, and files directly"

    # Create directories (no chown needed — we own them)
    mkdir -p "$FOCI_HOME/config" "$FOCI_HOME/data"

    # Config wizard
    if ! $HAS_CONFIG; then
        info "  Launching setup wizard..."
        eval "\"$SCRIPT_DIR/bin/foci\" setup $SETUP_WIZARD_ARGS"
        info "  Config written by foci setup"
    fi

    # Copy docs to shared directory
    mkdir -p "$FOCI_HOME/shared/docs"
    cp -r "$SCRIPT_DIR/docs/"* "$FOCI_HOME/shared/docs/"
    info "  Docs copied to $FOCI_HOME/shared/docs/"

    # Commit file + changelog
    mkdir -p "$(dirname "$COMMIT_FILE")"
    echo "$NEW_COMMIT" > "$COMMIT_FILE"
    if [[ -n "$STAGED_WELCOME" ]] && [[ -f "$STAGED_WELCOME" ]]; then
        cp "$STAGED_WELCOME" "$FOCI_HOME/data/WELCOME.md"
        rm -f "$STAGED_WELCOME"
        info "  Changelog installed"
    fi
    # Re-check secrets hardening (wizard may have just created secrets.toml)
    if ! $NEED_SECRETS_HARDEN && [[ -f "$SECRETS_FILE" ]]; then
        CURRENT_OWNER="$(stat -c '%u:%G' "$SECRETS_FILE" 2>/dev/null || true)"
        CURRENT_PERMS="$(stat -c '%a' "$SECRETS_FILE" 2>/dev/null || true)"
        if [[ "$CURRENT_OWNER" != "0:$SECRETS_GROUP" ]] || [[ "$CURRENT_PERMS" != "660" ]]; then
            NEED_SECRETS_HARDEN=true
        fi
    fi
elif $IS_SELF && $DRY_RUN; then
    info "  (dry-run) Would create directories, copy docs, run config wizard, write commit file (self-mode)"
    # Assume wizard will create secrets.toml needing hardening
    if ! $HAS_CONFIG; then
        NEED_SECRETS_HARDEN=true
    fi
fi

# ============================================================
# Phase 2: Generate minimal install script
# ============================================================

info "Generating install script"

emit()         { echo "$@" >> "$INSTALL_SCRIPT"; }
emit_comment() { echo "" >> "$INSTALL_SCRIPT"; echo "# $*" >> "$INSTALL_SCRIPT"; }

cat > "$INSTALL_SCRIPT" << 'EOF'
#!/bin/bash
set -euo pipefail
EOF

# --- User creation ---
if $NEED_USERADD; then
    emit_comment "Create system user"
    emit "useradd --system --home-dir \"$FOCI_HOME\" --create-home --shell /bin/bash \"$FOCI_USER\""
fi

# --- Group creation ---
if $NEED_GROUPADD; then
    emit_comment "Create secrets group — secrets.toml is root:foci-secrets so gateway can read/write but agent subprocesses cannot"
    emit "groupadd \"$SECRETS_GROUP\""
fi

# --- Group membership ---
if $NEED_GROUPMEMBER; then
    emit_comment "Add $FOCI_USER to $SECRETS_GROUP"
    emit "usermod -aG \"$SECRETS_GROUP\" \"$FOCI_USER\""
fi

# --- Install binaries ---
emit_comment "Install binaries"
emit "install -m 755 \"$SCRIPT_DIR/bin/foci-gw\" \"$INSTALL_DIR/foci-gw\""
emit "install -m 755 \"$SCRIPT_DIR/bin/foci\" \"$INSTALL_DIR/foci\""
emit "install -m 755 \"$SCRIPT_DIR/bin/foci-call\" \"$INSTALL_DIR/foci-call\""

# --- Directories (only if needed and not self-mode) ---
if $NEED_DIRS; then
    emit_comment "Create directories"
    emit "mkdir -p \"$FOCI_HOME/config\" \"$FOCI_HOME/data\""
    emit "chown \"$FOCI_USER:$FOCI_USER\" \"$FOCI_HOME/config\" \"$FOCI_HOME/data\""
fi

# --- Config wizard (only if no config and not self-mode) ---
if ! $HAS_CONFIG && ! $IS_SELF; then
    emit_comment "Run config wizard"
    emit "sudo -u \"$FOCI_USER\" \"$INSTALL_DIR/foci\" setup $SETUP_WIZARD_ARGS"
    # Wizard may create secrets.toml — harden it if so
    emit "[ -f \"$SECRETS_FILE\" ] && chown \"root:$SECRETS_GROUP\" \"$SECRETS_FILE\" && chmod 0660 \"$SECRETS_FILE\""
fi

# --- Secrets hardening (existing file needs re-hardening) ---
if $NEED_SECRETS_HARDEN; then
    emit_comment "Harden secrets.toml — root-owned, group-readable so gateway can access but agent subprocesses cannot"
    emit "chown \"root:$SECRETS_GROUP\" \"$SECRETS_FILE\""
    emit "chmod 0660 \"$SECRETS_FILE\""
fi

# --- Copy docs to shared directory ---
emit_comment "Copy docs to shared directory"
emit "mkdir -p \"$FOCI_HOME/shared/docs\""
emit "cp -r \"$SCRIPT_DIR/docs/\"* \"$FOCI_HOME/shared/docs/\""
if ! $IS_SELF; then
    emit "chown -R \"$FOCI_USER:$FOCI_USER\" \"$FOCI_HOME/shared/docs\""
fi

# --- Commit file + changelog (only if not self-mode) ---
if ! $IS_SELF; then
    emit_comment "Record build commit — used to generate changelog on next update"
    emit "mkdir -p \"$(dirname "$COMMIT_FILE")\""
    emit "echo \"$NEW_COMMIT\" > \"$COMMIT_FILE\""
    emit "chown \"$FOCI_USER:$FOCI_USER\" \"$COMMIT_FILE\""
    if [[ -n "$STAGED_WELCOME" ]]; then
        emit "cp \"$STAGED_WELCOME\" \"$FOCI_HOME/data/WELCOME.md\""
        emit "chown \"$FOCI_USER:$FOCI_USER\" \"$FOCI_HOME/data/WELCOME.md\""
        emit "rm -f \"$STAGED_WELCOME\""
    fi
fi

# --- Systemd service ---
if $HAS_SYSTEMCTL; then
    if $NEED_SERVICE_INSTALL; then
        SERVICE_PATH="$FOCI_HOME/.local/bin"
        for d in /usr/local/sbin /usr/local/bin /usr/sbin /usr/bin /sbin /bin; do
            [[ -d "$d" ]] && SERVICE_PATH="$SERVICE_PATH:$d"
        done
        emit_comment "Install systemd service"
        cat >> "$INSTALL_SCRIPT" << EMIT_SERVICE
cat > "$SERVICE_FILE" << 'EOF'
[Unit]
Description=Foci Agent
After=network.target

[Service]
Type=simple
User=$FOCI_USER
SupplementaryGroups=$SECRETS_GROUP
AmbientCapabilities=CAP_SETGID
WorkingDirectory=$FOCI_HOME
Environment="PATH=$SERVICE_PATH"
ExecStart=$INSTALL_DIR/foci-gw -config $FOCI_HOME/config/foci.toml
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
EMIT_SERVICE
        emit "systemctl daemon-reload"
        emit "systemctl enable \"$SERVICE_NAME\""
    elif $NEED_SERVICE_PATCH; then
        emit_comment "Patch systemd service — add secrets group, CAP_SETGID, and migrate binary name"
        if ! grep -q "SupplementaryGroups=" "$SERVICE_FILE" 2>/dev/null; then
            emit "sed -i '/^User=/a SupplementaryGroups=$SECRETS_GROUP' \"$SERVICE_FILE\""
        fi
        if ! grep -q "AmbientCapabilities=" "$SERVICE_FILE" 2>/dev/null; then
            emit "sed -i '/^SupplementaryGroups=/a AmbientCapabilities=CAP_SETGID' \"$SERVICE_FILE\""
        fi
        if grep -q "focigw" "$SERVICE_FILE" 2>/dev/null; then
            emit "sed -i 's|/usr/local/bin/focigw|$INSTALL_DIR/foci-gw|g' \"$SERVICE_FILE\""
        fi
        emit "systemctl daemon-reload"
    else
        emit_comment "Reload systemd"
        emit "systemctl daemon-reload"
    fi
fi

# --- Polkit rule ---
if $HAS_POLKIT && ! $HAS_POLKIT_RULE; then
    emit_comment "Install polkit rule — lets $FOCI_USER restart its own service without sudo"
    cat >> "$INSTALL_SCRIPT" << EMIT_POLKIT
cat > "/etc/polkit-1/rules.d/49-${SERVICE_NAME}.rules" << 'EOF'
// Allow $FOCI_USER to manage the $SERVICE_NAME.service unit without a password.
polkit.addRule(function(action, subject) {
    if (action.id === "org.freedesktop.systemd1.manage-units" &&
        action.lookup("unit") === "$SERVICE_NAME.service" &&
        subject.user === "$FOCI_USER") {
        return polkit.Result.YES;
    }
});
EOF
EMIT_POLKIT
fi

# --- Start/restart ---
if $HAS_SYSTEMCTL; then
    if $SERVICE_ACTIVE; then
        emit_comment "Restart service"
        emit "systemctl restart \"$SERVICE_NAME\" --no-block"
    else
        emit_comment "Start service"
        emit "systemctl start \"$SERVICE_NAME\""
    fi
fi

chmod +x "$INSTALL_SCRIPT"

# ============================================================
# Phase 3: Execute or display the install script
# ============================================================

# Check if install script has any real commands (beyond the 3-line header)
INSTALL_LINES=$(wc -l < "$INSTALL_SCRIPT")
if [[ "$INSTALL_LINES" -le 3 ]]; then
    info "Nothing to install — already up to date."
    rm -f "$INSTALL_SCRIPT"
elif $DRY_RUN; then
    echo ""
    info "Dry run — nothing was built or installed."
    info "Build would produce: foci-gw, foci, foci-call in $SCRIPT_DIR/bin/"
    echo ""
    info "The following install script would be generated and run as root:"
    echo ""
    echo -e "${BLUE}--- $INSTALL_SCRIPT ---${NC}"
    cat "$INSTALL_SCRIPT"
    echo -e "${BLUE}--- end ---${NC}"
    rm -f "$INSTALL_SCRIPT"
elif $DO_INSTALL; then
    echo ""
    info "Running install script via sudo..."
    echo ""
    sudo bash "$INSTALL_SCRIPT"
    rm -f "$INSTALL_SCRIPT" "$SCRIPT_DIR/.staged-WELCOME.md" 2>/dev/null || true
    echo ""
    info "Done."
    if $HAS_SYSTEMCTL; then
        info "  Status:  systemctl status $SERVICE_NAME"
        info "  Logs:    journalctl -u $SERVICE_NAME -f"
    fi
    info "  Now message your bot on Telegram — it will introduce itself."
else
    echo ""
    info "Build complete. Binaries are in $SCRIPT_DIR/bin/"
    echo ""
    info "To install, review and run the generated script:"
    echo ""
    echo -e "  ${YELLOW}# Review what will run as root:${NC}"
    echo -e "  cat $INSTALL_SCRIPT"
    echo ""
    echo -e "  ${YELLOW}# Then install:${NC}"
    echo -e "  sudo bash $INSTALL_SCRIPT"
    echo ""
    echo -e "  ${YELLOW}# Or re-run with --install to do both in one step:${NC}"
    echo -e "  $0 --install"
    echo ""
fi
