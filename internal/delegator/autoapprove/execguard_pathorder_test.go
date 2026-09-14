package autoapprove

// #1900 / #1914 regression: the match-time guard must resolve command names
// against the PATH the TOOL SHELLS actually use — the operator population —
// and NOT against foci-gw's own process PATH.
//
// The two are now genuinely different lists. Before #1914 they were the same
// object (shellenv.Apply os.Setenv'd the operator env onto the daemon), and
// #1900's fix was simply to read it late enough. #1914 split them: the daemon
// keeps the unit's PATH so it cannot be made to exec an agent-writable binary,
// and only agent shells get the operator's. Reading os.Getenv("PATH") here
// would now be #1900 wearing the opposite mask — the guard judging
// /usr/bin/git while ~/scripts/git is what the agent runs.
//
// These tests therefore install the dotfile ONLY into the operator population
// and assert that the process PATH is untouched. That second assertion is the
// load-bearing one: it is what distinguishes "the guard reads the operator
// env" from "the guard reads whatever happens to be global".
//
// WHY THE REST OF THE SUITE CANNOT CATCH THIS. Every other test here injects a
// fixed execguard.Env through withGuardEnv, which asserts only that
// Substitutable honours the PathDirs it is handed. It always did. The bug was
// one level up — WHICH PathDirs the production code handed it — so a test built
// on the fake passes identically against the broken code.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"foci/internal/execguard"
	"foci/internal/procx"
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
}

func TestGuardSeesTheOperatorPathAndNotTheDaemonsOwn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) ignores mode bits, so nothing is ever read-only")
	}
	const probe = "zzguardprobe"
	shimDir, rc := shimOnDotfilePath(t, probe)
	useProductionGuard(t)

	// Control: the shim is nowhere near either PATH yet, so the guard must
	// have nothing to say about it.
	if sub, _, _ := commandIsSubstitutable(probe + " --version"); sub {
		t.Fatal("control failed: the shim must not be reachable before the dotfile is loaded")
	}

	installOperatorEnv(t, &rc)

	// Precondition, and the point of the test: the shim is on the OPERATOR
	// PATH and NOT on the daemon's own. Note that /proc/self/environ can audit
	// neither — it records the exec-time environment, so it reflects neither
	// os.Setenv nor a per-spawn cmd.Env. Reading it is what produced #1900's
	// first, wrong diagnosis.
	if !slices.Contains(procx.PathDirs(procx.Operator), shimDir) {
		t.Fatalf("precondition failed: %s is not on the operator PATH (%v)", shimDir, procx.PathDirs(procx.Operator))
	}
	if slices.Contains(filepath.SplitList(os.Getenv("PATH")), shimDir) {
		t.Fatalf("precondition failed: %s leaked onto the DAEMON's own PATH — the capture must stay a value (#1914)", shimDir)
	}

	if sub, _, _ := commandIsSubstitutable(probe + " --version"); !sub {
		t.Fatal("the guard did not see a writable shim that IS on the operator PATH: it is judging the daemon's PATH, so it clears a different binary than the one the agent runs (#1900, #1914)")
	}
}

// The same failure wearing its other mask: a command ABSENT from the daemon's
// PATH and present on the operator's. Substitutable yields no verdict for an
// unresolvable name, which reads as "safe" — so this arm cannot be caught by
// the veto at all, only by asking whether the guard can locate the command.
// That is what UnresolvedBareName is for.
func TestUnresolvedBareNameFollowsTheOperatorPath(t *testing.T) {
	const probe = "zzabsentprobe"
	shimDir, rc := shimOnDotfilePath(t, probe)
	useProductionGuard(t)

	if !execguard.UnresolvedBareName(probe, guardEnv()) {
		t.Fatalf("control failed: %s must be unresolvable before the dotfile is loaded", probe)
	}

	installOperatorEnv(t, &rc)
	if !slices.Contains(procx.PathDirs(procx.Operator), shimDir) {
		t.Fatalf("precondition failed: %s is not on the operator PATH", shimDir)
	}

	if execguard.UnresolvedBareName(probe, guardEnv()) {
		t.Errorf("%s is on the operator PATH and must resolve; reporting it unresolvable is the 'looking in the wrong place' half of #1900", probe)
	}
}

// installOperatorEnv does exactly what main() does: capture the rc file as a
// value and record it as the operator population. It does NOT touch this
// process's environment — that is the behaviour under test.
func installOperatorEnv(t *testing.T, rc *string) {
	t.Helper()
	env, _, ok := shellenv.Load(rc)
	if !ok {
		t.Fatalf("shellenv.Load(%q) captured nothing", *rc)
	}
	prev := procx.OperatorOverlay()
	t.Cleanup(func() {
		restored := make(map[string]string, len(prev))
		for _, kv := range prev {
			if k, v, ok := strings.Cut(kv, "="); ok {
				restored[k] = v
			}
		}
		procx.SetOperatorEnv(restored)
	})
	procx.SetOperatorEnv(env)
}
