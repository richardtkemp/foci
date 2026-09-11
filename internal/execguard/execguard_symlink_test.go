package execguard

import (
	"os"
	"path/filepath"
	"testing"
)

// liveEnv is the real process environment with an explicit PATH, so these
// tests exercise the SAME predicates the daemon uses. The injected map-backed
// Env in execguard_test.go cannot model symlinks at all: its CanWrite is a set
// lookup, so a link and its target are unrelated keys and every symlink test
// written against it passes regardless of the code under test. Symlink
// behaviour is only testable against a real filesystem.
func liveEnv(t *testing.T, pathDirs ...string) Env {
	t.Helper()
	env := Live()
	env.PathDirs = pathDirs
	return env
}

// symlinkFixture builds the layout from #1893:
//
//	ro/          not writable
//	ro/cmd   ->  w/target
//	w/target     0555, not writable
//	w/           writable
//
// Neither thing the pre-fix check looked at is writable — access(2) follows the
// link to the read-only target, and filepath.Dir yields the link's read-only
// directory — yet the bytes that run are trivially replaceable.
func symlinkFixture(t *testing.T) (link, target, roDir, wDir string) {
	t.Helper()
	root := t.TempDir()
	roDir = filepath.Join(root, "ro")
	wDir = filepath.Join(root, "w")
	for _, d := range []string{roDir, wDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("Mkdir %s: %v", d, err)
		}
	}
	target = filepath.Join(wDir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o555); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link = filepath.Join(roDir, "cmd")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
	return link, target, roDir, wDir
}

func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) ignores mode bits, so no path is ever read-only")
	}
}

// #1893: the two legs of checkResolvedPath disagreed about symlinks. Leg 1
// (access) followed the link; leg 2 (filepath.Dir) did not. So a symlink whose
// TARGET sits in a writable directory was reported safe.
func TestSymlink_WritableTargetDirectoryIsSubstitutable(t *testing.T) {
	requireNonRoot(t)
	link, target, roDir, wDir := symlinkFixture(t)

	// Preconditions — prove the trap is set, so a pass cannot come from the
	// fixture being wrong rather than the code being right.
	if processCanWrite(target) {
		t.Fatal("precondition: the target must NOT be writable")
	}
	if processCanWrite(roDir) {
		t.Fatal("precondition: the link's own directory must NOT be writable")
	}
	if !processCanWrite(wDir) {
		t.Fatal("precondition: the target's directory MUST be writable")
	}

	// And prove the exploit is real: unlink-and-recreate through the link's
	// directory really does change the bytes that run under the link's name.
	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove target: %v", err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho PWNED\n"), 0o555); err != nil {
		t.Fatalf("rewrite target: %v", err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile via link: %v", err)
	}
	if string(got) != "#!/bin/sh\necho PWNED\n" {
		t.Fatalf("exploit precondition failed: link reads %q", got)
	}

	sub, path, reason := Substitutable(link, liveEnv(t))
	if !sub {
		t.Fatal("a symlink whose target's directory is writable must be substitutable: the target can be unlinked and replaced")
	}
	if path == "" || reason == "" {
		t.Errorf("path=%q reason=%q, want both populated", path, reason)
	}
}

// The same defect one level up: a BARE NAME whose PATH winner is such a
// symlink. This is the shape that matters in production — auto_approve entries
// name commands, not paths.
func TestSymlink_BareNameWinnerWithWritableTargetDirectory(t *testing.T) {
	requireNonRoot(t)
	link, _, roDir, _ := symlinkFixture(t)

	if sub, _, _ := Substitutable(filepath.Base(link), liveEnv(t, roDir)); !sub {
		t.Error("a bare name resolving to a symlink with a writable target directory must be substitutable")
	}
}

// Control for both tests above: make ONLY the target's directory read-only and
// everything must pass. Without this, a checker that reports every symlink
// substitutable would satisfy the tests above.
func TestSymlink_FullyReadOnlyChainPasses(t *testing.T) {
	requireNonRoot(t)
	link, _, _, wDir := symlinkFixture(t)
	if err := os.Chmod(wDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wDir, 0o755) })

	if sub, path, reason := Substitutable(link, liveEnv(t)); sub {
		t.Errorf("a read-only target in a read-only directory, linked from a read-only directory, must pass; got path=%q reason=%q", path, reason)
	}
}

// The OTHER symlink vector, which the pre-fix code did catch by accident and
// which must not be lost: a re-pointable link. The target and its directory are
// both read-only, but the LINK lives somewhere writable, so the link itself can
// be swapped for one aiming elsewhere.
func TestSymlink_RepointableLinkStaysSubstitutable(t *testing.T) {
	requireNonRoot(t)
	root := t.TempDir()
	roDir := filepath.Join(root, "ro")
	linkDir := filepath.Join(root, "links")
	for _, d := range []string{roDir, linkDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("Mkdir %s: %v", d, err)
		}
	}
	target := filepath.Join(roDir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	link := filepath.Join(linkDir, "cmd")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if sub, _, _ := Substitutable(link, liveEnv(t)); !sub {
		t.Error("a symlink in a writable directory can be re-pointed and must stay substitutable")
	}
}
