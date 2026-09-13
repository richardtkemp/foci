package procx

import (
	"os"
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
