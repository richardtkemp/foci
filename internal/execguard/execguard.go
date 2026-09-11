// Package execguard answers one question: could this process substitute the
// executable that a given command token will actually run?
//
// It exists because an auto-approve entry is only as trustworthy as the binary
// behind it. If foci can rewrite that binary, replace it, or plant a shadow
// earlier on PATH, then approving the entry approves whatever runs under that
// name at the time it runs — not the command the operator read.
//
// Two callers share this package so their answers cannot drift:
//   - internal/config, at startup, to drop entries that were never trustworthy;
//   - internal/delegator/autoapprove, at match time, because a startup verdict
//     goes stale the moment a file changes, and the agent runs continuously
//     long after startup.
//
// Substitution has three shapes, and any one is a finding:
//
//  1. The executable file itself is writable.
//  2. The directory holding it is writable — the file can be unlinked and
//     replaced, so a read-only file there is not protected.
//  3. For a bare command name, the PATH search is performed and shapes 1 and 2
//     are applied to the executable that actually wins. A writable PATH
//     directory that does NOT contain the command is deliberately not a
//     finding: that is a shadow which could be created, not one that exists.
//
// SCOPE: THE EXECUTABLE ONLY, NEVER ITS ARGUMENTS. Both callers hand this
// package the FIRST WORD of each shell segment and nothing else — see
// config.commandTokens, which splits on && || ; | and truncates each segment at
// the first space, and autoapprove.commandIsSubstitutable, which passes
// tokens[0]. So an entry rewritten as "python3 /home/foci/x/script.py *" always
// survives, because python3 is /usr/bin/python3 and script.py is an argument
// this package never sees. That restores auto-approval WITHOUT restoring the
// property the check exists to establish, and it is the tempting fix for an
// entry this guard drops — do not mistake a surviving entry for a safe one.
// Arguments are unguardable in general (`bash -c` is the limiting case), so this
// is a documented boundary, not a gap awaiting a patch.
//
// SCOPE: WHAT THE FILE IS, NEVER WHAT ITS DIRECTORY ALLOWS THROUGH A SYMLINK.
// Shape 2 tests filepath.Dir of the path it was GIVEN, and filepath.Dir does not
// follow symlinks; shape 1 uses access(2), which does. So for a symlink, this
// package tests the TARGET's permissions and the SYMLINK's directory — never the
// target's directory. A non-writable target inside a writable directory can
// still be unlinked and recreated, and this package returns false for it.
// Hardening a symlinked executable therefore has to cover both directories, not
// just the two things the check happens to look at (#1898).
package execguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Env supplies everything the check needs from the outside world. Injected so
// tests exercise the logic without depending on the host's filesystem or PATH.
type Env struct {
	CanWrite     func(path string) bool
	PathDirs     []string
	IsExecutable func(path string) bool
	HomeDir      string
}

// processCanWrite reports whether this process can write path, via a real
// access(2) check rather than a mode/owner reconstruction — the kernel's answer
// accounts for ACLs, supplementary groups and read-only mounts, all of which a
// hand-rolled permission calculation gets wrong.
func processCanWrite(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}

func processCanExecute(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
}

// Live builds the Env describing this process.
//
// PATH comes from the daemon's own environment, which is the closest available
// proxy for the PATH an agent's Bash tool will use. It is a PROXY, not the
// truth: an agent shell is initialised from the user profile and may prepend
// further directories.
//
// That divergence is NOT merely permissive-at-the-margin. A directory the agent
// searches and the daemon does not changes WHICH FILE this package judges, so it
// can report a bare name safe on the strength of a file the agent will never
// run. Measured on the live host, 2026-09-11 (#1898): ~/.shellcommon prepends
// $HOME/bin, which the unit's PATH omitted, so `mdq` resolved here to the
// root-owned /usr/local/bin/mdq while the agent ran the writable
// ~/bin/mdq wrapper that shadows it, and `mds` resolved to nothing at all —
// which is also a pass, since an unresolvable token yields no verdict.
//
// The unit's PATH (Makefile SERVICE_PATH -> deploy/foci.service.tmpl) must
// therefore mirror the agent shell's, and a new directory added to one belongs
// in the other. Verify with: diff <(systemctl show -p Environment foci) against
// the PATH an agent's Bash tool reports.
func Live() Env {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return Env{
		CanWrite:     processCanWrite,
		PathDirs:     filepath.SplitList(os.Getenv("PATH")),
		IsExecutable: processCanExecute,
		HomeDir:      home,
	}
}

