#!/bin/bash

# Foci Prerequisites Installer
# =============================================================================
#
# Purpose: Install all system dependencies needed to build and run foci.
# Run this script first, then ./setup.sh --install will succeed.
#
# This script is distro-agnostic and idempotent (safe to re-run).
# Detects package manager and installs: git, Go 1.23+, gcc, make, curl, jq, sqlite3
#
# Usage:
#   ./prerequisites.sh                # Show what would be installed (dry-run)
#   ./prerequisites.sh --install      # Actually install dependencies
#   ./prerequisites.sh -h|--help      # Show this help
#
# Supported distros: Ubuntu/Debian, RHEL/Fedora/CentOS, Arch, Alpine, openSUSE, macOS
# =============================================================================

set -euo pipefail

# Minimum Go version required (from go.mod)
MIN_GO_VERSION="1.23"

# Default mode
DRY_RUN=true

# Colors (disabled if not a terminal)
if [[ -t 1 ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; NC=''
fi

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
error() { echo -e "${RED}[x]${NC} $*" >&2; }

show_help() {
    cat << EOF
Foci Prerequisites Installer

This script installs all system dependencies needed to build and run foci.
Run this script first, then ./setup.sh --install will succeed.

USAGE:
    ./prerequisites.sh                Show what would be installed (dry-run)
    ./prerequisites.sh --install      Actually install dependencies
    ./prerequisites.sh -h|--help      Show this help

MODES:
    (default)    Show what would be installed without doing anything
    --install    Actually install the dependencies (requires root/sudo)

DEPENDENCIES INSTALLED:
    - git (version control)
    - Go ${MIN_GO_VERSION}+ (programming language - downloaded from go.dev if needed)
    - gcc/build-essential (C compiler for CGO/sqlite)
    - make (build tool)
    - curl (HTTP client)
    - jq (JSON processor)
    - sqlite3 (database)

SUPPORTED SYSTEMS:
    - Ubuntu/Debian (apt)
    - RHEL/Fedora/CentOS (dnf/yum)
    - Arch Linux (pacman)
    - Alpine Linux (apk)
    - openSUSE (zypper)
    - macOS (brew)

The script is idempotent and safe to re-run.
EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --install) DRY_RUN=false; shift ;;
        -h|--help) show_help; exit 0 ;;
        *) error "Unknown option: $1"; echo "Use -h for help"; exit 1 ;;
    esac
done

# Check if running with appropriate privileges (only for actual installation)
if [[ "$DRY_RUN" == "false" && "$OSTYPE" != "darwin"* ]]; then
    if [[ $EUID -ne 0 ]]; then
        error "Installation mode requires root privileges (use sudo)"
        error "Example: sudo ./prerequisites.sh --install"
        exit 1
    fi
fi

info "Foci Prerequisites Installer"
info "Detecting system and package manager..."

# Detect OS and package manager
DISTRO=""
PKG_MANAGER=""
INSTALL_CMD=""
UPDATE_CMD=""

if command -v apt-get &>/dev/null; then
    DISTRO="Debian/Ubuntu"
    PKG_MANAGER="apt"
    UPDATE_CMD="apt-get update"
    INSTALL_CMD="apt-get install -y"
elif command -v dnf &>/dev/null; then
    DISTRO="RHEL/Fedora"
    PKG_MANAGER="dnf"
    UPDATE_CMD="dnf check-update || true"
    INSTALL_CMD="dnf install -y"
elif command -v yum &>/dev/null; then
    DISTRO="RHEL/CentOS"
    PKG_MANAGER="yum"
    UPDATE_CMD="yum check-update || true"
    INSTALL_CMD="yum install -y"
elif command -v pacman &>/dev/null; then
    DISTRO="Arch Linux"
    PKG_MANAGER="pacman"
    UPDATE_CMD="pacman -Sy"
    INSTALL_CMD="pacman -S --noconfirm"
elif command -v apk &>/dev/null; then
    DISTRO="Alpine Linux"
    PKG_MANAGER="apk"
    UPDATE_CMD="apk update"
    INSTALL_CMD="apk add"
elif command -v zypper &>/dev/null; then
    DISTRO="openSUSE"
    PKG_MANAGER="zypper"
    UPDATE_CMD="zypper refresh"
    INSTALL_CMD="zypper install -y"
elif command -v brew &>/dev/null; then
    DISTRO="macOS"
    PKG_MANAGER="brew"
    UPDATE_CMD="brew update"
    INSTALL_CMD="brew install"
