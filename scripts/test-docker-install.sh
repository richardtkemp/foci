#!/bin/bash
# Test foci installation in a clean Docker container.
#
# Usage:
#   ./scripts/test-docker-install.sh                      # Ubuntu 24.04 (default)
#   ./scripts/test-docker-install.sh debian:12             # Debian 12
#   ./scripts/test-docker-install.sh --persist             # leave container running after test
#   ./scripts/test-docker-install.sh --persist ubuntu:22.04
#
# Secrets are loaded from .env.test (gitignored). Create it with:
#
#   FOCI_TELEGRAM_TOKEN=123456789:AAF-...
#   FOCI_TELEGRAM_USER=5970082313
#   FOCI_AUTH_METHOD=skip
#
# By default the container is removed on exit. Use --persist to keep it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ENV_FILE="$REPO_DIR/.env.test"

PERSIST=false
IMAGE="ubuntu:24.04"
for arg in "$@"; do
    case "$arg" in
        --persist) PERSIST=true ;;
        *)         IMAGE="$arg" ;;
    esac
done
CONTAINER="foci-install-test-$$"

# Colors
if [[ -t 1 ]]; then
    GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
else
    GREEN=''; RED=''; YELLOW=''; NC=''
fi

pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; }
info() { echo -e "${YELLOW}[....]${NC} $*"; }

# Load secrets
if [[ ! -f "$ENV_FILE" ]]; then
    echo "Missing $ENV_FILE — create it with:"
    echo ""
    echo "  FOCI_TELEGRAM_TOKEN=123456789:AAF-..."
    echo "  FOCI_TELEGRAM_USER=your_user_id"
    echo "  FOCI_AUTH_METHOD=skip"
    echo ""
    exit 1
fi
source "$ENV_FILE"

# Validate required vars
for var in FOCI_TELEGRAM_TOKEN FOCI_TELEGRAM_USER; do
    if [[ -z "${!var:-}" ]]; then
        fail "$var not set in $ENV_FILE"
        exit 1
    fi
done

# Clean up on exit unless --persist was given
cleanup() {
    if $PERSIST; then
        return
    fi
    info "Cleaning up container $CONTAINER"
    docker rm -f "$CONTAINER" &>/dev/null || true
}
trap cleanup EXIT

# ---------- Run ----------

info "Starting container ($IMAGE)"
docker run -d --name "$CONTAINER" "$IMAGE" sleep infinity >/dev/null

run() { docker exec "$CONTAINER" bash -c "$*"; }

info "Installing system prerequisites"
run "apt-get update -qq && apt-get install -y -qq git build-essential make curl tmux jq sqlite3 2>&1 | tail -1"
pass "System packages installed"

info "Copying repo into container"
docker exec "$CONTAINER" mkdir -p /root/src
docker cp "$REPO_DIR" "$CONTAINER:/root/src/foci" 2>&1 | grep -v "sockets not supported" || true
pass "Repo copied"

info "Running setup.sh --install (this downloads Go and builds — may take a few minutes)"
run "cd /root/src/foci && \
    FOCI_TELEGRAM_TOKEN='$FOCI_TELEGRAM_TOKEN' \
    FOCI_TELEGRAM_USER='$FOCI_TELEGRAM_USER' \
    FOCI_AUTH_METHOD='${FOCI_AUTH_METHOD:-skip}' \
    FOCI_AUTH_TOKEN='${FOCI_AUTH_TOKEN:-}' \
    FOCI_AGENT_ID='${FOCI_AGENT_ID:-main}' \
    ./setup.sh -u foci --install 2>&1"
pass "setup.sh completed"

# ---------- Verify ----------

info "Verifying installation"
ERRORS=0

# Binaries installed
for bin in foci-gw foci foci-call; do
    if run "test -x /usr/local/bin/$bin"; then
        pass "Binary: /usr/local/bin/$bin"
    else
        fail "Binary missing: /usr/local/bin/$bin"
        ERRORS=$((ERRORS + 1))
    fi
done

# User created
if run "id foci >/dev/null 2>&1"; then
    pass "User: foci"
else
    fail "User foci not created"
    ERRORS=$((ERRORS + 1))
fi

# Config created
if run "test -f /home/foci/config/foci.toml"; then
    pass "Config: /home/foci/config/foci.toml"
else
    fail "Config not created"
    ERRORS=$((ERRORS + 1))
fi

# Secrets file exists and has correct permissions
if run "test -f /home/foci/config/secrets.toml"; then
    PERMS=$(run "stat -c '%a %U:%G' /home/foci/config/secrets.toml")
    if [[ "$PERMS" == "660 root:foci-secrets" ]]; then
        pass "Secrets: correct permissions ($PERMS)"
    else
        fail "Secrets: wrong permissions ($PERMS), expected 660 root:foci-secrets"
        ERRORS=$((ERRORS + 1))
    fi
else
    fail "Secrets file not created"
    ERRORS=$((ERRORS + 1))
fi

# Docs copied
if run "test -d /home/foci/shared/docs"; then
    pass "Docs: /home/foci/shared/docs/"
else
    fail "Docs not copied"
    ERRORS=$((ERRORS + 1))
fi

# Commit file
if run "test -f /home/foci/data/.foci-commit"; then
    pass "Commit file: /home/foci/data/.foci-commit"
else
    fail "Commit file not created"
    ERRORS=$((ERRORS + 1))
fi

# foci-gw started (no systemd in container, so setup.sh should start it via runuser)
sleep 2
if run "pgrep -x foci-gw >/dev/null 2>&1"; then
    pass "Process: foci-gw running"
else
    fail "foci-gw not running"
    ERRORS=$((ERRORS + 1))
fi

echo ""
if [[ $ERRORS -eq 0 ]]; then
    pass "All checks passed ($IMAGE)"
else
    fail "$ERRORS check(s) failed ($IMAGE)"
    exit 1
fi

if $PERSIST; then
    echo ""
    info "Container left running: $CONTAINER"
    info "  docker exec -it $CONTAINER bash"
    info "  docker rm -f $CONTAINER"
fi
