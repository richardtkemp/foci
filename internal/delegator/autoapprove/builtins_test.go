package autoapprove

import (
	"testing"

	"foci/internal/execguard"
)

// builtinsEnv builds a map-backed execguard.Env: PATH directories, writable
// paths, executable files. No test touches the host's filesystem or PATH.
func builtinsEnv(pathDirs, writable, executables []string) execguard.Env {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return execguard.Env{
		CanWrite:     func(p string) bool { return w[p] },
		PathDirs:     pathDirs,
		IsExecutable: func(p string) bool { return x[p] },
		HomeDir:      "/home/foci",
	}
}

// TestSubstitutableBuiltins_OnlySubstitutableReported proves the scan reports
// a built-in command exactly when (and only when) the enforcer's own
// judgement refuses it: a substitutable PATH winner is reported with
// execguard's own reason and path, a safe winner is not, and a tool absent
// from this host (yq) is silent — a built-in naming an uninstalled tool is
// normal, not a config error.
func TestSubstitutableBuiltins_OnlySubstitutableReported(t *testing.T) {
	env := builtinsEnv(
		[]string{"/home/foci/bin", "/usr/bin"},
		[]string{"/home/foci/bin/mds"},
		[]string{"/home/foci/bin/mds", "/usr/bin/rg", "/usr/bin/crontab"},
	)

	found := SubstitutableBuiltins(true, false, env)

	if len(found) != 1 {
		t.Fatalf("found %d substitutable built-ins, want exactly mds: %+v", len(found), found)
	}
	got := found[0]
	if got.Group != GroupReadonly || got.Command != "mds" {
		t.Fatalf("finding = {Group:%q Command:%q}, want {read-only mds}", got.Group, got.Command)
	}
	wantSub, wantPath, wantReason := execguard.Substitutable("mds", env)
	if !wantSub {
		t.Fatal("control failed: execguard says mds is not substitutable")
	}
	if got.Path != wantPath || got.Reason != wantReason {
		t.Errorf("finding = {Path:%q Reason:%q}, want execguard's own {Path:%q Reason:%q} — the report must be the enforcer's verdict, not a copy of its rules", got.Path, got.Reason, wantPath, wantReason)
	}
}

// TestSubstitutableBuiltins_GroupTogglesAndNonBash pins the group gating and
// the shapes that are never reported: a disabled group reports nothing even
// with a substitutable command, the safe-write group is scanned under its own
// name, a subcommand entry (crontab -l) is judged on its first word, and the
// groups' non-Bash tool rules (Search, Read, …) name no executable and never
// produce a finding.
func TestSubstitutableBuiltins_GroupTogglesAndNonBash(t *testing.T) {
	env := builtinsEnv(
		[]string{"/home/foci/bin", "/usr/bin"},
		[]string{"/home/foci/bin/mds", "/home/foci/bin/curl", "/home/foci/bin/crontab"},
		[]string{"/home/foci/bin/mds", "/home/foci/bin/curl", "/home/foci/bin/crontab"},
	)

	if found := SubstitutableBuiltins(false, false, env); len(found) != 0 {
		t.Errorf("readonly=false, safe-write=false reported %+v; a disabled group is not reported", found)
	}

	found := SubstitutableBuiltins(true, false, env)
	if len(found) != 2 {
		t.Fatalf("readonly findings = %+v, want mds and crontab only", found)
	}
	// crontab -l is judged on its first word, exactly as the match-time veto
	// tokenizes the segment.
	for _, b := range found {
		if b.Group != GroupReadonly {
			t.Errorf("finding %+v carries group %q, want %q", b, b.Group, GroupReadonly)
		}
		if b.Command == "crontab" && (b.Path != "/home/foci/bin/crontab" || b.Reason == "") {
			t.Errorf("crontab finding = %+v, want the writable winner with a reason", b)
		}
	}

	safeFound := SubstitutableBuiltins(false, true, env)
	if len(safeFound) != 1 || safeFound[0].Group != GroupSafeWrite || safeFound[0].Command != "curl" {
		t.Fatalf("safe-write findings = %+v, want exactly curl under the safe-write group", safeFound)
	}
}
