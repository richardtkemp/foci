package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"foci/internal/execguard"
	"foci/internal/procx"
)

// This file is the ALARM for #1914's flip.
//
// Splitting one environment into two populations has exactly two failure
// modes and BOTH ARE SILENT:
//
//   - a tool shell wrongly marked TRUSTED → agents quietly lose tools they
//     had, and it surfaces days later as an unrelated-looking failure;
//   - a daemon site wrongly marked OPERATOR → the hole simply persists, and
//     every test still passes.
//
// Neither has a natural alarm, so the flip has to build one. reportPopulations
// makes the split ANNOUNCE what it changed — at startup, in the log, in terms
// an operator can act on — rather than leaving it to be inferred later from a
// symptom. If you are reading this because something stopped resolving, the
// two PATH lines below are the first thing to look at.

// trustedPathExceptions names the directories that are allowed to be on the
// daemon's trusted PATH while still being writable by the foci process, with
// the reason. The invariant that defines #1914 as done is:
//
//	"No directory on the daemon's trusted PATH is writable by the foci process."
//
// It FAILS on ~/.local/bin, and that is the correct outcome rather than a bug
// to rearrange away: foci-gw execs `claude` by bare name, claude lives in
// ~/.local/bin, and Claude Code's own updater must be able to rewrite it.
// gcalcli is the same shape. The exception is therefore made EXPLICIT and
// REPORTED — a named entry the assertion prints every boot — instead of being
// smuggled in via a bare name and a PATH entry that looks like all the others.
//
// Keys are matched against the directory as it appears on PATH, with a leading
// "~/" expanded against the service user's home.
var trustedPathExceptions = map[string]string{
	"~/.local/bin": "Claude Code's updater rewrites ~/.local/bin/claude, and gcalcli lives here too; " +
		"foci execs both by bare name, so this directory cannot be made unwritable without breaking self-update",
}

// pathFinding is one directory on the trusted PATH, judged against the
// invariant.
type pathFinding struct {
	Dir       string
	Writable  bool
	Exception string // non-empty when Writable is an allowed, named exception
}

// checkTrustedPathFooting applies the invariant to each trusted PATH
// directory. canWrite is execguard's own CanWrite primitive pointed at foci
// itself — the guard checking its own footing — so ACLs, supplementary groups
// and read-only mounts are accounted for by the kernel rather than by a
// mode-bit reconstruction.
func checkTrustedPathFooting(dirs []string, home string, canWrite func(string) bool) []pathFinding {
	exceptions := make(map[string]string, len(trustedPathExceptions))
	for k, v := range trustedPathExceptions {
		key := k
		if home != "" && strings.HasPrefix(key, "~/") {
			key = filepath.Join(home, key[2:])
		}
		exceptions[filepath.Clean(key)] = v
	}

	findings := make([]pathFinding, 0, len(dirs))
	for _, dir := range dedupe(dirs) {
		f := pathFinding{Dir: dir, Writable: canWrite(dir)}
		if f.Writable {
			f.Exception = exceptions[filepath.Clean(dir)]
		}
		findings = append(findings, f)
	}
	return findings
}

// dedupe removes repeated PATH entries while preserving order. The operator's
// dotfiles routinely prepend a directory that is already present, and a
// repeated entry in the report is noise that makes the real diff harder to see.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// pathDelta returns the directories present for one population and not the
// other, order preserved.
func pathDelta(have, lack []string) []string {
	out := make([]string, 0)
	for _, d := range dedupe(have) {
		if !slices.Contains(lack, d) {
			out = append(out, d)
		}
	}
	return out
}

// formatList renders a directory list for a log line, or "(none)" when empty —
// so "nothing differs" is stated rather than shown as a blank the reader has
// to interpret.
func formatList(in []string) string {
	if len(in) == 0 {
		return "(none)"
	}
	return strings.Join(in, " ")
}

