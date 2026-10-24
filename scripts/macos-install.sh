#!/usr/bin/env bash
# macos-install.sh — install or update foci on macOS as the CURRENT user.
#
# The macOS counterpart of `sudo make setup` / `sudo make update`. There is no
# separate `foci` system user, no root and no systemd: foci-gw runs as you, as a
# per-user LaunchAgent (deploy/foci.launchd.plist.tmpl). Run from the repo root,
# normally via `make macos-install`. Safe to re-run: it rebuilds, reinstalls the
# binaries, docs and agent scripts, re-renders the LaunchAgent and restarts foci.
#
# Usage: scripts/macos-install.sh [-h|--help]
#
# Environment (all optional):
#   FOCI_HOME            install root           (default: ~/foci)
#   FOCI_LAUNCHD_LABEL   LaunchAgent label       (default: com.github.richardtkemp.foci)
#   FOCI_NO_START=1      install but do not (re)start the LaunchAgent
#
# Layout (mirrors the Linux /home/foci layout, see docs/INSTALL.md):
#   $FOCI_HOME/bin       foci-gw, foci, foci-call, foci-cc-hook, foci-codex-hook,
#                        and shared/scripts/* (the agent PATH dir)
#   $FOCI_HOME/config    foci.toml, secrets.toml (0600)
#   $FOCI_HOME/data, logs, shared/
#
# foci-gw runs with HOME=$FOCI_HOME (see the plist template): foci derives its
# install root from $HOME. To use the foci CLI from your own shell, run it as
#   HOME=~/foci ~/foci/bin/foci <command>
#
# What is NOT available on macOS (see docs/INSTALL.md "macOS"): the
# foci-secrets group isolation (agents run as you and can read secrets.toml),
# the nosgid preload shim, and Landlock sandboxing (llbox).
set -euo pipefail

case "${1:-}" in
-h | --help)
	sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
	exit 0
	;;
esac

die() { echo "macos-install: $*" >&2; exit 1; }
say() { echo ">>> $*" >&2; }

[ "$(uname -s)" = Darwin ] || die "this script is for macOS; on Linux use sudo make setup / update"
[ "$(id -u)" -ne 0 ] || die "run as your own user, not root: foci runs as the installing user on macOS"
grep -qs '^module foci$' go.mod || die "run from the foci repo root"
command -v go >/dev/null || die "go not found — run: make macos-deps"

FOCI_HOME=${FOCI_HOME:-$HOME/foci}
LABEL=${FOCI_LAUNCHD_LABEL:-com.github.richardtkemp.foci}
BIN_DIR=$FOCI_HOME/bin
PLIST=$HOME/Library/LaunchAgents/$LABEL.plist
DOMAIN=gui/$(id -u)
BREW=$(brew --prefix 2>/dev/null || echo /opt/homebrew)
BINS=(foci-gw foci foci-call foci-cc-hook foci-codex-hook)

# The daemon PATH. Same shape as the Linux unit's SERVICE_PATH (FOCI_HOME
# dirs first, then system dirs), plus the user's ~/.local/bin (where Claude
# Code installs itself) and Homebrew LAST. Unlike Linux, every non-system
# directory here is writable by the user foci runs as, so internal/execguard
# declines to auto-approve a command that resolves into one (correct: the agent
# could replace that file). The SIP-protected system dirs therefore come before
# Homebrew, so ls/grep/jq/sqlite3/diff/git resolve to files nobody can swap and
# their auto-approve rules keep working; Homebrew still supplies what macOS
# lacks (tmux, rg, go, node), which then asks for approval.
SERVICE_PATH="$FOCI_HOME/.local/bin:$BIN_DIR:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$BREW/bin:$BREW/sbin:/usr/local/bin"

say "building ${BINS[*]}"
make --no-print-directory "${BINS[@]}"

say "installing into $FOCI_HOME"
mkdir -p "$BIN_DIR" "$FOCI_HOME/config" "$FOCI_HOME/data" "$FOCI_HOME/logs" "$FOCI_HOME/shared/docs" \
	"$HOME/Library/LaunchAgents"
