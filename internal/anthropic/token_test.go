package anthropic

import (
	"os"
	"testing"
)

func TestExpandHome(t *testing.T) {
	// Proves that expandHome replaces a leading ~ with the actual home directory path, and leaves paths that do not start with ~ unchanged.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	got := expandHome("~/test/path")
	want := home + "/test/path"
	if got != want {
		t.Errorf("expandHome(~/test/path) = %q, want %q", got, want)
	}

	// Non-~ path unchanged
	got = expandHome("/absolute/path")
	if got != "/absolute/path" {
		t.Errorf("expandHome(/absolute/path) = %q", got)
	}
}