else
    error "Unsupported package manager. This script supports:"
    error "  apt (Debian/Ubuntu), dnf/yum (RHEL/Fedora), pacman (Arch)"
    error "  apk (Alpine), zypper (openSUSE), brew (macOS)"
    exit 1
fi

info "Detected: $DISTRO ($PKG_MANAGER)"

# Update package cache
if [[ "$DRY_RUN" == "false" ]]; then
    info "Updating package cache..."
    eval "$UPDATE_CMD"
else
    info "Would update package cache with: $UPDATE_CMD"
fi

# Define package names by distro
declare -A PACKAGES
case "$PKG_MANAGER" in
    apt)
        PACKAGES=(
            [git]="git"
            [gcc]="build-essential"
            [make]="make"
            [curl]="curl"
            [jq]="jq"
            [sqlite]="sqlite3"
        )
        ;;
    dnf|yum)
        PACKAGES=(
            [git]="git"
            [gcc]="gcc gcc-c++ make"
            [make]="make"
            [curl]="curl"
            [jq]="jq"
            [sqlite]="sqlite"
        )
        ;;
    pacman)
        PACKAGES=(
            [git]="git"
            [gcc]="base-devel"
            [make]="make"
            [curl]="curl"
            [jq]="jq"
            [sqlite]="sqlite"
        )
        ;;
    apk)
        PACKAGES=(
            [git]="git"
            [gcc]="build-base"
            [make]="make"
            [curl]="curl"
            [jq]="jq"
            [sqlite]="sqlite"
        )
        ;;
    zypper)
        PACKAGES=(
            [git]="git"
            [gcc]="gcc gcc-c++ make"
            [make]="make"
            [curl]="curl"
            [jq]="jq"
            [sqlite]="sqlite3"
        )
        ;;
    brew)
        PACKAGES=(
            [git]="git"
            [gcc]=""  # macOS has clang by default
            [make]="make"
            [curl]=""  # macOS has curl by default
            [jq]="jq"
            [sqlite]="sqlite3"
        )
        ;;
esac

# Install system packages
info "Installing system packages..."

for pkg_type in git gcc make curl jq sqlite; do
    pkg_name="${PACKAGES[$pkg_type]}"

    if [[ -z "$pkg_name" ]]; then
        info "  $pkg_type: already available (system default)"
        continue
    fi

    # Check if package is already installed (only check in install mode to avoid errors)
    ALREADY_INSTALLED=false
    if [[ "$DRY_RUN" == "false" ]]; then
        case "$PKG_MANAGER" in
            apt)
                if dpkg -l | grep -q "^ii.*$pkg_name" 2>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
            dnf|yum)
                if rpm -qa | grep -q "$pkg_name" 2>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
            pacman)
                if pacman -Qs "$pkg_name" &>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
            apk)
                if apk info -e "$pkg_name" &>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
            zypper)
                if rpm -qa | grep -q "$pkg_name" 2>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
            brew)
                if brew list "$pkg_name" &>/dev/null; then
                    ALREADY_INSTALLED=true
                fi
                ;;
        esac
    fi

    if [[ "$ALREADY_INSTALLED" == "true" ]]; then
        info "  $pkg_name: already installed"
        continue
    fi

    if [[ "$DRY_RUN" == "false" ]]; then
        info "  Installing $pkg_name..."
        eval "$INSTALL_CMD $pkg_name"
    else
        info "  Would install $pkg_name with: $INSTALL_CMD $pkg_name"
    fi
done

# Install or upgrade Go
info "Checking Go installation..."

GO_INSTALLED=false
GO_VERSION=""
NEED_GO_INSTALL=true

if command -v go &>/dev/null; then
    GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+' || echo "unknown")
    GO_INSTALLED=true

    if [[ "$GO_VERSION" != "unknown" ]]; then
        # Compare versions (simple comparison works for X.Y format)
        if awk "BEGIN {exit !($GO_VERSION >= $MIN_GO_VERSION)}"; then
            info "  Go $GO_VERSION: meets requirement (>= $MIN_GO_VERSION)"
            NEED_GO_INSTALL=false
        else
            warn "  Go $GO_VERSION: too old (need >= $MIN_GO_VERSION)"
            warn "  Will download and install Go $MIN_GO_VERSION"
        fi
    else
        warn "  Go version detection failed, will reinstall"
    fi
else
    info "  Go not found, will install Go $MIN_GO_VERSION"
fi

