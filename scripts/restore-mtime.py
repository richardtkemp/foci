#!/usr/bin/env python3
"""restore-mtime.py [REPO] — set every tracked, unmodified file's mtime to the
time of the last commit that touched it.

Why: go test's result cache identifies each file a test READ (testdata,
foci.toml.example, ...) by size and modification time, not content. A fresh
checkout or worktree writes every file "now", so the same commit in two
checkouts gave different cache keys and nothing was shared (traced with
GODEBUG=gocachetest=1, Dick 2026-10-07). With commit-time mtimes, the same
file content has the same mtime in every checkout, so every checkout — every
Fabro run, every worktree — shares one test cache.

Correctness: a file that differs from HEAD (staged or unstaged) is left alone,
so an edit keeps its newer mtime and still invalidates. A file whose content
changes between commits gets a different commit time. Go also refuses to
cache an input file younger than 2 s, so commit-time mtimes help there too.

Directories too: go test keys a directory a test opens (a WalkDir, a
ReadDir) by the directory's own stat AND every entry's stat, mtimes included,
and a checkout's directory mtimes are "whenever a file was last created in
it". Every directory in the worktree (not .git) is set to one fixed time on
every call. That loses nothing: an added, removed or renamed entry still
changes the entry list go hashes, and a changed file still changes its own
stat.

Cheap to call before every test run: it records the HEAD it last applied in
the worktree's git dir and returns at once while HEAD is unchanged. Outside a
git checkout it does nothing.
"""
import os
import subprocess
import sys


def git(repo, *args):
    return subprocess.run(["git", "-C", repo, *args], capture_output=True, check=True).stdout


DIR_MTIME = 946684800  # 2000-01-01T00:00:00Z: any fixed value works


def fix_dirs(root):
    for dirpath, dirnames, _ in os.walk(root):
        if ".git" in dirnames:
            dirnames.remove(".git")
        try:
            os.utime(dirpath, (DIR_MTIME, DIR_MTIME), follow_symlinks=False)
        except OSError:
            pass


def main():
    repo = sys.argv[1] if len(sys.argv) > 1 else "."
    try:
        root = git(repo, "rev-parse", "--show-toplevel").decode().strip()
        head = git(root, "rev-parse", "HEAD").decode().strip()
        stamp = git(root, "rev-parse", "--git-path", "foci-restore-mtime-head").decode().strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return 0  # not a git checkout (or no git): nothing to restore
    if not os.path.isabs(stamp):
        stamp = os.path.join(root, stamp)
    fix_dirs(root)
    try:
        with open(stamp) as f:
            if f.read().strip() == head:
                return 0
    except OSError:
        pass

    tracked = {p for p in git(root, "ls-files", "-z").decode().split("\0") if p}
    dirty = {p for p in git(root, "diff", "--name-only", "-z", "HEAD").decode().split("\0") if p}
    pending = tracked - dirty

    # Newest first: the first time a path appears is its last commit. Stop as
    # soon as every pending file has a time.
    proc = subprocess.Popen(["git", "-C", root, "log", "--format=%x01%ct", "--name-only", "-z", "HEAD"],
                            stdout=subprocess.PIPE)
    ts = None
    buf = b""
    done = 0
    try:
        while pending:
            chunk = proc.stdout.read(1 << 16)
            if not chunk:
                break
            buf += chunk
            parts = buf.split(b"\0")
            buf = parts.pop()
            for part in parts:
                for tok in part.split(b"\n"):
                    if not tok:
                        continue
                    if tok.startswith(b"\x01"):
                        ts = int(tok[1:])
                        continue
                    path = tok.decode("utf-8", "surrogateescape")
                    if path in pending and ts is not None:
                        try:
                            os.utime(os.path.join(root, path), (ts, ts), follow_symlinks=False)
                            done += 1
                        except OSError:
                            pass
                        pending.discard(path)
    finally:
        proc.kill()
        proc.wait()

    try:
        with open(stamp, "w") as f:
            f.write(head + "\n")
    except OSError:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
