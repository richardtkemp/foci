package tempdir

import (
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
	if result := probeDir("/proc/nonexistent"); result != "" {
		t.Errorf("probeDir(/proc/nonexistent) = %q, want empty", result)
	}

	// Writable path should succeed.
	dir := t.TempDir()
	if result := probeDir(dir); result != dir {
		t.Errorf("probeDir(%q) = %q, want %q", dir, result, dir)
	}
}
