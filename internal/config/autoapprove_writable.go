package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// An auto-approve entry that names an executable BY PATH is only as trustworthy
// as that file. If the foci process can write the file — or can write the
// directory holding it, and so replace the file — then approving the entry
// approves "whatever that path contains at run time", which is not what the
// operator thought they were approving.
//
// Startup therefore drops such entries and warns. The entry stays in the config
// file; it is ignored, not rewritten, so the operator sees a warning rather than
// silently losing a rule.
//
// Scope is deliberately narrow: only Bash entries whose command token is an
// explicit path (absolute, or ~-rooted). A bare command name resolved through
// PATH is a different hazard (a writable PATH directory can shadow any name) and
// is not addressed here.

// DroppedAutoApproveRule records one entry removed by the writable-path check.
type DroppedAutoApproveRule struct {
	Rule   string // the config entry, verbatim
	Path   string // the resolved executable path that failed the check
	Reason string // human-readable cause
}

// writablePredicate reports whether the current process can write the path.
// Injectable so tests can exercise the checker without touching the filesystem.
type writablePredicate func(path string) bool

// processCanWrite reports whether this process can write path, via a real
// access(2) check rather than a mode/owner reconstruction — the kernel's answer
// accounts for ACLs, supplementary groups and read-only mounts, all of which a
// hand-rolled permission calculation gets wrong.
func processCanWrite(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}

// executablePathIsWritable applies the strict test: the file itself writable, OR
// the directory holding it writable. The directory case matters because being
// able to write the parent means being able to unlink the file and put a
// different one in its place, which defeats a read-only file entirely.
func executablePathIsWritable(path string, canWrite writablePredicate) (bool, string) {
	if canWrite(path) {
		return true, "file is writable by the foci process"
	}
	dir := filepath.Dir(path)
	if canWrite(dir) {
		return true, fmt.Sprintf("directory %s is writable by the foci process, so the file can be replaced", dir)
	}
	return false, ""
}

// autoApproveCommandPaths extracts the path-form command token from every
// segment of an auto-approve entry. Entries are composable with && || ; so each
// segment carries its own command. Returns nil for entries that name no
// path-form command (a bare name, a non-Bash tool, an empty pattern).
func autoApproveCommandPaths(rule string) []string {
	toolName, pattern, found := strings.Cut(rule, ":")
	if !found || toolName != "Bash" {
		// Non-Bash entries (Read/Edit/Write) name DATA, not code. A writable
		// data path is the normal case and says nothing about trust.
		return nil
	}
	var paths []string
	for _, segment := range splitCommandSegments(pattern) {
		token := strings.TrimSpace(segment)
		if idx := strings.IndexAny(token, " \t"); idx >= 0 {
			token = token[:idx]
		}
		if token == "" {
			continue
		}
		resolved, ok := resolvePathToken(token)
		if ok {
			paths = append(paths, resolved)
		}
	}
	return paths
}

// splitCommandSegments splits on the shell operators an entry may compose with.
func splitCommandSegments(pattern string) []string {
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	return strings.Split(replacer.Replace(pattern), "\x00")
}

// resolvePathToken reports whether the token names an executable by path, and
// if so its absolute form. A bare name (no separator) is out of scope. A
// relative path is also out of scope: there is no cwd at config-load time to
// resolve it against, so any answer would be a guess.
func resolvePathToken(token string) (string, bool) {
	if strings.HasPrefix(token, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, strings.TrimPrefix(token, "~")), true
	}
	if !strings.Contains(token, "/") {
		return "", false
	}
	if !filepath.IsAbs(token) {
		return "", false
	}
	return filepath.Clean(token), true
}

// filterWritableAutoApproveRules returns the entries that survive the check plus
// the ones dropped. Pure: all filesystem contact goes through canWrite.
func filterWritableAutoApproveRules(rules []string, canWrite writablePredicate) ([]string, []DroppedAutoApproveRule) {
	if len(rules) == 0 {
		return rules, nil
	}
	kept := make([]string, 0, len(rules))
	var dropped []DroppedAutoApproveRule
	for _, rule := range rules {
		var bad *DroppedAutoApproveRule
		for _, path := range autoApproveCommandPaths(rule) {
			writable, reason := executablePathIsWritable(path, canWrite)
			if writable {
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
func (cfg *Config) dropWritableAutoApproveRules(canWrite writablePredicate) {
	warn := func(scope string, dropped []DroppedAutoApproveRule) {
		for _, d := range dropped {
			configLog.Warnf("[permissions] %s: ignoring auto_approve entry %q — %s (%s). Move the file somewhere the foci process cannot write, or remove the entry.",
				scope, d.Rule, d.Reason, d.Path)
		}
	}
	kept, dropped := filterWritableAutoApproveRules(cfg.Permissions.AutoApprove, canWrite)
	cfg.Permissions.AutoApprove = kept
	warn("global", dropped)

	for i := range cfg.Agents {
		agentKept, agentDropped := filterWritableAutoApproveRules(cfg.Agents[i].Permissions.AutoApprove, canWrite)
		cfg.Agents[i].Permissions.AutoApprove = agentKept
		warn(fmt.Sprintf("agent %q", cfg.Agents[i].ID), agentDropped)
	}
}
