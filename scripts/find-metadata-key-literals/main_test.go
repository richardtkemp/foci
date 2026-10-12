package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const fixture = `package x

func f(s store, k string) {
	s.SetSessionMetadata("sk", "model", "v")        // literal: flagged
	s.GetSessionMetadata("sk", "effort")            // literal: flagged
	s.DeleteSessionMetadata("sk", ` + "`raw`" + `)  // raw-string literal: flagged
	s.SessionKeysWithMetadata("cc_resume_id")       // literal: flagged
	s.SetSessionMetadata("sk", session.MetaKeyModel, "v") // constant: ok
	s.GetSessionMetadata("sk", k)                   // variable: ok
	s.SessionKeysWithMetadata(session.MetaKeyEffort) // constant: ok
	s.SetOther("sk", "model", "v")                  // other API: ok
	s.SetSessionMetadata("model")                   // too few args: ok
}
`

// TestScanFileFlagsLiteralKeys is the fail-arm: every bare string literal in
// the key position is caught, and constants, variables and other methods are
// not.
func TestScanFileFlagsLiteralKeys(t *testing.T) {
	got, err := scanFile("x.go", []byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"x.go:4:2", "x.go:5:2", "x.go:6:2", "x.go:7:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n got %v\nwant %v", got, want)
	}
}

// TestScanTreeSkipsTestsAndExcludedDirs proves the walk checks ordinary
// source but ignores _test.go files and the dot/vendor/testdata/bin dirs.
func TestScanTreeSkipsTestsAndExcludedDirs(t *testing.T) {
	root := t.TempDir()
	bad := []byte("package x\n\nfunc f(s s) { s.GetSessionMetadata(\"sk\", \"model\") }\n")
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/a/a.go")
	write("internal/a/a_test.go")
	write(".git/x.go")
	write("vendor/v/v.go")
	write("internal/a/testdata/t.go")
	write("bin/b.go")

	got, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "internal/a/a.go") + ":3:15"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n got %v\nwant %v", got, want)
	}
}

// TestRunExitCodes pins the lint contract: 1 on a finding, 0 when clean.
func TestRunExitCodes(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.go")
	if err := os.WriteFile(p, []byte("package x\n\nfunc f(s s) { s.SessionKeysWithMetadata(k) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{root}); code != 0 {
		t.Fatalf("clean tree: exit %d, want 0", code)
	}
	if err := os.WriteFile(p, []byte("package x\n\nfunc f(s s) { s.SessionKeysWithMetadata(\"k\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{root}); code != 1 {
		t.Fatalf("literal key: exit %d, want 1", code)
	}
}
