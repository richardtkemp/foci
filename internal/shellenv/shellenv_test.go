package shellenv

import (
	"os"
	"path/filepath"
	"testing"
)

func strp(s string) *string { return &s }

func TestResolveLadder(t *testing.T) {
	home := t.TempDir()

	// Nothing present → load nothing.
	if p, load := Resolve(nil, home); load {
		t.Fatalf("empty home should load nothing, got %q", p)
	}

	// .profile only → picked (ladder falls through to it).
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, load := Resolve(nil, home); !load || p != profile {
		t.Fatalf("ladder should pick .profile, got %q load=%v", p, load)
	}

	// .bashrc present → wins over .profile (ladder order, max one).
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("export A=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, _ := Resolve(nil, home); p != bashrc {
		t.Fatalf("ladder should prefer .bashrc, got %q", p)
	}
}

func TestResolveExplicitAndBlank(t *testing.T) {
	home := t.TempDir()

	// Blank → load nothing, even though .bashrc exists.
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, load := Resolve(strp(""), home); load {
		t.Fatal("blank config should load nothing")
	}

	// Explicit path (with ~ expansion) → that file.
	custom := filepath.Join(home, "custom.env")
	if p, load := Resolve(strp("~/custom.env"), home); !load || p != custom {
		t.Fatalf("explicit ~ path: got %q load=%v want %q", p, load, custom)
	}
}

func TestCapture(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "rc")
	// Exercise a value with a newline to prove NUL-delimited parsing.
	if err := os.WriteFile(f, []byte("export FOCI_TEST_X=hello\nexport FOCI_TEST_Y=$'a\\nb'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := Capture(f)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if env["FOCI_TEST_X"] != "hello" {
		t.Errorf("FOCI_TEST_X = %q, want hello", env["FOCI_TEST_X"])
	}
	if env["SHLVL"] != "" || env["PWD"] != "" {
		t.Errorf("shell-noise vars leaked: SHLVL=%q PWD=%q", env["SHLVL"], env["PWD"])
	}
}
