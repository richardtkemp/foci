#!/usr/bin/env bash
# remote-run.sh — run one make target of this checkout on a remote build host (#2525, #2526).
#
# Usage: scripts/remote-run.sh HOST DIR TARGET [VAR=value ...]
#
# Called by the remote-* targets in mk/remote.mk; not meant to be run by hand.
#   1. rsyncs the working tree to HOST:DIR (no .git, bin/, .remote.mk, .ci.mk);
#   2. runs `make TARGET VAR=value ...` there, under a forced PTY so a killed
#      local run hangs up the remote make instead of orphaning it;
#   3. tees the output to a local log under $FGW_ROOT, and copies back the run's
#      own log (the "full log: <path>" line the test targets print);
#   4. exits with the remote make's exit status (255 = ssh itself failed).
# DIR may start with ~/ : it is taken relative to the remote home.
# REMOTE_NAME (env, optional) names the host in the closing line.
set -uo pipefail

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; }
case "${1:-}" in -h|--help) usage; exit 0 ;; esac
if [ $# -lt 3 ]; then usage >&2; exit 2; fi

host=$1
dir=${2#\~/}
target=$3
shift 3

root=${FGW_ROOT:-/tmp/fgw}
mkdir -p "$root"
# One ssh connection for the whole run (foci_todo #2551): the mkdir below opens
# it, and the rsync, the remote make and the log copy reuse it instead of each
# paying a new handshake (~0.3 s apiece to the Pro). ControlPersist keeps it 60 s
# for the next run. The socket name has the local user in it: %C hashes only the
# hosts, port and remote user, so rich's and foci's runs would share one path.
mux=(-o ControlMaster=auto -o "ControlPath=$root/ssh-$(id -un)-%C" -o ControlPersist=60)
live=(-o "ConnectTimeout=${REMOTE_SSH_TIMEOUT:-5}" -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=8 "${mux[@]}")
log=$(mktemp "$root/remote-$target-XXXXXXXX.log")

qdir=$(printf %q "$dir")
if ! ssh "${live[@]}" "$host" "mkdir -p $qdir"; then
  echo ">>> remote-run: cannot reach $host (nothing ran there)" >&2
  exit 255
fi
echo ">>> remote-run: syncing $(pwd) -> $host:$dir" >&2
if ! rsync -e "ssh ${live[*]}" --timeout=300 -az --delete \
    --exclude=.git --exclude=/bin/ --exclude=.remote.mk --exclude=.ci.mk \
    ./ "$host:$dir/"; then
  echo ">>> remote-run: rsync to $host failed (nothing ran there)" >&2
  exit 255
fi

# REMOTE_PICK= : the Mac's make must never dispatch again (it has no
# .remote.mk anyway; this makes it explicit).
cmd="cd $qdir && REMOTE_PICK= make --no-print-directory $(printf '%q ' "$target" "$@")"
echo ">>> remote-run: on $host: $cmd" >&2
ssh -tt "${live[@]}" "$host" "$cmd" < /dev/null 2>&1 | tr -d '\r' | tee "$log"
status=${PIPESTATUS[0]}

remote_log=$(grep -o 'full log: [^ ]*' "$log" | tail -1 | cut -d' ' -f3-)
if [ -n "$remote_log" ]; then
  local_copy=${log%.log}.mac.log
  if rsync -e "ssh ${live[*]}" --timeout=300 -q "$host:$remote_log" "$local_copy"; then
    echo ">>> remote-run: the Mac's full log ($remote_log) copied to $local_copy" >&2
  fi
fi
echo ">>> remote-run: $target ran on ${REMOTE_NAME:-$host}, exit $status; output: $log" >&2
exit "$status"