// bashBuiltins are resolved by the shell itself, ahead of any PATH search, so
// no file planted on PATH can substitute one. There is nothing on disk to
// check, and treating them as shadowable is a false positive that would drop
// ordinary entries like "cd *".
var bashBuiltins = map[string]bool{
	".": true, ":": true, "[": true, "alias": true, "bg": true, "bind": true,
	"break": true, "builtin": true, "caller": true, "cd": true, "command": true,
	"compgen": true, "complete": true, "compopt": true, "continue": true,
	"declare": true, "dirs": true, "disown": true, "echo": true, "enable": true,
	"eval": true, "exec": true, "exit": true, "export": true, "false": true,
	"fc": true, "fg": true, "getopts": true, "hash": true, "help": true,
	"history": true, "jobs": true, "kill": true, "let": true, "local": true,
	"logout": true, "mapfile": true, "popd": true, "printf": true, "pushd": true,
	"pwd": true, "read": true, "readarray": true, "readonly": true,
	"return": true, "set": true, "shift": true, "shopt": true, "source": true,
	"suspend": true, "test": true, "times": true, "trap": true, "true": true,
	"type": true, "typeset": true, "ulimit": true, "umask": true, "unalias": true,
	"unset": true, "wait": true,
}

// hasGlobMeta reports whether the token is a pattern rather than a command
// name. "foci_*" names no single file, so resolving it against PATH would be
// meaningless.
func hasGlobMeta(token string) bool {
	return strings.ContainsAny(token, "*?[")
}

// Substitutable reports whether the token's executable is substitutable by
// this process, and why. A token the checker cannot resolve to a single file —
// a shell builtin, a glob pattern, a relative path with no cwd to resolve
// against — yields false: it never invents a verdict it cannot support.
func Substitutable(token string, env Env) (bool, string, string) {
	if hasGlobMeta(token) {
		return false, "", ""
	}
	if strings.HasPrefix(token, "~") {
		if env.HomeDir == "" {
			return false, "", ""
		}
		path := filepath.Join(env.HomeDir, strings.TrimPrefix(token, "~"))
		return checkResolvedPath(path, env)
	}
	if strings.Contains(token, "/") {
		if !filepath.IsAbs(token) {
			// No cwd exists at config-load time to resolve a relative path.
			return false, "", ""
		}
		return checkResolvedPath(filepath.Clean(token), env)
	}
	if bashBuiltins[token] {
		return false, "", ""
	}
	return checkBareCommand(token, env)
}

// checkResolvedPath applies shapes 1 and 2 to a known path.
func checkResolvedPath(path string, env Env) (bool, string, string) {
	if env.CanWrite(path) {
		return true, path, "the file is writable by the foci process"
	}
	dir := filepath.Dir(path)
	if env.CanWrite(dir) {
		return true, path, fmt.Sprintf("its directory %s is writable by the foci process, so the file can be replaced", dir)
	}
	return false, "", ""
}

// checkBareCommand performs the shell's own PATH search — first executable of
// that name wins — and applies the file/directory test to the winner.
//
// A writable PATH directory that does NOT contain the command is deliberately
// NOT a finding. It describes a shadow that could be created, not one that
// exists, and reporting a hypothesis as a finding drops working entries and
// trains the operator to ignore the warning. If the shadow is ever created, it
// becomes the PATH winner and this check catches it on the next startup.
func checkBareCommand(name string, env Env) (bool, string, string) {
	for _, dir := range env.PathDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if !env.IsExecutable(candidate) {
			continue
		}
		// First match wins the search; later directories are unreachable.
		return checkResolvedPath(candidate, env)
	}
	// Not found on PATH: a shell builtin, a shell function, or simply absent.
	// Nothing to substitute that we can see, so no verdict.
	return false, "", ""
}
