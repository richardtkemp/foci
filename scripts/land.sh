#!/usr/bin/env bash
# land.sh — the sanctioned path to main (merge-lock landing, #1448 pieces 1+2).
#
# Run FROM the feature-branch worktree you want to land (via `make land`).
# Serialises every landing through a dedicated MERGE lock (/tmp/foci-merge.lock),
# separate from the /tmp/heavy COMPUTE lock. The merge lock spans the WHOLE
# landing including `make test`, so a second lander blocks and then tests once
# against the final main (blocks, never redoes) — the price being that a lander
# briefly holds merge + heavy together. Order is invariant BY CONSTRUCTION: land
# is the only taker of the merge lock and always takes it before heavy, so no
# cycle is possible — just don't add a heavy-holding step that also lands.
#
# RED-TEST PROTOCOL (#1448 piece 5): if `make test` goes red here, it ran on
# YOUR branch rebased onto the LATEST origin/main — so a peer's shared-semantics
# change (a renamed constant, a reworded error, a changed lookup) can turn a
# cross-package test red even though your diff never touched it. Before assuming
# your change caused it, test the base: `git stash; git checkout origin/main;
# make test`. Reproducible-on-your-branch is NOT caused-by-your-branch; the
# control is the same suite on the clean base. (Seen 3x in one day: a digit-
# format change and a ratelimit-transport change each reddened an unrelated
# package's over-specific assertion.)
#
# Cheap repo-state work (fetch, rebase, conflict/dirty detection) runs BEFORE
# the compute step, so merge-lock hold time is ~= the unit suite + heavy
# queueing, not a full rebuild. Pushes HEAD:main to origin (atomic remote ff),
# then best-effort fast-forwards the LOCAL main checkout to the just-landed
# commit so it's current immediately (not just at the next deploy). land runs in
# a feature worktree and cannot checkout/ref-update main from here — it's checked
# out in the main checkout, and moving a checked-out branch's ref would desync
# that worktree — so it ff's main IN its own checkout. This project never works
# on main directly, so it should be clean; if it somehow is dirty / mid-deploy /
# not on main, the ff is skipped with a note and the next deploy's sync-main
# (piece #4) catches it up. Either way origin/main is the source of truth.
#
# On a successful landing it then auto-removes the feature worktree + branch
# (from the main checkout, since git won't remove the current worktree) — so a
# clean land needs no manual `git worktree remove`. This deletes the caller's
# cwd; `make land` does nothing after, and prints where to cd.
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

	# Bring the LOCAL main checkout current too (best-effort — the push above is
	# the actual landing; a successful push already updated the local origin/main
	# tracking ref). Find the worktree that has main checked out and ff it there
	# (the only place ref+index+worktree move together consistently).
	mc=$(git worktree list --porcelain | awk '/^worktree /{w=$2} /^branch refs\/heads\/main$/{print w; exit}')
	if [ -z "$mc" ]; then
		echo ">>> note: no worktree has main checked out — skipping local main ff" >&2
	elif ! git -C "$mc" diff --quiet || ! git -C "$mc" diff --cached --quiet; then
		echo ">>> note: main checkout ($mc) is dirty — skipping local main ff (deploy's sync-main will catch up)" >&2
	elif git -C "$mc" merge --ff-only origin/main >/dev/null 2>&1; then
		echo ">>> brought local main checkout ($mc) up to date" >&2
	else
		echo ">>> note: could not ff local main checkout ($mc) — skipping (deploy's sync-main will catch up)" >&2
	fi

	local landed wt
	landed=$(git rev-parse --short HEAD)
	echo ">>> LANDED $landed to origin/main" >&2

	# Auto-clean the feature worktree now that the landing succeeded. Must run
	# from the main checkout ($mc, found above): `git worktree remove` refuses
	# the CURRENT worktree, and we are inside the feature one. --force because a
	# landed feature worktree normally carries untracked artifacts (built
	# binaries, notes-*.md, local.properties) — land already refused any tracked
	# uncommitted changes upfront (return 6), so nothing committable is lost.
	# Removing the worktree deletes THIS shell's cwd; harmless for `make land`
	# (nothing runs after), but the caller returns to a gone directory, so we
	# say where to go. Skipped if no main checkout was found (can't remove a
	# worktree from within itself).
	wt=$(git rev-parse --show-toplevel)
	if [ -z "$mc" ] || [ "$wt" = "$mc" ]; then
		echo ">>> note: no separate main checkout — leaving worktree $wt in place (remove manually)" >&2
	elif git -C "$mc" worktree remove --force "$wt"; then
		git -C "$mc" branch -D "$branch" >/dev/null 2>&1 || true
		echo ">>> cleaned up worktree $wt and branch $branch" >&2
		echo ">>> NOTE: your cwd was that worktree and is now gone — cd $mc" >&2
	else
		echo ">>> note: could not auto-remove worktree $wt — remove manually (git -C $mc worktree remove --force $wt)" >&2
	fi
}

# Hold the merge lock on FD 9 for the whole land() call; the subshell's exit
# (any return above, or normal completion) closes FD 9 and releases the lock.
( land ) 9<"$LOCK"
