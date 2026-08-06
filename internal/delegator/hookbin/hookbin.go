// Package hookbin resolves a foci hook helper binary (foci-cc-hook,
// foci-codex-hook) for a backend that needs to hand its path to a subprocess.
//
// It exists to have ONE resolver rather than a copy per backend, because the
// copies drifted into a hermeticity bug: both consulted $PATH, so under test
// they found whatever the machine happened to have DEPLOYED at
// /usr/local/bin. A unit test's behaviour then depended on whether foci was
// installed on the box, which is not a property any test should have.
//
// Concretely (2026-08-06): TestRunBatch_CodexOwnerGetsDistinctFacadeSharedApp
// Server passed on commit 448192bf at 16:17 and failed on the SAME commit
// from 11:24 the next morning. Nothing in the repo changed. What changed was
// /usr/local/bin/foci-codex-hook appearing at 16:23:01 — a deploy, six
// minutes after the last green run. With the hook resolvable, codex's
// prepareHookArgs stops returning early and runs a trust probe, and that
// probe spawns a THROWAWAY app-server that calls initialize a second time.
// The test counts initialize calls, so it read 2 instead of 1 and reported
// "the batch spawned its own process" — a mechanism that never happened.
//
// See Resolve for the guard that makes that impossible rather than merely
// fixed.
package hookbin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// EnvOverride names an explicit hook binary. Production never sets it; it is
// how a TEST opts in to the hook-present branch, deliberately and with a path
// it controls. Mirrors tempdir.EnvOverride in spirit: the escape hatch is an
// env var so a test can set it without the production call site growing a
// parameter that only tests would ever pass.
const EnvOverride = "FOCI_HOOK_BIN"

// Resolve locates a hook helper binary by name: first as a sibling of the
// running executable (the Makefile builds foci-gw and the hooks into the same
// bin/), then on $PATH.
//
// UNDER TEST both of those lookups are DISABLED, and this returns an error
// unless EnvOverride names an executable. That is the structural half of the
// fix: a test cannot accidentally resolve a deployed binary, because the code
// path that would find one does not run. `testing.Testing()` is the same
// mechanism internal/tempdir uses to refuse the live shared root.
//
// The asymmetry with tempdir is deliberate: tempdir PANICS because writing
// into a live install is never recoverable, whereas "no hook binary" is an
// ordinary, supported production state (every caller logs a warning and
// continues without the hook). So the test-time answer is the SAFE one —
// absent — rather than a crash. A test that wants the hook present says so.
func Resolve(name string) (string, error) {
	if override := os.Getenv(EnvOverride); override != "" {
		if isExecutableFile(override) {
			return override, nil
		}
		return "", fmt.Errorf("%s=%q is not an executable file", EnvOverride, override)
	}

	if testing.Testing() {
		return "", fmt.Errorf(
			"%s not resolved: under test, the sibling and $PATH lookups are disabled so a "+
				"DEPLOYED binary can never leak into a test's behaviour. Set %s to an "+
				"executable to exercise the hook-present branch", name, EnvOverride)
	}

	return resolveFromSystem(name, os.Executable, exec.LookPath)
}

// resolveFromSystem is Resolve's production lookup, with its two environment
// reads injected. Split out so the sibling-then-$PATH ORDER stays directly
// testable even though Resolve refuses to perform it under test: the tests
// that used to cover this (ccstream's TestResolveHookBinary_SiblingFound /
// _PathFallback) drove the real filesystem, and one of them skipped itself
// whenever the sibling happened to exist — deployment-dependent coverage,
// the same disease in a quieter form. Stubs make the order assertable on any
// machine, deployed or not.
func resolveFromSystem(
	name string,
	executable func() (string, error),
	lookPath func(string) (string, error),
) (string, error) {
	var siblingErr error
	if self, err := executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), name)
		if isExecutableFile(candidate) {
			return candidate, nil
		}
		siblingErr = fmt.Errorf("sibling %s not executable", candidate)
	} else {
		siblingErr = fmt.Errorf("os.Executable: %w", err)
	}

	if path, err := lookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found (%v; and not on $PATH)", name, siblingErr)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
}
