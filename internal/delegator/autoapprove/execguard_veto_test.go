package autoapprove

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/execguard"
)

// TestMain pins guardEnv to a hermetic filesystem for the whole package.
//
// Without this the suite's verdicts depend on the HOST: on a machine where
// sqlite3 or gcalcli live in a writable ~/.local/bin, the match-time veto fires
// and unrelated tests fail. Those failures were correct behaviour but useless
// signal. Tests that care about the veto set guardEnv themselves.
func TestMain(m *testing.M) {
	guardEnv = execguard.Env{
		CanWrite:     func(string) bool { return false },
		PathDirs:     []string{"/usr/bin"},
		IsExecutable: func(p string) bool { return filepath.Dir(p) == "/usr/bin" },
		HomeDir:      "/home/foci",
	}
	os.Exit(m.Run())
}

// withGuardEnv swaps the package guard for one test and restores it after.
func withGuardEnv(t *testing.T, env execguard.Env) {
	t.Helper()
	prev := guardEnv
	guardEnv = env
	t.Cleanup(func() { guardEnv = prev })
}

func bashInput(t *testing.T, command string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A writable executable must NOT be auto-approved even when a rule matches it
// exactly. This is the whole point of checking at match time.
func TestMatchTimeVeto_WritableExecutableIsNotApproved(t *testing.T) {
	withGuardEnv(t, execguard.Env{
		CanWrite:     func(p string) bool { return p == "/home/foci/.local/bin/sqlite3" },
		PathDirs:     []string{"/home/foci/.local/bin", "/usr/bin"},
		IsExecutable: func(p string) bool { return p == "/home/foci/.local/bin/sqlite3" },
		HomeDir:      "/home/foci",
	})
	rules := Compile([]string{"Bash:sqlite3 *"})
	if MatchWithEnv(rules, "Bash", bashInput(t, "sqlite3 -readonly /tmp/x.db 'SELECT 1'"), nil) {
		t.Error("a writable executable must not be auto-approved despite a matching rule")
	}
}

// Control: the identical rule and command ARE approved when the binary is not
// writable. Without this, the test above would pass for a veto that rejects
// everything.
func TestMatchTimeVeto_ReadOnlyExecutableIsApproved(t *testing.T) {
	withGuardEnv(t, execguard.Env{
		CanWrite:     func(string) bool { return false },
		PathDirs:     []string{"/usr/bin"},
		IsExecutable: func(p string) bool { return p == "/usr/bin/sqlite3" },
		HomeDir:      "/home/foci",
	})
	rules := Compile([]string{"Bash:sqlite3 *"})
	if !MatchWithEnv(rules, "Bash", bashInput(t, "sqlite3 -readonly /tmp/x.db 'SELECT 1'"), nil) {
		t.Error("a read-only executable with a matching rule must be auto-approved")
	}
}

// The veto ignores rule provenance: a built-in read-only rule gets the same
// treatment as a user entry, because "can this binary be swapped" does not
// depend on which rule allowed it.
func TestMatchTimeVeto_AppliesToBuiltinReadonlyRules(t *testing.T) {
	withGuardEnv(t, execguard.Env{
		CanWrite:     func(p string) bool { return p == "/home/foci/.local/bin/cat" },
		PathDirs:     []string{"/home/foci/.local/bin", "/usr/bin"},
		IsExecutable: func(p string) bool { return p == "/home/foci/.local/bin/cat" },
		HomeDir:      "/home/foci",
	})
	rules := Compile(CommonReadonlyRules)
	if MatchWithEnv(rules, "Bash", bashInput(t, "cat /etc/hostname"), nil) {
		t.Error("a built-in readonly rule must not approve a substitutable binary")
	}
}

// A shadow planted AFTER startup is caught, which a startup-only check cannot
// do. This is the gap the match-time check exists to close.
func TestMatchTimeVeto_CatchesAShadowPlantedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	realTool := filepath.Join(roDir, "mytool")
	// 0555, not 0755: an owner-writable file is itself a finding, which would
	// make the precondition fail for the wrong reason.
	if err := os.WriteFile(realTool, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	shadowDir := filepath.Join(dir, "writable")
	if err := os.Mkdir(shadowDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Real PATH semantics against the real filesystem — shadowDir first.
	withGuardEnv(t, execguard.Env{
		CanWrite:     processCanWriteForTest,
		PathDirs:     []string{shadowDir, roDir},
		IsExecutable: processCanExecuteForTest,
		HomeDir:      dir,
	})
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) ignores mode bits")
	}
	rules := Compile([]string{"Bash:mytool *"})
	cmd := bashInput(t, "mytool --version")

	// Before: resolves to the read-only real tool, so it is approved.
	if !MatchWithEnv(rules, "Bash", cmd, nil) {
		t.Fatal("precondition failed: the read-only tool should be approved")
	}
	// Plant the shadow, exactly as a running agent could at any time.
	if err := os.WriteFile(filepath.Join(shadowDir, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// After: the SAME rules object, no restart, no reload.
	if MatchWithEnv(rules, "Bash", cmd, nil) {
		t.Error("a shadow planted after the rules were compiled must be caught")
	}
}
