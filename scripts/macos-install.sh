#!/usr/bin/env bash
# macos-install.sh — install or update foci on macOS as the CURRENT user.
#
# The macOS counterpart of `sudo make setup` / `sudo make update`. There is no
# separate `foci` system user, no root and no systemd: foci-gw runs as you, as a
# per-user LaunchAgent (deploy/foci.launchd.plist.tmpl). Run from the repo root,
# normally via `make install`. Safe to re-run: it rebuilds, reinstalls the
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
# foci-gw runs with HOME = your real home and FOCI_HOME = the install root (see
# the plist template, #2580): foci takes its install root from $FOCI_HOME, so
# everything foci and its agents start (codex, opencode, gh, git, ssh, Homebrew
# tools) sees your own logins and config. The Claude Code foci runs is the
# exception: foci-gw gives it its own config folder, $FOCI_HOME/.claude
# (CLAUDE_CONFIG_DIR), separate from your interactive ~/.claude. To use the foci
# CLI from your own shell, export FOCI_HOME=~/foci (e.g. in ~/.zprofile), or run
#   FOCI_HOME=~/foci ~/foci/bin/foci <command>
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

[ "$(uname -s)" = Darwin ] || die "this script is for macOS; on Linux use sudo make install"
[ "$(id -u)" -ne 0 ] || die "run as your own user, not root: foci runs as the installing user on macOS"
grep -qs '^module foci$' go.mod || die "run from the foci repo root"
command -v go >/dev/null || die "go not found — run: make macos-deps"

FOCI_HOME=${FOCI_HOME:-$HOME/foci}
# Exported for the foci binaries this script runs (first-run, -check-config):
# they take the install root from it, as foci-gw does under the LaunchAgent.
export FOCI_HOME
LABEL=${FOCI_LAUNCHD_LABEL:-com.github.richardtkemp.foci}
BIN_DIR=$FOCI_HOME/bin
PLIST=$HOME/Library/LaunchAgents/$LABEL.plist
DOMAIN=gui/$(id -u)
BREW=$(brew --prefix 2>/dev/null || echo /opt/homebrew)
BINS=(foci-gw foci foci-call foci-cc-hook foci-codex-hook)

# migrate_home_root carries an install made before #2580 over to the separate
# install root. Run only while no foci-gw is running: an old one still runs
# with HOME=$FOCI_HOME and needs both of the things this moves.
#  - Claude Code's state file: with HOME=$FOCI_HOME it was $FOCI_HOME/.claude.json;
#    with CLAUDE_CONFIG_DIR=$FOCI_HOME/.claude CC reads $FOCI_HOME/.claude/.claude.json.
#    Copied once (so a rollback still finds the old one).
#  - The $FOCI_HOME/Library/Keychains link this script used to make so Claude
#    Code found the login keychain under the fake HOME. With the real HOME
#    macOS finds it itself; the link is removed if it is ours.
migrate_home_root() {
	[ "$FOCI_HOME" != "$HOME" ] || return 0
	local root=${FOCI_HOME:?}
	if [ -f "$root/.claude.json" ] && [ ! -e "$root/.claude/.claude.json" ]; then
		say "copying Claude Code's state file to $root/.claude/.claude.json"
		mkdir -p "$root/.claude"
		cp -p "$root/.claude.json" "$root/.claude/.claude.json"
	fi
	if [ -L "$root/Library/Keychains" ] &&
		[ "$(readlink "$root/Library/Keychains")" = "$HOME/Library/Keychains" ]; then
		say "removing the old $root/Library/Keychains link"
		unlink "$root/Library/Keychains"
	fi
}

# The daemon PATH. Same shape as the Linux unit's SERVICE_PATH (FOCI_HOME/bin
# first, then system dirs), plus the user's ~/.local/bin (where Claude Code
# installs itself) and, after the system dirs, ~/.opencode/bin (where
# opencode's official installer puts it, adding it to the interactive shell
# PATH only) and Homebrew. Unlike Linux, every non-system directory here is
# writable by the user foci runs as, so internal/execguard declines to
# auto-approve a command that resolves into one (correct: the agent could
# replace that file). The SIP-protected system dirs therefore come before
# ~/.opencode/bin and Homebrew, so ls/grep/jq/sqlite3/diff/git resolve to
# files nobody can swap and their auto-approve rules keep working; Homebrew
# still supplies what macOS lacks (tmux, rg, go, node), which then asks for
# approval.
SERVICE_PATH="$BIN_DIR:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$HOME/.opencode/bin:$BREW/bin:$BREW/sbin:/usr/local/bin"

say "building ${BINS[*]}"
make --no-print-directory "${BINS[@]}"

say "installing into $FOCI_HOME"
mkdir -p "$BIN_DIR" "$FOCI_HOME/config" "$FOCI_HOME/data" "$FOCI_HOME/logs" "$FOCI_HOME/shared/docs" \
	"$HOME/Library/LaunchAgents"
chmod 700 "$FOCI_HOME/config"

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
	# First install. At a terminal, run the setup wizard here (as `sudo make
	# setup` does on Linux) and carry on; without one there is nobody to answer
	# it, so stop and say what to run.
	if [ -t 0 ] && [ -t 1 ]; then
		say "no $CONFIG yet — running the setup wizard"
		"$BIN_DIR/foci" first-run --config-dir "$FOCI_HOME/config"
		[ -f "$CONFIG" ] || die "the wizard did not write $CONFIG"
		[ -f "$SECRETS" ] && chmod 600 "$SECRETS"
	else
		say "no $CONFIG yet. Create one, then re-run this script:"
		echo "      FOCI_HOME=$FOCI_HOME $BIN_DIR/foci first-run --config-dir $FOCI_HOME/config" >&2
		exit 0
	fi
fi

say "checking config against the new binary"
# FOCI_HOME and PATH exactly as the LaunchAgent runs it: relative config paths
# resolve against FOCI_HOME, and the auto-approve veto report resolves against PATH.
PATH="$SERVICE_PATH" "$BIN_DIR/foci-gw" -check-config -config "$CONFIG"

say "rendering $PLIST"
sed -e "s|@LABEL@|$LABEL|g" -e "s|@BIN_DIR@|$BIN_DIR|g" -e "s|@FOCI_HOME@|$FOCI_HOME|g" -e "s|@HOME@|$HOME|g" \
	-e "s|@SERVICE_PATH@|$SERVICE_PATH|g" \
	deploy/foci.launchd.plist.tmpl >"$PLIST.tmp"
plutil -lint -s "$PLIST.tmp"
mv "$PLIST.tmp" "$PLIST"

if [ "${FOCI_NO_START:-}" = 1 ]; then
	launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 || migrate_home_root
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
migrate_home_root
say "starting foci ($DOMAIN/$LABEL)"
launchctl bootstrap "$DOMAIN" "$PLIST"
sleep 2
if ! launchctl print "$DOMAIN/$LABEL" | grep -q 'state = running'; then
	launchctl print "$DOMAIN/$LABEL" >&2 || true
	die "foci did not reach state=running; see $FOCI_HOME/logs/launchd.err.log"
fi
say "foci running: $(launchctl print "$DOMAIN/$LABEL" | awk '/^\tpid =/ {print "pid " $3}')"
