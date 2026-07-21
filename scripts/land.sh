#!/usr/bin/env bash
# land.sh — the sanctioned path to main (merge-lock landing, #1448 pieces 1+2).
#
# Run FROM the feature-branch worktree you want to land (via `make land`).
# Serialises every landing on this host through a per-repo MERGE lock
# (/tmp/foci-merge.lock), distinct from the /tmp/heavy COMPUTE lock that
# `make test`/`make integration`/`make deploy-build` take: merge arbitrates
# repo state, heavy arbitrates CPU. This process holds the merge lock for the
# whole landing and, in its test step, `make test` transiently ALSO takes
# /tmp/heavy — so a lander briefly holds BOTH.
#
# LOCK-ORDER INVARIANT (deadlock-safety): always merge-lock -> heavy, NEVER the
# reverse. Nothing that holds /tmp/heavy may attempt a landing. Safe today:
# only test/integration/deploy-build take heavy and none of them land.
#
# Cheap repo-state work (fetch, rebase, conflict/dirty detection) runs BEFORE
# the compute step, so merge-lock hold time is ~= the unit suite + heavy
# queueing, not a full rebuild. Pushes HEAD:main to origin (atomic remote ff);
# does NOT touch the local main checkout — deploys read origin/main directly
# (piece #4), so the local main ref is non-load-bearing.
#
# /tmp lock-file gotchas, identical to the `test` target: /tmp is world-writable
# + sticky and with fs.protected_regular=2 the kernel denies WRITE-opening a
# lock file there owned by the other shared account (rich vs foci) — so open the
# lock READ-ONLY (9<), and seed it only when missing (creator owns it, umask
# keeps it readable by all). FD 9 is closed (9<&-) before `make test` execs so
# the test process and its children do not inherit — and thus cannot pin — the
# merge lock.
set -u

LOCK=/tmp/foci-merge.lock
[ -e "$LOCK" ] || : > "$LOCK"

land() {
	echo ">>> waiting for merge lock ($LOCK; another landing may be in progress) ..." >&2
	flock 9
	echo ">>> acquired merge lock" >&2

	local branch
	branch=$(git rev-parse --abbrev-ref HEAD)
	if [ "$branch" = "main" ]; then
		echo "ABORT: refusing to land from main itself — run from your feature branch" >&2
		return 2
	fi
	if ! git diff --quiet || ! git diff --cached --quiet; then
		echo "ABORT: uncommitted changes — commit or stash before landing" >&2
		return 6
	fi

	echo ">>> fetching origin ..." >&2
	git fetch -q origin || { echo "ABORT: git fetch failed" >&2; return 1; }

	echo ">>> rebasing $branch onto origin/main ..." >&2
	if ! git rebase origin/main; then
		git rebase --abort 2>/dev/null || true
		echo "ABORT: rebase conflict onto origin/main — resolve manually, then re-run make land" >&2
		return 3
	fi

	echo ">>> running unit tests (make test) ..." >&2
	make test 9<&- || { echo "ABORT: unit tests failed — not landing" >&2; return 4; }

	echo ">>> pushing HEAD:main to origin ..." >&2
	git push origin HEAD:main || {
		echo "ABORT: push rejected (origin/main moved by a non-lander since fetch?) — re-run make land" >&2
		return 5
	}

	echo ">>> LANDED $(git rev-parse --short HEAD) to origin/main" >&2
}

# Hold the merge lock on FD 9 for the whole land() call; the subshell's exit
# (any return above, or normal completion) closes FD 9 and releases the lock.
( land ) 9<"$LOCK"