chmod 700 "$FOCI_HOME/config"

# macOS finds the login keychain through $HOME/Library/Keychains. foci-gw and
# its children run with HOME=$FOCI_HOME, so without this link `security` sees
# only the System keychain and Claude Code reports "Not logged in" (measured:
# HOME=~/foci `security list-keychains` listed System.keychain alone; with the
# link it lists login.keychain-db and `claude -p` authenticates). Only the
# keychain is shared: Claude Code still keeps its own $FOCI_HOME/.claude.
if [ "$FOCI_HOME" != "$HOME" ]; then
	mkdir -p "$FOCI_HOME/Library"
	[ -L "$FOCI_HOME/Library/Keychains" ] || ln -s "$HOME/Library/Keychains" "$FOCI_HOME/Library/Keychains"
fi
for b in "${BINS[@]}"; do
	install -m 755 "bin/$b" "$BIN_DIR/$b"
done

# shared/: first install copies everything (skills, prompts — the seed-if-missing
# files an install may customise). Later runs only refresh docs and scripts,
# exactly as `make update` does on Linux (install-docs, install-scripts).
if [ ! -e "$FOCI_HOME/shared/.installed" ]; then
	cp -R shared/. "$FOCI_HOME/shared/"
	: >"$FOCI_HOME/shared/.installed"
fi
rsync -a --delete docs/ "$FOCI_HOME/shared/docs/"
cp README.md "$FOCI_HOME/shared/docs/README.md"
for f in shared/scripts/*; do
	[ -f "$f" ] && install -m 755 "$f" "$BIN_DIR/$(basename "$f")"
done

SECRETS=$FOCI_HOME/config/secrets.toml
[ -f "$SECRETS" ] && chmod 600 "$SECRETS"

CONFIG=$FOCI_HOME/config/foci.toml
if [ ! -f "$CONFIG" ]; then
	say "no $CONFIG yet. Create one, then re-run this script:"
	echo "      $BIN_DIR/foci first-run --config-dir $FOCI_HOME/config" >&2
	exit 0
fi

say "checking config against the new binary"
# HOME and PATH exactly as the LaunchAgent runs it: relative config paths
# resolve against HOME, and the auto-approve veto report resolves against PATH.
HOME="$FOCI_HOME" PATH="$SERVICE_PATH" "$BIN_DIR/foci-gw" -check-config -config "$CONFIG"

say "rendering $PLIST"
sed -e "s|@LABEL@|$LABEL|g" -e "s|@BIN_DIR@|$BIN_DIR|g" -e "s|@FOCI_HOME@|$FOCI_HOME|g" \
	-e "s|@SERVICE_PATH@|$SERVICE_PATH|g" \
	deploy/foci.launchd.plist.tmpl >"$PLIST.tmp"
plutil -lint -s "$PLIST.tmp"
mv "$PLIST.tmp" "$PLIST"

if [ "${FOCI_NO_START:-}" = 1 ]; then
	say "FOCI_NO_START=1: installed, not started"
	exit 0
fi

# bootout + bootstrap (not kickstart alone) so a changed plist takes effect.
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
	say "stopping the running foci ($DOMAIN/$LABEL)"
	launchctl bootout "$DOMAIN/$LABEL" || true
	# bootout returns before the process has exited; wait for it to go.
	for _ in $(seq 1 50); do
		launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 || break
		sleep 1
	done
fi
say "starting foci ($DOMAIN/$LABEL)"
launchctl bootstrap "$DOMAIN" "$PLIST"
sleep 2
if ! launchctl print "$DOMAIN/$LABEL" | grep -q 'state = running'; then
	launchctl print "$DOMAIN/$LABEL" >&2 || true
	die "foci did not reach state=running; see $FOCI_HOME/logs/launchd.err.log"
fi
say "foci running: $(launchctl print "$DOMAIN/$LABEL" | awk '/^\tpid =/ {print "pid " $3}')"
