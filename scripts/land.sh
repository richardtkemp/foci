#!/usr/bin/env bash
# land.sh — the ONE sanctioned path to main for foci and foci-client
# (merge-lock landing, #1448 pieces 1+2, #1496, #1748, #1902, #1943, #1973).
#
# MIRRORED IN TWO REPOS (Dick, 2026-10-28, #2583): an identical copy of this
# file lives at scripts/land.sh in BOTH /home/rich/git/foci and
# /home/rich/git/foci-client. ANY CHANGE HERE MUST BE MIRRORED IN THE OTHER
# REPO (same bytes). History: #1973 kept one shared copy at
# /home/foci/shared/scripts/land.sh; it moved into the repos so a checkout on
# a machine without /home/foci (the work Mac) can land too. Each repo's
# `make land` runs its own scripts/land.sh. The only per-repo differences
# are passed in as explicit parameters (env vars), never silently forked:
#
#   LAND_LOCK          required. Merge-lock file path, UNIQUE PER REPO so two
#                       different repos' landings never block each other
#                       (e.g. /tmp/foci-merge.lock, /tmp/foci-client-merge.lock).
#   LAND_TEST_TARGET    required. The `make` target land runs as the test gate
#                       before pushing (e.g. `test` for foci, `test-mpp` for
#                       foci-client — foci-client compiles + tests both
#                       platforms under that target, foci does not need to).
#
# Run FROM the feature-branch worktree you want to land (via `make land`, which
# passes the two vars above and nothing else repo-specific).
#
# Serialises every landing through its own MERGE lock (LAND_LOCK), separate
# from whatever COMPUTE lock(s) the repo's own LAND_TEST_TARGET may take
# internally (foci: a local /tmp/heavy lock; foci-client: a local /tmp/heavy
# lock plus a remote Mac lock for its remote-build target) — this script does
# not know or care which, it just runs `make "$LAND_TEST_TARGET"`. The merge
# lock spans the WHOLE landing including that test run, so a second lander
# blocks and then tests once against the final main (blocks, never redoes) —
# the price being that a lander briefly holds merge + a compute lock together.
# Order is invariant BY CONSTRUCTION: land is the only taker of the merge lock
# and always takes it before any compute lock, so no cycle is possible — just
# don't add a compute-lock-holding step that also lands.
#
# RED-TEST PROTOCOL (#1448 piece 5): if the test gate goes red here, it ran on
# YOUR branch rebased onto the LATEST origin/main — so a peer's shared-semantics
# change (a renamed constant, a reworded error, a changed lookup) can turn a
# cross-package test red even though your diff never touched it. Before assuming
# your change caused it, test the base: `git stash; git checkout origin/main;
# make $LAND_TEST_TARGET`. Reproducible-on-your-branch is NOT caused-by-your-
# branch; the control is the same suite on the clean base.
#
# Cheap repo-state work (fetch, rebase, conflict/dirty detection) runs BEFORE
# the compute step, so merge-lock hold time is ~= the test gate + compute-lock
# queueing, not a full rebuild. Pushes HEAD:main to origin (atomic remote ff),
# then best-effort fast-forwards the LOCAL main checkout to the just-landed
# commit so it's current immediately (not just at the next deploy). land runs in
# a feature worktree and cannot checkout/ref-update main from here — it's
# checked out (or not) in some OTHER worktree — so it ff's main IN that
# worktree, if one exists and is clean.
#
# DIVERGED vs BEHIND local main (#1973): a plain "could not ff" note used to
# cover two very different situations with one shrug. If local main is merely
# BEHIND origin/main (a strict subset), `merge --ff-only` always succeeds — so
# reaching the failure branch means local main is DIVERGED: it holds commit(s)
# origin/main does not. That matters because `sync-main` (run at deploy time)
# is ALSO --ff-only and ABORTS on divergence — so a silent skip here just
# delays the same failure to deploy time, where it's harder to diagnose. This
# script now tells BEHIND and DIVERGED apart explicitly and, on DIVERGED, names
# the extra commit(s) and runs `git cherry` against origin/main so you know
# immediately whether they're genuinely unpushed work or just a rebased twin
# already upstream under a different hash (the #1973 repro: a commit made
# directly on local main, rebased and landed by someone else as a different
# SHA, so local main technically "differs" while carrying nothing new).
#
# On a successful landing it then auto-removes the feature worktree + branch
# (from any OTHER worktree of the same repo — git refuses to remove the current
# one — see #1943 below), so a clean land needs no manual `git worktree remove`.
# This deletes the caller's cwd; `make land` does nothing after, and prints
# where to cd.
#
# CLEANUP IS NOT GATED ON MAIN BEING CHECKED OUT ANYWHERE (#1943): earlier
# versions reused the "found a worktree with main checked out" search ($mc) to
# also decide whether cleanup could run — so on a box where a peer had some
# OTHER branch checked out in the main checkout (normal weather with concurrent
# agents), land would both skip the local-main ff (correct — nothing to ff
# against) AND skip removing the just-landed worktree + branch (wrong — that
# needs any OTHER worktree of the repo, not specifically one on main). The
# leftover branch then failed `branch -d` too ("not fully merged"), because
# that check reads the STALE local main ref that land had just said it
# couldn't advance — exactly the "not fully merged" message you'd expect if
# the work genuinely weren't landed, when it was. Cleanup now finds its own
# "admin" worktree (`git worktree list`'s first entry, which is always the
# repo's primary checkout and is never the linked feature worktree being
# landed) independently of $mc, and uses `branch -D` (no merged-check, no
# stale-ref hazard) since the successful push above already proved the content
# is on origin/main.
#
# EXIT CODE IS TRUSTWORTHY (#1496, applied here for BOTH repos — #1902): this
# script's own exit code always reflects the merge result — 0 iff the push to
# origin/main succeeded, nonzero (see the `return` codes below) otherwise — and
# a final `LAND_RESULT:` line is always the last thing printed, so grepping for
# it (or just checking $?) is sufficient. Landing itself never touches this
# script's own process cwd (every command after the worktree-removal point is
# either a plain builtin or explicitly `-C`'d), so removing the caller's own
# worktree out from under it cannot flip this script's exit status. What it CAN
# do is strand *the caller's shell* (if the caller's cwd — not this script's —
# was inside the worktree, e.g. via a persisted `cd` rather than `-C`): that
# shell's own next getcwd-dependent command (plain `pwd`, relative paths, etc.)
# will fail with something like "getcwd: cannot access parent directories".
# That failure belongs to the CALLER's session, is reported by the CALLER's
# next command, and is unrelated to whether the land itself (already complete
# by then) succeeded — the fix is to `cd` (the printed admin path) before
# running anything else in that session, not to distrust this exit code.
#
# /tmp lock-file gotchas, identical to the test targets' own locks: /tmp is
# world-writable + sticky and with fs.protected_regular=2 the kernel denies
# WRITE-opening a lock file there owned by the other shared account (rich vs
# foci) — so open the lock READ-ONLY (9<), and seed it only when missing
# (creator owns it, umask keeps it readable by all). FD 9 is closed (9<&-)
# before the test target execs so the test process and its children do not
# inherit — and thus cannot pin — the merge lock.
#
# FAIL CLOSED ON THE LOCK (#2583): the merge lock is taken with `flock`, which
# macOS does not ship (Homebrew's `flock` formula provides it on the work Mac).
# A missing or failing lock tool stops the landing BEFORE anything else — it
# prints `LAND_RESULT: FAILED ...` naming the cause and exits non-zero — so a
# landing never runs the gate or pushes without having held the lock. The
# script must also run under macOS's /bin/bash 3.2 and BSD userland: keep to
# bash-3.2 syntax and POSIX tool flags. scripts/test-land.sh exercises it
# against a throwaway bare origin on either OS.
#
# Timestamped progress marker (#1748): mirrors $(SAY) in mk/say.mk so make
# output and script output interleave in ONE readable format. `+Ns` is seconds
# since this script started, which is the number that localises a stall: scan
# for the big jump between consecutive markers rather than subtracting clock
# times by hand. Filed after a ~19min build timeout could not be attributed to
# any phase, because every marker used to be a bare untimed echo.
set -u

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	cat <<'EOF' >&2
usage: LAND_LOCK=<path> LAND_TEST_TARGET=<make-target> land.sh

