package preload

import (
	"os"
	"path/filepath"
	"testing"
)

// writeShim creates a fake nosgid.so under a temp HOME and points HOME at it.
func writeShim(t *testing.T) (home, so string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	so = filepath.Join(home, RelPath)
	if err := os.MkdirAll(filepath.Dir(so), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(so, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home, so
}

func TestApply_SetsPreloadWhenShimPresent(t *testing.T) {
	_, so := writeShim(t)
	t.Setenv("LD_PRELOAD", "")

	Apply()

	if got := os.Getenv("LD_PRELOAD"); got != so {
		t.Fatalf("LD_PRELOAD = %q, want %q", got, so)
	}
}

func TestApply_PrependsToExisting(t *testing.T) {
	_, so := writeShim(t)
	t.Setenv("LD_PRELOAD", "/other/lib.so")

	Apply()

	want := so + " /other/lib.so"
	if got := os.Getenv("LD_PRELOAD"); got != want {
		t.Fatalf("LD_PRELOAD = %q, want %q", got, want)
	}
}

func TestApply_IdempotentWhenAlreadyPresent(t *testing.T) {
	_, so := writeShim(t)
	pre := so + " /other/lib.so"
	t.Setenv("LD_PRELOAD", pre)

	Apply()

	if got := os.Getenv("LD_PRELOAD"); got != pre {
		t.Fatalf("LD_PRELOAD = %q, want unchanged %q", got, pre)
	}
}

func TestApply_NoopWhenShimMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no .lib/nosgid.so created
	t.Setenv("LD_PRELOAD", "/keep.so")

	Apply()

	if got := os.Getenv("LD_PRELOAD"); got != "/keep.so" {
		t.Fatalf("LD_PRELOAD = %q, want unchanged /keep.so", got)
	}
}
