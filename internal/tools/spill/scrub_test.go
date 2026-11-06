package spill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A value split across two read chunks is still scrubbed, and nothing else moves.
func TestScrubStream_SecretAcrossChunkBoundary(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-VALUE"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[R]") }
	for _, at := range []int{scrubChunkBytes - 5, scrubChunkBytes, scrubChunkBytes - scrubOverlapBytes - 3, 2*scrubChunkBytes - 1} {
		in := strings.Repeat("a", at) + secret + strings.Repeat("b", 100)
		var out bytes.Buffer
		if err := scrubStream(strings.NewReader(in), &out, redact); err != nil {
			t.Fatal(err)
		}
		if want := strings.Repeat("a", at) + "[R]" + strings.Repeat("b", 100); out.String() != want {
			t.Errorf("secret at %d: got %d bytes, secret left=%v", at, out.Len(), strings.Contains(out.String(), secret))
		}
	}
}

func TestScrubFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "spill.tmp")
	if err := os.WriteFile(path, []byte("a SECRET b"), 0o600); err != nil {
		t.Fatal(err)
	}
	redact := func(s string) string { return strings.ReplaceAll(s, "SECRET", "[R]") }

	// A failed scrub reports the error, so the caller knows the file is still raw.
	if err := os.WriteFile(path+".scrub", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ScrubFile(path, redact); err == nil {
		t.Error("ScrubFile returned nil, want an error when the scrub file cannot be created")
	}
	if err := os.Remove(path + ".scrub"); err != nil {
		t.Fatal(err)
	}

	if err := ScrubFile(path, redact); err != nil {
		t.Fatalf("ScrubFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a [R] b" {
		t.Errorf("file = %q, want the scrubbed form", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	if _, err := os.Stat(path + ".scrub"); !os.IsNotExist(err) {
		t.Errorf("the scrub temp file is still there: %v", err)
	}
}