The sanctioned path to main for foci and foci-client. Run from the feature-
branch worktree you want to land (normally via `make land`, which sets the two
required env vars for you — see each repo's mk/deploy.mk or Makefile).

  LAND_LOCK          merge-lock file path, unique per repo
  LAND_TEST_TARGET    `make` target to run as the test gate before pushing
EOF
	exit 0
fi

if [ -z "${LAND_LOCK:-}" ] || [ -z "${LAND_TEST_TARGET:-}" ]; then
	echo "usage: LAND_LOCK=<path> LAND_TEST_TARGET=<make-target> land.sh (see -h)" >&2
	exit 2
fi

_sayt0=$(date +%s)
say() { printf '>>> [%s +%ss] %s\n' "$(date +%H:%M:%S)" "$(( $(date +%s) - _sayt0 ))" "$*"; }

LOCK="$LAND_LOCK"
if ! command -v flock >/dev/null 2>&1; then
	echo "ABORT: no flock on PATH — cannot take the merge lock (macOS: brew install flock)" >&2
	echo ">>> LAND_RESULT: FAILED no flock on PATH (merge lock unavailable)" >&2
	exit 7
fi
[ -e "$LOCK" ] || : > "$LOCK"
if [ ! -r "$LOCK" ]; then
	echo "ABORT: merge lock file $LOCK is missing or unreadable" >&2
	echo ">>> LAND_RESULT: FAILED merge lock file $LOCK unusable" >&2
	exit 7
fi

land() {
	say "waiting for merge lock ($LOCK; another landing may be in progress) ..." >&2
	if ! flock 9; then
		echo "ABORT: flock failed to take the merge lock ($LOCK) — not landing" >&2
		echo ">>> LAND_RESULT: FAILED could not take merge lock $LOCK" >&2
		return 7
	fi
	say "acquired merge lock" >&2

	local branch
	branch=$(git rev-parse --abbrev-ref HEAD)
	if [ "$branch" = "main" ]; then
		echo "ABORT: refusing to land from main itself — run from your feature branch" >&2
		echo ">>> LAND_RESULT: FAILED refusing to land from main itself" >&2
		return 2
	fi
	if ! git diff --quiet || ! git diff --cached --quiet; then
		echo "ABORT: uncommitted changes — commit or stash before landing" >&2
		echo ">>> LAND_RESULT: FAILED uncommitted changes" >&2
		return 6
	fi

	say "fetching origin ..." >&2
	git fetch -q origin || {
		echo "ABORT: git fetch failed" >&2
		echo ">>> LAND_RESULT: FAILED git fetch failed" >&2
		return 1
	}

	say "rebasing $branch onto origin/main ..." >&2
	if ! git rebase origin/main; then
		git rebase --abort 2>/dev/null || true
		echo "ABORT: rebase conflict onto origin/main — resolve manually, then re-run make land" >&2
		echo ">>> LAND_RESULT: FAILED rebase conflict onto origin/main" >&2
		return 3
	fi
	# The rebased branch's base, captured BEFORE the push: after a successful push
	# the local origin/main tracking ref moves to HEAD, so origin/main..HEAD would
	# be empty by the time the dirty-main-checkout check below needs it (#1701).
	local base
	base=$(git rev-parse origin/main)

	say "running test gate (make $LAND_TEST_TARGET) ..." >&2
	make "$LAND_TEST_TARGET" 9<&- || {
		echo "ABORT: test gate failed — not landing" >&2
		echo ">>> LAND_RESULT: FAILED test gate failed" >&2
		return 4
	}

	say "pushing HEAD:main to origin ..." >&2
	git push origin HEAD:main || {
		echo "ABORT: push rejected (origin/main moved by a non-lander since fetch?) — re-run make land" >&2
		echo ">>> LAND_RESULT: FAILED push rejected" >&2
		return 5
	}

	# Bring the LOCAL main checkout current too (best-effort — the push above is
	# the actual landing; a successful push already updated the local origin/main
	# tracking ref). Find the worktree that has main checked out and ff it there
	# (the only place ref+index+worktree move together consistently).
	local mc
	mc=$(git worktree list --porcelain | awk '/^worktree /{w=$2} /^branch refs\/heads\/main$/{print w; exit}')
	if [ -z "$mc" ]; then
		say "note: no worktree has main checked out — skipping local main ff" >&2
	elif ! git -C "$mc" diff --quiet || ! git -C "$mc" diff --cached --quiet; then
		say "note: main checkout ($mc) is dirty — skipping local main ff (deploy's sync-main will catch up). Modified there:" >&2
		git -C "$mc" status --porcelain --untracked-files=no 2>/dev/null | sed 's/^/    /' >&2
		# A path dirty in main's checkout that this branch ALSO changed is
		# near-proof an edit meant for this worktree went to the main checkout
		# instead (a relative path from the wrong cwd), so the branch's `git add`
		# of it staged nothing — silently, exit 0 — and what just landed is
		# missing that edit (#1701). This note is the only automatic signal.
		local overlap
		overlap=$(comm -12 <(git -C "$mc" diff --name-only HEAD 2>/dev/null | sort -u) \
			<(git diff --name-only "$base"..HEAD | sort -u))
		if [ -n "$overlap" ]; then
			say "WARNING: file(s) dirty in the main checkout were ALSO changed by the commit(s) just landed — an edit likely went to the MAIN checkout instead of this worktree, and the landed commit is probably INCOMPLETE. Check these against what you meant to land:" >&2
			printf '%s\n' "$overlap" | sed 's/^/    /' >&2
		fi
	else
		local ahead behind
		ahead=$(git -C "$mc" rev-list --count origin/main..main 2>/dev/null || echo 0)
		behind=$(git -C "$mc" rev-list --count main..origin/main 2>/dev/null || echo 0)
		if [ "$ahead" = 0 ] && [ "$behind" = 0 ]; then
			say "local main checkout ($mc) already matches origin/main" >&2
		elif [ "$ahead" = 0 ]; then
			# Pure BEHIND (a strict subset of origin/main) — ff-only is always safe.
			if git -C "$mc" merge --ff-only origin/main >/dev/null 2>&1; then
				say "brought local main checkout ($mc) up to date (was $behind commit(s) behind)" >&2
			else
				say "note: local main checkout ($mc) was $behind commit(s) behind but ff failed unexpectedly — skipping (deploy's sync-main will catch up)" >&2
			fi
		else
			# DIVERGED: local main holds commit(s) origin/main does not, so a plain
			# ff-only is impossible — and sync-main at deploy time (also --ff-only)
			# WILL abort on this too, so surface it loudly now rather than at deploy.
			say "WARNING: local main checkout ($mc) has DIVERGED from origin/main — $ahead commit(s) local-only, $behind commit(s) behind. Skipping local ff; deploy's sync-main is --ff-only too and WILL ABORT on this until it's resolved." >&2
			say "  local-only commits (origin/main..main):" >&2
			git -C "$mc" log --oneline origin/main..main 2>/dev/null | sed 's/^/    /' >&2
			say "  git cherry -v origin/main main  ('-' = content already upstream under a different hash, '+' = genuinely not upstream yet):" >&2
			git -C "$mc" cherry -v origin/main main 2>/dev/null | sed 's/^/    /' >&2
		fi
	fi

	local landed wt
	landed=$(git rev-parse --short HEAD)
	say "LANDED $landed to origin/main" >&2

	# Auto-clean the feature worktree now that the landing succeeded. This needs
	# ANY other worktree of the same repo to run `git worktree remove` from (git
	# refuses to remove the CURRENT worktree, and we are inside the feature one)
	# — it does NOT need main checked out anywhere (#1943: cleanup is deliberately
	# independent of $mc above, which answers a different question). The first
	# entry from `git worktree list` is always the repo's primary checkout and is
	# never the linked feature worktree being landed, so it's a stable admin
	# context regardless of what branch (if any specific one) is checked out
	# there. --force because a landed feature worktree normally carries untracked
	# artifacts (built binaries, notes-*.md, local.properties) — land already
	# refused any tracked uncommitted changes upfront (return 6), so nothing
	# committable is lost. `branch -D` (capital) force-deletes without a
	# merged-check — the successful push above already proved the content is on
	# origin/main, so there's nothing to gain from `-d`'s stale-local-ref check
	# (and #1943 showed that check can wrongly refuse using a ref this same
	# script just told you it couldn't advance).
	wt=$(git rev-parse --show-toplevel)
	local admin
	admin=$(git worktree list --porcelain | awk -v wt="$wt" '/^worktree /{w=$2} w!=wt{print w; exit}')

	# Defensively relocate THIS script's own process out of $wt before removing
	# it below (#1496) — belt-and-braces so nothing in our own process tree
	# (now, or after a future edit) ever inherits a cwd that's about to be
	# deleted. This cannot relocate the CALLER's shell (a child can't chdir an
	# ancestor) — that's the harmless caller-side artifact documented above.
	if [ -n "$admin" ] && [ "$wt" != "$admin" ] && [ -d "$admin" ]; then
		cd "$admin" 2>/dev/null || true
	fi

	if [ -z "$admin" ] || [ "$wt" = "$admin" ]; then
		say "note: no separate worktree available to run cleanup from — leaving worktree $wt in place (remove manually)" >&2
	elif git -C "$admin" worktree remove --force "$wt"; then
		git -C "$admin" branch -D "$branch" >/dev/null 2>&1 || true
		say "cleaned up worktree $wt and branch $branch" >&2
		say "NOTE: if YOUR shell's cwd was $wt (not just this script's), it is now" >&2
		say "invalid until you 'cd $admin' — a harmless side effect of this SUCCESSFUL" >&2
		say "land, not a failure (see EXIT CODE IS TRUSTWORTHY note up top)." >&2
	else
		say "note: could not auto-remove worktree $wt — remove manually (git -C $admin worktree remove --force $wt)" >&2
	fi

	echo ">>> LAND_RESULT: SUCCESS $landed" >&2
	return 0
}

# Hold the merge lock on FD 9 for the whole land() call; the subshell's exit
# (any return above, or normal completion) closes FD 9 and releases the lock.
# Capture the result explicitly and exit with it deliberately (#1496) — rather
# than falling through to whatever the last command in the script happened to
# return — so this script's own exit code is always exactly land()'s verdict.
( land ) 9<"$LOCK"
rc=$?
exit "$rc"
