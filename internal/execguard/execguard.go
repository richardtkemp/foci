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
// SYMLINKS: BOTH ENDS ARE CHECKED, AND THEY ARE DIFFERENT ATTACKS (#1893).
// A symlinked command offers two independent substitutions, and until #1893
// this package tested one thing from each end and so caught neither reliably:
// access(2) follows the link (so shape 1 judged the TARGET), while filepath.Dir
// is string manipulation and does not (so shape 2 judged the LINK's directory).
// A read-only target in a writable directory therefore passed — its bytes can
// be unlinked and recreated through that directory, which is exactly the
// substitution this package exists to report. Every path is now resolved with
// Env.EvalSymlinks first, shapes 1 and 2 are applied to the RESOLVED target,
// and the link's own directory is checked as well, because a writable link
// directory lets the link be re-pointed at different bytes entirely.
//
// NOT CHECKED: intermediate directories on the resolved path. A writable
// ancestor (e.g. /home/foci for /home/foci/bin/tool) also permits substitution,
// by renaming the directory component rather than the file. Deliberately out of
// scope for #1893: on the live config it adds no finding the file/directory
// tests do not already make, and a naive access(2) walk reports every path
// under a sticky world-writable directory (/tmp, so every test fixture) as
// substitutable — distinguishing those needs the mode/owner reconstruction
// processCanWrite exists to avoid. Tracked separately rather than guessed at.
//
// Env IS A SNAPSHOT, NOT A HANDLE — DO NOT CACHE IT (#1900). PathDirs is read
// from the process environment when Live() is called, and that environment
// CHANGES after startup: cmd/foci-gw/main.go calls shellenv.Apply() from
// main(), which os.Setenv()s the operator's dotfile PATH over the unit's. A
// caller that builds an Env once — a package-level var is the trap, since Go
// runs those initialisers before main() — pins the pre-shellenv PATH forever
// and then judges a different file than the agent shell runs. Call Live() at
// check time. See autoapprove.guardEnv for the shape that does this.
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
//
// EvalSymlinks may be nil, in which case paths are used as given. That is the
// right default for the map-backed fakes in the unit tests — a set of strings
// cannot model a link target — but it also means those fakes CANNOT exercise
// symlink behaviour at all, in either direction. Symlink coverage lives in
// execguard_symlink_test.go, against a real temp filesystem (#1893).
type Env struct {
	CanWrite     func(path string) bool
	PathDirs     []string
	IsExecutable func(path string) bool
	EvalSymlinks func(path string) (string, error)
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

// Live builds the Env describing this process AT THE MOMENT IT IS CALLED.
//
// PATH is read from os.Getenv here, so the value it captures depends entirely
// on WHEN the call happens. foci-gw's PATH is written twice: the unit supplies
// one (Makefile SERVICE_PATH -> deploy/foci.service.tmpl) and shellenv.Apply()
// replaces it from the operator's dotfiles partway through main(). Tool shells
// inherit the second one. So a Live() taken before shellenv.Apply() — anything
// at package-init time, and config.Load — describes a PATH nothing will ever
// execute under.
//
// That divergence is NOT merely permissive-at-the-margin. A directory the agent
// searches and this Env does not changes WHICH FILE this package judges, so it
// can report a bare name safe on the strength of a file the agent will never
// run. Measured on the live host, 2026-09-11 (#1900): ~/.shellcommon prepends
// $HOME/scripts, which is agent-writable and sits ahead of /usr/bin, while the
// unit's PATH has no such entry. Every bare-name allowlist rule — git, gh,
// make, jq, sed, grep — resolved here to the root-owned /usr/bin copy and ran
// from an agent-authored shadow. The fix is not to keep the two PATH lists in
// sync (there are four hand-maintained copies and they were already out of
// sync); it is to call Live() at check time so there is only ever one PATH.
//
// /proc/<pid>/environ CANNOT be used to audit this. It records the environment
// at EXEC time and is never updated by os.Setenv, so it shows the unit's PATH
// no matter what shellenv installed. Two independent readings of it produced
// the same wrong diagnosis for #1900 and the agreement looked like
// corroboration. To see a running process's live PATH, read it from a child it
// spawned.
func Live() Env {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return Env{
		CanWrite:     processCanWrite,
		PathDirs:     filepath.SplitList(os.Getenv("PATH")),
		IsExecutable: processCanExecute,
		EvalSymlinks: filepath.EvalSymlinks,
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

// evalSymlinks resolves path to the file whose bytes actually execute. A nil
// resolver or a resolution error (the commonest cause being a path that does
// not exist, which every injected-Env unit test relies on) leaves the path
// untouched — never a verdict, just no extra information.
func evalSymlinks(path string, env Env) string {
	if env.EvalSymlinks == nil {
		return path
	}
	real, err := env.EvalSymlinks(path)
	if err != nil || real == "" {
		return path
	}
	return real
}

// checkResolvedPath applies shapes 1 and 2 to a known path, against the file
// the path RESOLVES to rather than the path itself, plus the symlink-specific
// third vector. See the package doc (#1893) for why all three are needed.
func checkResolvedPath(path string, env Env) (bool, string, string) {
	real := evalSymlinks(path, env)

	// Shape 1: the bytes that execute are directly writable.
	if env.CanWrite(real) {
		return true, real, "the file is writable by the foci process"
	}
	// Shape 2: the directory holding those bytes is writable, so a read-only
	// file there can still be unlinked and recreated.
	if dir := filepath.Dir(real); env.CanWrite(dir) {
		return true, real, fmt.Sprintf("its directory %s is writable by the foci process, so the file can be replaced", dir)
	}
	// Shape 3, symlinks only: the LINK's directory is writable, so the link can
	// be re-pointed at other bytes without touching the target at all.
	if real != path {
		if dir := filepath.Dir(path); env.CanWrite(dir) {
			return true, path, fmt.Sprintf("it is a symlink in %s, which is writable by the foci process, so the link can be re-pointed", dir)
		}
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
	winner, found := lookPath(name, env)
	if !found {
		// Not found on PATH: a shell function, or absent, or — the case that
		// hid #1900 for a month — present, but only on a PATH this Env does
		// not describe. Nothing to substitute that we can SEE, so no verdict.
		// Callers that can afford to be loud should ask UnresolvedBareName
		// first; a silent pass here is indistinguishable from "safe".
		return false, "", ""
	}
	return checkResolvedPath(winner, env)
}

// lookPath performs the shell's own PATH search: the first executable of that
// name wins, and later directories are unreachable.
func lookPath(name string, env Env) (string, bool) {
	for _, dir := range env.PathDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if env.IsExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// UnresolvedBareName reports whether token is a plain command name that no
// directory on env.PathDirs provides.
//
// Substitutable answers false for such a token, which is correct — it has no
// file to judge — but that false is INDISTINGUISHABLE from "checked and safe",
// and the two mean opposite things for an auto_approve entry. #1900 lived in
// that gap: `mds` was absent from the daemon's PATH, read as nothing to
// substitute, and ran from an agent-writable directory every time. A rule
// naming a command the guard cannot locate is a configuration error — either
// the entry is dead, or the guard is looking at the wrong PATH — and either way
// it deserves a warning rather than a silent approval.
//
// Tokens Substitutable deliberately declines to judge (builtins, globs, and
// anything with a path separator, which is resolved directly) are not
// "unresolved" and yield false.
func UnresolvedBareName(token string, env Env) bool {
	if token == "" || hasGlobMeta(token) || bashBuiltins[token] {
		return false
	}
	if strings.HasPrefix(token, "~") || strings.Contains(token, "/") {
		return false
	}
	_, found := lookPath(token, env)
	return !found
}
