package procx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resetOperatorEnv restores the package-level overlay after a test mutates it.
// The overlay is process-global by design (it is the single derivation), so a
// test that sets it must put it back.
func resetOperatorEnv(t *testing.T) {
	t.Helper()
	operatorMu.RLock()
	prev := slices.Clone(operatorOverlay)
	operatorMu.RUnlock()
	t.Cleanup(func() {
		operatorMu.Lock()
		operatorOverlay = prev
		operatorMu.Unlock()
	})
}

// writeExe drops an executable stub at dir/name and returns its path.
func writeExe(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The zero Population must never be usable. Both misclassifications are
// silent; a MISSING classification is the one failure we can make loud, so it
// panics rather than defaulting.
func TestZeroPopulationPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Spawn with the zero Population must panic, not pick a default")
		}
	}()
	_ = Spawn(context.Background(), Population{}, "echo", "hi")
}

// With no overlay set, the two populations are identical — the correct state
// when no rc file resolved, and the state phases 1-2 rely on for "no
// behaviour change".
func TestPopulationsCollapseWithoutOverlay(t *testing.T) {
	resetOperatorEnv(t)
	SetOperatorEnv(nil)

	if got, want := Env(Operator), Env(Trusted); !slices.Equal(got, want) {
		t.Fatalf("with no overlay the populations must be identical:\n operator=%v\n trusted =%v", got, want)
	}
}

// The overlay must override the inherited value, not merely be appended
// alongside it — that is the layering shellenv.Apply used to get from
// os.Setenv.
func TestOperatorOverlayOverridesInherited(t *testing.T) {
	resetOperatorEnv(t)
	t.Setenv("FOCI_TEST_POP", "from-process")
	SetOperatorEnv(map[string]string{"FOCI_TEST_POP": "from-dotfile"})

	if got := envValue(Env(Operator), "FOCI_TEST_POP"); got != "from-dotfile" {
		t.Fatalf("operator env: got %q, want the dotfile value to win", got)
	}
	if got := envValue(Env(Trusted), "FOCI_TEST_POP"); got != "from-process" {
		t.Fatalf("trusted env: got %q, want the process value — the overlay must NOT reach the trusted population", got)
	}
}

// The core of #1914: os/exec resolves a bare name against the PARENT's $PATH,
// so setting cmd.Env alone would not change which FILE runs. Spawn must
// re-resolve against the population's own PATH.
//
// The arms are deliberately asymmetric — same bare name, two different files —
// because that is exactly the defect: the daemon executing an agent-writable
// `bash` while every check judged /usr/bin/bash.
func TestSpawnResolvesAgainstThePopulationPath(t *testing.T) {
	resetOperatorEnv(t)
	trustedDir := t.TempDir()
	operatorDir := t.TempDir()
	trustedExe := writeExe(t, trustedDir, "zzpopprobe")
	operatorExe := writeExe(t, operatorDir, "zzpopprobe")

	// The process (= trusted) PATH sees only the trusted copy.
	t.Setenv("PATH", trustedDir)
	// The operator overlay puts its own directory FIRST, as ~/.shellcommon does.
	SetOperatorEnv(map[string]string{"PATH": operatorDir + string(os.PathListSeparator) + trustedDir})

	if got := Spawn(context.Background(), Trusted, "zzpopprobe").Path; got != trustedExe {
		t.Fatalf("trusted spawn resolved %q, want %q", got, trustedExe)
	}
	if got := Spawn(context.Background(), Operator, "zzpopprobe").Path; got != operatorExe {
		t.Fatalf("operator spawn resolved %q, want %q — cmd.Env alone does not steer resolution, so Spawn must", got, operatorExe)
	}
}