if [[ "$NEED_GO_INSTALL" == "true" ]]; then
    # Determine architecture
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64) GO_ARCH="amd64" ;;
        aarch64|arm64) GO_ARCH="arm64" ;;
        armv6l) GO_ARCH="armv6l" ;;
        armv7l) GO_ARCH="armv7l" ;;
        i386|i686) GO_ARCH="386" ;;
        *) error "Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    # Determine OS
    OS_NAME="linux"
    if [[ "$OSTYPE" == "darwin"* ]]; then
        OS_NAME="darwin"
    fi

    # Download and install Go
    GO_VERSION_FULL="${MIN_GO_VERSION}.0"  # Convert 1.23 to 1.23.0
    GO_TARBALL="go${GO_VERSION_FULL}.${OS_NAME}-${GO_ARCH}.tar.gz"
    GO_URL="https://go.dev/dl/$GO_TARBALL"

    if [[ "$DRY_RUN" == "false" ]]; then
        info "Installing Go $MIN_GO_VERSION from go.dev..."

        info "  Downloading $GO_URL..."
        curl -L "$GO_URL" -o "/tmp/$GO_TARBALL"

        # Remove old Go installation
        if [[ -d "/usr/local/go" ]]; then
            info "  Removing old Go installation..."
            rm -rf /usr/local/go
        fi

        # Extract new Go
        info "  Extracting Go to /usr/local/go..."
        tar -C /usr/local -xzf "/tmp/$GO_TARBALL"
        rm "/tmp/$GO_TARBALL"

        # Add Go to PATH for all users
        if [[ ! -f "/etc/profile.d/go.sh" ]]; then
            info "  Adding Go to system PATH..."
            cat > /etc/profile.d/go.sh << 'EOF'
# Add Go to PATH
if [[ ":$PATH:" != *":/usr/local/go/bin:"* ]]; then
    export PATH="/usr/local/go/bin:$PATH"
fi
EOF
            chmod +x /etc/profile.d/go.sh
        fi

        # Add to current session
        export PATH="/usr/local/go/bin:$PATH"

        info "  Go installed successfully"
    else
        info "Would install Go $MIN_GO_VERSION:"
        info "  Download: $GO_URL"
        info "  Install to: /usr/local/go"
        info "  Add to system PATH: /etc/profile.d/go.sh"
    fi
fi

if [[ "$DRY_RUN" == "false" ]]; then
    # Verify all tools are present and show versions
    info "Verifying installed tools..."

    echo ""
    echo -e "${BLUE}=== Tool Versions ===${NC}"

    # Check git
    if command -v git &>/dev/null; then
        echo "✓ git: $(git --version | head -n1)"
    else
        error "git not found after installation"
        exit 1
    fi

    # Check Go
    if command -v go &>/dev/null; then
        echo "✓ Go: $(go version | awk '{print $3, $4}')"
    else
        error "Go not found after installation"
        exit 1
    fi

    # Check gcc
    if command -v gcc &>/dev/null; then
        echo "✓ gcc: $(gcc --version | head -n1 | awk '{print $1, $NF}')"
    elif command -v clang &>/dev/null; then
        echo "✓ clang: $(clang --version | head -n1 | awk '{print $1, $4}')"
    else
        error "gcc/clang not found after installation"
        exit 1
    fi

    # Check make
    if command -v make &>/dev/null; then
        echo "✓ make: $(make --version | head -n1)"
    else
        error "make not found after installation"
        exit 1
    fi

    # Check curl
    if command -v curl &>/dev/null; then
        echo "✓ curl: $(curl --version | head -n1 | awk '{print $1, $2}')"
    else
        error "curl not found after installation"
        exit 1
    fi

    # Check jq
    if command -v jq &>/dev/null; then
        echo "✓ jq: $(jq --version)"
    else
        error "jq not found after installation"
        exit 1
    fi

    # Check sqlite3
    if command -v sqlite3 &>/dev/null; then
        echo "✓ sqlite3: $(sqlite3 --version | awk '{print $1}')"
    else
        error "sqlite3 not found after installation"
        exit 1
    fi

    echo ""
    info "All prerequisites installed successfully!"
    echo ""
    echo -e "${GREEN}Prerequisites installed. Now run: ${YELLOW}./setup.sh --install${NC}"
else
    echo ""
    info "Dry run complete — nothing was installed."
    echo ""
    echo -e "${YELLOW}To actually install these dependencies, run:${NC}"
    echo -e "  ${GREEN}sudo ./prerequisites.sh --install${NC}"
    echo ""
    echo -e "${YELLOW}Then proceed with:${NC}"
    echo -e "  ${GREEN}./setup.sh --install${NC}"
fi