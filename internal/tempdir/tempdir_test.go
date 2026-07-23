package tempdir

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Verifies Dir() returns a writable directory.
func TestDirIsWritable(t *testing.T) {
	d := Dir()
	if d == "" {
		t.Fatal("Dir() returned empty string")
	}
	info, err := os.Stat(d)
	if err != nil {
		t.Fatalf("Dir() directory does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("Dir() path is not a directory")
	}

	// Verify we can actually create files in it.
	f, err := os.CreateTemp(d, "test-*.txt")
	if err != nil {
		t.Fatalf("CreateTemp in Dir(): %v", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
}

// Verifies SpawnDir() returns a writable subdirectory under Dir().
func TestSpawnDirIsWritable(t *testing.T) {
	d := SpawnDir()
	if !strings.HasPrefix(d, Dir()+"/") {
		t.Fatalf("SpawnDir() %q is not under Dir() %q", d, Dir())
	}
	info, err := os.Stat(d)
	if err != nil {
		t.Fatalf("SpawnDir() directory does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("SpawnDir() path is not a directory")
	}
}

// Verifies temp subdirectories can be created in Dir().
func TestMkdirTemp(t *testing.T) {
	d, err := os.MkdirTemp(Dir(), "test-*")
	if err != nil {
		t.Fatalf("MkdirTemp in Dir(): %v", err)
	}
	defer os.RemoveAll(d)

	if !strings.HasPrefix(d, Dir()+"/") {
		t.Fatalf("temp dir %q not under Dir() %q", d, Dir())
	}
}

// Verifies probeDir returns empty for an unwritable path and succeeds
// for a writable one.
func TestProbeDir(t *testing.T) {
	// Unwritable path should return empty.
	if result := probeDir("/proc/nonexistent", rootMode); result != "" {
		t.Errorf("probeDir(/proc/nonexistent) = %q, want empty", result)
	}

	// Writable path should succeed.
	dir := t.TempDir()
	if result := probeDir(dir, rootMode); result != dir {
		t.Errorf("probeDir(%q) = %q, want %q", dir, result, dir)
	}
}

// Verifies the root/spawn dir request no longer asks for a world-writable
// mode (#1501): a fresh install where MkdirAll actually creates the dir
// must not hand every local user a symlink-plant target against every
// predictable-path writer under the root.
func TestRootModeNotWorldWritable(t *testing.T) {
	if rootMode&0o002 != 0 {
		t.Fatalf("rootMode %o is world-writable — must not request 1777", rootMode)
	}
	if privateMode&0o077 != 0 {
		t.Fatalf("privateMode %o grants non-owner access — must be private", privateMode)
	}
}

// Verifies the #1510 guard: when no override is set and the root would
// resolve to the shared production Root, a test binary must panic loudly
// rather than silently write into the live install's state.
func TestGuardLiveRootInTestFiresOnUnsetOverride(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("guardLiveRootInTest(\"\", Root) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		if !strings.Contains(msg, EnvOverride) {
			t.Errorf("panic message %q doesn't name %s", msg, EnvOverride)
		}
		if !strings.Contains(msg, Root) {
			t.Errorf("panic message %q doesn't name the live root %s", msg, Root)
		}
		if !strings.Contains(msg, "go test") {
			t.Errorf("panic message %q doesn't give the actionable one-liner", msg)
		}
	}()
	guardLiveRootInTest("", Root)
}

// Verifies the guard STILL fires when an override was set but the run
// nonetheless landed on the shared Root — resolveRoot degrades an unusable
// override by falling through the ladder, so an override that is set is no
// evidence the run is isolated. The panic must name the override so the cause
// is obvious.
func TestGuardLiveRootInTestFiresWhenUnusableOverrideFellThrough(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("guardLiveRootInTest with an override resolving to Root did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		if !strings.Contains(msg, "/some/override/dir") {
			t.Errorf("panic message %q should name the unusable override", msg)
		}
	}()
	guardLiveRootInTest("/some/override/dir", Root)
}

// Verifies an override that resolves somewhere OTHER than the shared root is
// fine — that is the normal isolated-test case.
func TestGuardLiveRootInTestDoesNotFireWithWorkingOverride(t *testing.T) {
	guardLiveRootInTest("/some/override/dir", "/some/override/dir") // must not panic
}

// Verifies the guard does NOT fire for the per-uid fallback or os.TempDir()
// — only the exact shared Root is disallowed under test with no override.
func TestGuardLiveRootInTestDoesNotFireOnFallback(t *testing.T) {
	guardLiveRootInTest("", fmt.Sprintf("/tmp/foci-%d", os.Getuid())) // must not panic
	guardLiveRootInTest("", os.TempDir())                             // must not panic
}

// Verifies the FOCI_TMPDIR override ladder: a usable override wins over the
// shared Root (hermetic test runs / multi-instance hosts); an unusable
// override falls through to the default ladder instead of failing (a bad
// value degrades gracefully); no override behaves as before.
func TestResolveRoot(t *testing.T) {
	override := t.TempDir()
	if got := resolveRoot(override); got != override {
		t.Errorf("usable override: resolveRoot(%q) = %q, want the override", override, got)
	}

	// Unusable override (can't be created) falls through to the ladder —
	// the result must be a real writable dir, not "" and not the override.
	if got := resolveRoot("/proc/nonexistent"); got == "" || got == "/proc/nonexistent" {
		t.Errorf("unusable override: resolveRoot = %q, want a fallback dir", got)
	}

	// No override: same ladder as before (Root or its fallbacks).
	if got := resolveRoot(""); got == "" {
		t.Error("empty override: resolveRoot returned empty string")
	}
}
