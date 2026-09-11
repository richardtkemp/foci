package autoapprove

// #1900 regression: the match-time guard must resolve command names against the
// PATH the tool shells actually use, which is the one shellenv.Apply() installs
// from the operator's dotfiles INSIDE main() — not the one this package saw
// when Go ran its package-level initialisers before main() started.
//
// WHY THE REST OF THE SUITE CANNOT CATCH THIS. Every other test here injects a
// fixed execguard.Env through withGuardEnv, which asserts only that
// Substitutable honours the PathDirs it is handed. It always did. The bug was
// one level up — WHICH PathDirs the production code handed it — so a test built
// on the fake passes identically against the broken code. These tests use the
// real production binding and a real dotfile, and they assert the ORDERING: a
// PATH entry that appears only after package init must still be seen.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"foci/internal/execguard"
	"foci/internal/shellenv"
)

// shimOnDotfilePath builds an agent-writable directory holding an executable,
// plus an rc file that prepends that directory to PATH — the live host's
// ~/.shellcommon in miniature (it prepends $HOME/bin and $HOME/scripts, and
// $HOME/scripts is agent-writable and ahead of /usr/bin).
func shimOnDotfilePath(t *testing.T, name string) (shimDir, rc string) {
	t.Helper()
	shimDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile shim: %v", err)
	}
	rc = filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rc, []byte(fmt.Sprintf("export PATH=%q:$PATH\n", shimDir)), 0o644); err != nil {
		t.Fatalf("WriteFile rc: %v", err)
	}
	return shimDir, rc
}

// productionGuardEnv is the package-level binding EXACTLY as package init left
// it. It must be captured here, because TestMain overwrites guardEnv with a
// hermetic fake before any test runs — and a test that restores the real thing
// by naming execguard.Live directly would be asserting on its own literal
// rather than on what production binds. Go initialises package-level vars in
// dependency order, so this reads guardEnv after guardEnv is set and before
// TestMain touches it. Rebinding guardEnv to a fresh init-time SNAPSHOT (the
// pre-#1900 shape) is what reddens the tests below.
var productionGuardEnv = guardEnv

// useProductionGuard swaps the package's hermetic fake back for the real
// binding, restoring the fake afterwards so the rest of the suite stays
// host-independent.
func useProductionGuard(t *testing.T) {
	t.Helper()
	prev := guardEnv
	guardEnv = productionGuardEnv
	t.Cleanup(func() { guardEnv = prev })
	// Registering the current value restores PATH after shellenv.Apply has
	// overwritten it.
	t.Setenv("PATH", os.Getenv("PATH"))
}

func TestGuardSeesThePathShellenvInstallsAfterPackageInit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) ignores mode bits, so nothing is ever read-only")
	}
	const probe = "zzguardprobe"
	shimDir, rc := shimOnDotfilePath(t, probe)
	useProductionGuard(t)

	// guardEnv was bound during package init — before this function ran, just
	// as it is bound before main() calls shellenv.Apply(). Control: the shim is
	// nowhere near PATH yet, so the guard must have nothing to say about it.
	if commandIsSubstitutable(probe + " --version") {
		t.Fatal("control failed: the shim must not be reachable before the dotfile is loaded")
	}

	shellenv.Apply(&rc)

	// Precondition: the dotfile really did land in this process's environment.
	// Note that /proc/self/environ still shows the OLD PATH here — os.Setenv
	// never updates it. Reading that file is what produced #1900's first, wrong
	// diagnosis; os.Getenv is the instrument that works.
	if !slices.Contains(filepath.SplitList(os.Getenv("PATH")), shimDir) {
		t.Fatalf("precondition failed: shellenv did not install %s on PATH (%s)", shimDir, os.Getenv("PATH"))
	}

	if !commandIsSubstitutable(probe + " --version") {
		t.Fatal("the guard resolved against a PATH captured at package init: a writable shim installed by shellenv AFTER init must be seen, or the guard clears a different binary than the one that runs (#1900)")
	}
}

// The same ordering failure wearing its other mask: a command that is ABSENT
// from the init-time PATH and present on the shellenv one. Substitutable yields
// no verdict for an unresolvable name, which reads as "safe" — so this arm
// cannot be caught by the veto at all, only by asking whether the guard can
// locate the command. That is what UnresolvedBareName is for.
func TestUnresolvedBareNameFollowsTheLivePath(t *testing.T) {
	const probe = "zzabsentprobe"
	shimDir, rc := shimOnDotfilePath(t, probe)
	useProductionGuard(t)

	if !execguard.UnresolvedBareName(probe, guardEnv()) {
		t.Fatalf("control failed: %s must be unresolvable before the dotfile is loaded", probe)
	}

	shellenv.Apply(&rc)
	if !slices.Contains(filepath.SplitList(os.Getenv("PATH")), shimDir) {
		t.Fatalf("precondition failed: shellenv did not install %s on PATH", shimDir)
	}

	if execguard.UnresolvedBareName(probe, guardEnv()) {
		t.Errorf("%s is on the live PATH and must resolve; reporting it unresolvable is the 'looking in the wrong place' half of #1900", probe)
	}
}
