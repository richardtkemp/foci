package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// An auto-approve entry is only as trustworthy as the executable it will
// actually run. If the foci process can substitute that executable, then
// approving the entry approves "whatever runs under this name at run time",
// which is not what the operator thought they were approving.
//
// Startup therefore drops such entries and warns. The entry stays in the config
// file; it is ignored, not rewritten, so the operator sees a warning rather than
// silently losing a rule.
//
// Substitution has three shapes, and an entry is dropped if ANY applies:
//
//  1. The executable file itself is writable.
//  2. The directory holding it is writable — the file can be unlinked and
//     replaced, so a read-only file there is not protected.
//  3. For a bare command name, any PATH directory searched at or before the one
//     that wins is writable — a file planted there shadows the real binary.
//     Checking only the resolved winner would pass `git *` while
//     /home/foci/.local/bin/git remained plantable.
//
// Scope: Bash entries only. Read/Edit/Write entries name DATA, not code, and a
// writable data path is the normal case.

// DroppedAutoApproveRule records one entry removed by the writable-path check.
type DroppedAutoApproveRule struct {
	Rule   string // the config entry, verbatim
	Path   string // the executable (or PATH directory) that failed the check
	Reason string // human-readable cause
}

// execEnv supplies everything the checker needs from the outside world. Injected
// so tests exercise the logic without depending on the host's filesystem or PATH.
type execEnv struct {
	canWrite     func(path string) bool
	pathDirs     []string
	isExecutable func(path string) bool
	homeDir      string
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

// liveExecEnv builds the execEnv describing this process.
//
// PATH comes from the daemon's own environment, which is the closest available
// proxy for the PATH an agent's Bash tool will use. It is a PROXY, not the
// truth: an agent shell is initialised from the user profile and may prepend
// further directories. The check is therefore permissive at the margin — it can
// miss a writable directory the agent adds later, but it never invents one.
func liveExecEnv() execEnv {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return execEnv{
		canWrite:     processCanWrite,
		pathDirs:     filepath.SplitList(os.Getenv("PATH")),
		isExecutable: processCanExecute,
		homeDir:      home,
	}
}

// commandTokens extracts the command token from every segment of an entry.
// Entries are composable with && || ; | so each segment carries its own command.
func commandTokens(rule string) []string {
	toolName, pattern, found := strings.Cut(rule, ":")
	if !found || toolName != "Bash" {
		return nil
	}
	var tokens []string
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	for _, segment := range strings.Split(replacer.Replace(pattern), "\x00") {
		token := strings.TrimSpace(segment)
		if idx := strings.IndexAny(token, " \t"); idx >= 0 {
			token = token[:idx]
		}
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
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

// checkCommandToken reports whether the token's executable is substitutable by
// this process, and why. A token the checker cannot resolve to a single file —
// a shell builtin, a glob pattern, a relative path with no cwd to resolve
// against — yields false: it never invents a verdict it cannot support.
func checkCommandToken(token string, env execEnv) (bool, string, string) {
	if hasGlobMeta(token) {
		return false, "", ""
	}
	if strings.HasPrefix(token, "~") {
		if env.homeDir == "" {
			return false, "", ""
		}
		path := filepath.Join(env.homeDir, strings.TrimPrefix(token, "~"))
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
func checkResolvedPath(path string, env execEnv) (bool, string, string) {
	if env.canWrite(path) {
		return true, path, "the file is writable by the foci process"
	}
	dir := filepath.Dir(path)
	if env.canWrite(dir) {
		return true, path, fmt.Sprintf("its directory %s is writable by the foci process, so the file can be replaced", dir)
	}
	return false, "", ""
}

// checkBareCommand walks PATH in order. A writable directory encountered before
// (or containing) the winning executable means the name is substitutable, so the
// walk stops at the first hit either way.
func checkBareCommand(name string, env execEnv) (bool, string, string) {
	for _, dir := range env.pathDirs {
		if dir == "" {
			continue
		}
		if env.canWrite(dir) {
			return true, dir, fmt.Sprintf("PATH directory %s is writable by the foci process, so %q can be shadowed there", dir, name)
		}
		candidate := filepath.Join(dir, name)
		if env.isExecutable(candidate) {
			// First match wins the PATH search; every earlier directory was
			// already proven unwritable above.
			if env.canWrite(candidate) {
				return true, candidate, "the file is writable by the foci process"
			}
			return false, "", ""
		}
	}
	// Not found on PATH: a shell builtin, a shell function, or simply absent.
	// Nothing to substitute that we can see, so no verdict.
	return false, "", ""
}

// filterWritableAutoApproveRules returns the entries that survive plus those
// dropped. Pure with respect to env: no direct filesystem or environment access.
func filterWritableAutoApproveRules(rules []string, env execEnv) ([]string, []DroppedAutoApproveRule) {
	if len(rules) == 0 {
		return rules, nil
	}
	kept := make([]string, 0, len(rules))
	var dropped []DroppedAutoApproveRule
	for _, rule := range rules {
		var bad *DroppedAutoApproveRule
		for _, token := range commandTokens(rule) {
			if substitutable, path, reason := checkCommandToken(token, env); substitutable {
				bad = &DroppedAutoApproveRule{Rule: rule, Path: path, Reason: reason}
				break
			}
		}
		if bad != nil {
			dropped = append(dropped, *bad)
			continue
		}
		kept = append(kept, rule)
	}
	return kept, dropped
}

// dropWritableAutoApproveRules applies the check to the global block and to
// every agent block, warning once per dropped entry.
func (cfg *Config) dropWritableAutoApproveRules(env execEnv) {
	warn := func(scope string, dropped []DroppedAutoApproveRule) {
		for _, d := range dropped {
			configLog.Warnf("[permissions] %s: ignoring auto_approve entry %q — %s (%s). Move the executable somewhere the foci process cannot write, or remove the entry.",
				scope, d.Rule, d.Reason, d.Path)
		}
	}
	kept, dropped := filterWritableAutoApproveRules(cfg.Permissions.AutoApprove, env)
	cfg.Permissions.AutoApprove = kept
	warn("global", dropped)

	for i := range cfg.Agents {
		agentKept, agentDropped := filterWritableAutoApproveRules(cfg.Agents[i].Permissions.AutoApprove, env)
		cfg.Agents[i].Permissions.AutoApprove = agentKept
		warn(fmt.Sprintf("agent %q", cfg.Agents[i].ID), agentDropped)
	}
}
