package tempdir

import (
	"os"
	"strings"
	"testing"
)

// Verifies Dir() returns the root path and creates the directory.
func TestDirCreatesRoot(t *testing.T) {
	d := Dir()
	if d != Root {
		t.Fatalf("Dir() = %q, want %q", d, Root)
	}
	info, err := os.Stat(d)
	if err != nil {
		t.Fatalf("Dir() directory does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("Dir() path is not a directory")
	}
}

// Verifies TestDir() returns a subdirectory under Root and creates it.
func TestTestDirCreatesSubdir(t *testing.T) {
	d := TestDir()
	if d != Tests {
		t.Fatalf("TestDir() = %q, want %q", d, Tests)
	}
	if !strings.HasPrefix(d, Root) {
		t.Fatalf("TestDir() %q is not under Root %q", d, Root)
	}
	info, err := os.Stat(d)
	if err != nil {
		t.Fatalf("TestDir() directory does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("TestDir() path is not a directory")
	}
}

// Verifies temp files can be created in Dir().
func TestCreateTempFile(t *testing.T) {
	f, err := os.CreateTemp(Dir(), "test-*.txt")
	if err != nil {
		t.Fatalf("CreateTemp in Dir(): %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if !strings.HasPrefix(f.Name(), Root+"/") {
		t.Fatalf("temp file %q not under Root %q", f.Name(), Root)
	}
}

// Verifies temp subdirectories can be created in Dir().
func TestMkdirTemp(t *testing.T) {
	d, err := os.MkdirTemp(Dir(), "test-*")
	if err != nil {
		t.Fatalf("MkdirTemp in Dir(): %v", err)
	}
	defer os.RemoveAll(d)

	if !strings.HasPrefix(d, Root+"/") {
		t.Fatalf("temp dir %q not under Root %q", d, Root)
	}
}
