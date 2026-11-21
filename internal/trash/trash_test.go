package trash

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMoveFreedesktop(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("freedesktop layout is the non-darwin path")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg"))
	at := func() time.Time { return time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC) }

	mk := func() string {
		p := filepath.Join(root, "work space")
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "sub", "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first, err := Move(mk(), at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Move(mk(), at)
	if err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(root, "xdg", "Trash")
	if first != filepath.Join(trashDir, "files", "work space") || second != filepath.Join(trashDir, "files", "work space.2") {
		t.Fatalf("got %q, %q", first, second)
	}
	if _, err := os.Stat(filepath.Join(second, "sub", "f.txt")); err != nil {
		t.Fatalf("content not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work space")); !os.IsNotExist(err) {
		t.Fatalf("original still there: %v", err)
	}
	info, err := os.ReadFile(filepath.Join(trashDir, "info", "work space.2.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Trash Info]\nPath=" + strings.ReplaceAll(filepath.Join(root, "work space"), " ", "%20") + "\nDeletionDate=2026-11-21T09:30:00\n"
	if string(info) != want {
		t.Fatalf("trashinfo:\n%s\nwant:\n%s", info, want)
	}
}

func TestMoveMissingFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg"))
	if _, err := Move(filepath.Join(root, "nope"), nil); err == nil {
		t.Fatal("moving a missing path succeeded")
	}
}

func TestMoveDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("~/.Trash is the darwin path")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	for i, want := range []string{"ws", "ws.2"} {
		p := filepath.Join(root, "ws")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := Move(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(root, ".Trash", want) {
			t.Fatalf("move %d: got %q", i, got)
		}
	}
}