// reportPopulations logs the two spawn populations and the difference between
// them, once, at startup.
//
// It logs at INFO even when nothing differs: the value of this report is that
// it is ALWAYS there, so "the operator population is empty" is a visible state
// rather than an absent line. The writability findings escalate — an
// unexplained writable directory on the trusted PATH is an ERROR, because it
// means foci can replace a binary it execs by bare name.
func reportPopulations(envFile string, home string, canWrite func(string) bool) {
	trustedDirs := dedupe(procx.PathDirs(procx.Trusted))
	operatorDirs := dedupe(procx.PathDirs(procx.Operator))

	startupLog.Infof("spawn populations: TRUSTED = foci's own machinery (sh, git, tmux, sudo, the capture shell); OPERATOR = agent and tool shells (claude, opencode, codex, Bash, Tmux)")
	if envFile == "" {
		startupLog.Infof("  operator env file: none loaded — the two populations are IDENTICAL")
	} else {
		startupLog.Infof("  operator env file: %s", envFile)
	}
	// Labelled with Population.String so the log lines and the argument you
	// pass at a spawn site are literally the same word.
	startupLog.Infof("  %-8s PATH: %s", procx.Trusted.String(), strings.Join(trustedDirs, ":"))
	startupLog.Infof("  %-8s PATH: %s", procx.Operator.String(), strings.Join(operatorDirs, ":"))

	operatorOnly := pathDelta(operatorDirs, trustedDirs)
	trustedOnly := pathDelta(trustedDirs, operatorDirs)
	startupLog.Infof("  operator-only dirs (agents search these, foci itself does NOT): %s", formatList(operatorOnly))
	// A trusted-only directory is the OTHER failure mode: foci can reach a
	// binary the agent cannot, so anything foci hands the agent by bare name
	// will not resolve for it.
	startupLog.Infof("  trusted-only dirs (foci searches these, agents do NOT): %s", formatList(trustedOnly))

	if vars := overlayVarNames(); len(vars) > 0 {
		startupLog.Infof("  operator-only vars (%d): %s", len(vars), strings.Join(vars, " "))
	}

	reportTrustedPathFooting(trustedDirs, home, canWrite)
}

// overlayVarNames lists the variable names the operator population adds or
// overrides, excluding PATH (reported above in full).
func overlayVarNames() []string {
	var names []string
	for _, kv := range procx.OperatorOverlay() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "PATH" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// reportTrustedPathFooting asserts and reports the invariant that defines
// #1914 as done.
func reportTrustedPathFooting(trustedDirs []string, home string, canWrite func(string) bool) {
	findings := checkTrustedPathFooting(trustedDirs, home, canWrite)

	var unexplained, allowed int
	for _, f := range findings {
		switch {
		case !f.Writable:
			continue
		case f.Exception != "":
			allowed++
			startupLog.Infof("  trusted PATH exception: %s is writable by foci — ALLOWED: %s", f.Dir, f.Exception)
		default:
			unexplained++
			startupLog.Errorf("  trusted PATH VIOLATION: %s is writable by the foci process, and is not a named exception. foci execs binaries from here by bare name, so anything that can write this directory can choose what the DAEMON runs — with no approval and no guard. Either make it unwritable, take it off the unit's PATH (Makefile SERVICE_PATH), or add it to trustedPathExceptions with the reason.", f.Dir)
		}
	}

	summary := fmt.Sprintf("  trusted PATH footing: %d dirs, %d writable-and-allowed, %d writable-and-UNEXPLAINED", len(findings), allowed, unexplained)
	if unexplained > 0 {
		startupLog.Errorf("%s", summary)
		return
	}
	startupLog.Infof("%s", summary)
}

// liveCanWrite is execguard's own writability primitive, taken from the same
// Env the auto-approve guard uses so the two cannot drift.
func liveCanWrite() func(string) bool { return execguard.Live().CanWrite }