// A name that exists only on the operator PATH must be unresolvable for the
// trusted population, and the failure must arrive the way exec.Command's does
// (via cmd.Err, as an *exec.Error) so callers' error handling is unchanged.
func TestTrustedSpawnCannotSeeOperatorOnlyBinary(t *testing.T) {
	resetOperatorEnv(t)
	trustedDir := t.TempDir()
	operatorDir := t.TempDir()
	writeExe(t, operatorDir, "zzoperatoronly")

	t.Setenv("PATH", trustedDir)
	SetOperatorEnv(map[string]string{"PATH": operatorDir})

	cmd := Spawn(context.Background(), Trusted, "zzoperatoronly")
	if cmd.Err == nil {
		t.Fatal("trusted spawn of an operator-only binary must fail")
	}
	if !errors.Is(cmd.Err, exec.ErrNotFound) {
		t.Fatalf("cmd.Err = %v, want it to wrap exec.ErrNotFound", cmd.Err)
	}
	if _, err := LookPath(Operator, "zzoperatoronly"); err != nil {
		t.Fatalf("the same name must resolve for the operator population: %v", err)
	}
}

// The child must actually RECEIVE the population's environment, not just have
// it computed — this is what every descendant of a tool shell then inherits.
func TestSpawnInstallsThePopulationEnvOnTheChild(t *testing.T) {
	resetOperatorEnv(t)
	SetOperatorEnv(map[string]string{"FOCI_TEST_MARKER": "operator"})

	out, err := Spawn(context.Background(), Operator, "sh", "-c", "printf %s \"$FOCI_TEST_MARKER\"").Output()
	if err != nil {
		t.Fatalf("operator spawn: %v", err)
	}
	if string(out) != "operator" {
		t.Fatalf("child saw FOCI_TEST_MARKER=%q, want %q", out, "operator")
	}

	out, err = Spawn(context.Background(), Trusted, "sh", "-c", "printf %s \"$FOCI_TEST_MARKER\"").Output()
	if err != nil {
		t.Fatalf("trusted spawn: %v", err)
	}
	if string(out) != "" {
		t.Fatalf("trusted child saw FOCI_TEST_MARKER=%q, want it absent", out)
	}
}

// PathDirs is what internal/execguard judges. It must follow the population,
// or the guard predicts a different file than the one that runs (#1900).
func TestPathDirsFollowsThePopulation(t *testing.T) {
	resetOperatorEnv(t)
	t.Setenv("PATH", "/usr/bin")
	SetOperatorEnv(map[string]string{"PATH": "/opt/agentbin:/usr/bin"})

	if got := PathDirs(Trusted); !slices.Equal(got, []string{"/usr/bin"}) {
		t.Fatalf("trusted PathDirs = %v", got)
	}
	if got := PathDirs(Operator); !slices.Equal(got, []string{"/opt/agentbin", "/usr/bin"}) {
		t.Fatalf("operator PathDirs = %v", got)
	}
}

// An absolute or relative path must be passed through untouched — no
// population's PATH is consulted, matching exec.Command.
func TestLookPathPassesThroughExplicitPaths(t *testing.T) {
	resetOperatorEnv(t)
	dir := t.TempDir()
	exe := writeExe(t, dir, "zzexplicit")
	SetOperatorEnv(map[string]string{"PATH": "/nonexistent"})

	got, err := LookPath(Operator, exe)
	if err != nil || got != exe {
		t.Fatalf("LookPath(%q) = %q, %v — an explicit path must not be re-resolved", exe, got, err)
	}
	if _, err := LookPath(Operator, filepath.Join(dir, "zzmissing")); err == nil {
		t.Fatal("an explicit path to a missing file must still fail")
	}
}

// Population.String feeds the startup report and any spawn diagnostics, so it
// must name the populations the way the docs and log lines do.
func TestPopulationString(t *testing.T) {
	for _, tc := range []struct {
		pop  Population
		want string
	}{{Trusted, "trusted"}, {Operator, "operator"}, {Population{}, "invalid"}} {
		if got := tc.pop.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// OperatorOverlay is the diagnostic read used by the startup report; it must
// hand back a copy, not the live slice.
func TestOperatorOverlayIsACopy(t *testing.T) {
	resetOperatorEnv(t)
	SetOperatorEnv(map[string]string{"A": "1", "B": "2"})
	got := OperatorOverlay()
	if len(got) != 2 || !strings.HasPrefix(got[0], "A=") {
		t.Fatalf("OperatorOverlay() = %v, want sorted KEY=VALUE pairs", got)
	}
	got[0] = "MUTATED=1"
	if again := OperatorOverlay(); again[0] == "MUTATED=1" {
		t.Fatal("OperatorOverlay must return a copy — a caller must not be able to edit the derivation")
	}
}
