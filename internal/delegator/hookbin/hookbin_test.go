package hookbin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolve_UnderTestIgnoresPATH is the regression test for the 2026-08-06
// red main: a unit test's outcome must not depend on whether foci happens to
// be DEPLOYED on the machine running it.
//
// It plants an executable named exactly like a hook binary on $PATH — which is
// what /usr/local/bin/foci-codex-hook is in production — and asserts Resolve
// does NOT find it. Before the guard, it did, and that flipped codex's
// prepareHookArgs from its early return into a trust probe that spawns an
// extra app-server, turning TestRunBatch_CodexOwnerGetsDistinctFacadeShared
// AppServer red on a commit that had passed hours earlier.
func TestResolve_UnderTestIgnoresPATH(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "foci-codex-hook")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvOverride, "")

	// Guard the premise: if the plant isn't actually reachable as an executable
	// on PATH, this test would pass for the wrong reason — it would be
	// asserting "not found" about something that was never findable.
	if !isExecutableFile(planted) {
		t.Fatalf("premise failed: planted %s is not an executable file", planted)
	}

	got, err := Resolve("foci-codex-hook")
	if err == nil {
		t.Fatalf("Resolve found %q on $PATH under test — a deployed binary leaked into a "+
			"test's behaviour, which is what makes the same commit pass on one box and fail "+
			"on another", got)
	}
	if !strings.Contains(err.Error(), EnvOverride) {
		t.Errorf("error should tell the reader how to opt in via %s; got: %v", EnvOverride, err)
	}
}

// TestResolve_OverrideOptsIn proves the escape hatch works, so a test that
// genuinely wants the hook-present branch is not stuck. Without this, the
// guard above could be "satisfied" by a Resolve that always fails.
func TestResolve_OverrideOptsIn(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "hook-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOverride, stub)

	got, err := Resolve("foci-codex-hook")
	if err != nil {
		t.Fatalf("Resolve with %s set: %v", EnvOverride, err)
	}
	if got != stub {
		t.Errorf("Resolve = %q, want the override %q", got, stub)
	}
}

// TestResolve_OverrideMustBeExecutable pins that the opt-in validates its
// argument rather than trusting it — a typo'd override should say so, not
// hand back a path that will fail confusingly later in a subprocess.
//
// The directory and non-executable cases came from codex's hooks_test.go,
// where they covered that package's own copy of isExecutableFile. They moved
// here with the function rather than being dropped, so consolidating the two
// resolvers costs no coverage.
func TestResolve_OverrideMustBeExecutable(t *testing.T) {
	dir := t.TempDir()

	nonExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(nonExec, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, path string }{
		{"missing", filepath.Join(dir, "does-not-exist")},
		{"directory", dir},
		{"non-executable", nonExec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvOverride, tc.path)
			if got, err := Resolve("foci-codex-hook"); err == nil {
				t.Fatalf("Resolve accepted a %s override, returning %q", tc.name, got)
			}
		})
	}
}

// TestResolveFromSystem_LookupOrder preserves the coverage that ccstream's
// TestResolveHookBinary_SiblingFound and _PathFallback used to provide: the
// sibling of the running executable wins, and $PATH is the fallback. Those
// two drove the real filesystem — one of them t.Skipf'd itself whenever a
// sibling existed, so on a machine with foci deployed it asserted nothing.
// Injecting both environment reads makes the order assertable everywhere.
func TestResolveFromSystem_LookupOrder(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "bin", "foci-cc-hook")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	onPath := filepath.Join(dir, "elsewhere-foci-cc-hook")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	self := func() (string, error) { return filepath.Join(dir, "bin", "foci-gw"), nil }
	found := func(string) (string, error) { return onPath, nil }
	missing := func(string) (string, error) { return "", os.ErrNotExist }

	t.Run("sibling wins over PATH", func(t *testing.T) {
		if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveFromSystem("foci-cc-hook", self, found)
		if err != nil {
			t.Fatalf("resolveFromSystem: %v", err)
		}
		if got != sibling {
			t.Errorf("got %q, want the sibling %q — $PATH must not win when a sibling exists", got, sibling)
		}
	})

	t.Run("falls back to PATH when no sibling", func(t *testing.T) {
		if err := os.Remove(sibling); err != nil {
			t.Fatal(err)
		}
		got, err := resolveFromSystem("foci-cc-hook", self, found)
		if err != nil {
			t.Fatalf("resolveFromSystem: %v", err)
		}
		if got != onPath {
			t.Errorf("got %q, want the $PATH hit %q", got, onPath)
		}
	})

	t.Run("errors naming the binary when neither hits", func(t *testing.T) {
		_, err := resolveFromSystem("foci-cc-hook", self, missing)
		if err == nil {
			t.Fatal("resolveFromSystem succeeded with no sibling and no $PATH hit")
		}
		if !strings.Contains(err.Error(), "foci-cc-hook") {
			t.Errorf("error should name the binary; got: %v", err)
		}
	})
}
