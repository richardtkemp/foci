package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writePkg writes src files into a fresh dir and returns it.
func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const cleanKeys = `package session

const (
	MetaKeyA = "a"
	MetaKeyB = "b"
)
`

const cleanRegistry = `package session

type SessionMetadataKey struct {
	Key   string
	Shown bool
}

var SessionMetadataKeys = []SessionMetadataKey{
	{Key: MetaKeyA, Shown: true},
	{Key: MetaKeyB},
}
`

func findings(t *testing.T, files map[string]string) []string {
	t.Helper()
	info, err := parseDir(writePkg(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return check(info)
}

// TestCleanPackage is the pass-arm: constants and registry agree, and a
// _test.go file declaring a stray MetaKey* constant is ignored.
func TestCleanPackage(t *testing.T) {
	got := findings(t, map[string]string{
		"keys.go":     cleanKeys,
		"registry.go": cleanRegistry,
		"x_test.go":   "package session\n\nconst MetaKeyTestOnly = \"t\"\n",
	})
	if len(got) != 0 {
		t.Fatalf("clean package: unexpected findings %v", got)
	}
}

// TestUnregisteredConstant: a MetaKey* constant missing from the registry
// (TestSessionMetadataKeysComplete's first half).
func TestUnregisteredConstant(t *testing.T) {
	got := findings(t, map[string]string{
		"keys.go":     cleanKeys + "\nconst MetaKeyC = \"c\"\n",
		"registry.go": cleanRegistry,
	})
	want := []string{`MetaKeyC = "c" is missing from SessionMetadataKeys`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// TestUnbackedEntry: registry entries not backed by a MetaKey* constant
// (TestSessionMetadataKeysComplete's second half) — a string literal with no
// matching constant, an identifier that is not a MetaKey* constant, and an
// entry with no Key at all.
func TestUnbackedEntry(t *testing.T) {
	reg := strings.Replace(cleanRegistry, "{Key: MetaKeyB},",
		"{Key: MetaKeyB},\n\t{Key: \"orphan\"},\n\t{Key: otherKey},\n\t{Shown: true},", 1)
	got := findings(t, map[string]string{"keys.go": cleanKeys, "registry.go": reg})
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %v", got)
	}
	for _, want := range []string{
		`entry "orphan" has no MetaKey* constant`,
		`entry Key is otherKey, not a MetaKey* constant`,
		`entry Key is no Key, not a MetaKey* constant`,
	} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("missing finding %q in %v", want, got)
		}
	}
}

// TestDuplicates: a key listed twice in the registry, and two constants
// sharing a value (TestSessionMetadataKeysNoDuplicates).
func TestDuplicates(t *testing.T) {
	reg := strings.Replace(cleanRegistry, "{Key: MetaKeyB},", "{Key: MetaKeyB},\n\t{Key: MetaKeyA},", 1)
	got := findings(t, map[string]string{"keys.go": cleanKeys, "registry.go": reg})
	if len(got) != 1 || !strings.Contains(got[0], `duplicate SessionMetadataKeys entry for key "a"`) {
		t.Fatalf("registry duplicate: got %v", got)
	}

	got = findings(t, map[string]string{
		"keys.go":     cleanKeys + "\nconst MetaKeyAlias = \"a\"\n",
		"registry.go": cleanRegistry,
	})
	want := []string{`MetaKeyA and MetaKeyAlias share the same value "a"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("constant collision: got %v\nwant %v", got, want)
	}
}

// TestRunExitCodes pins the lint contract: 0 clean, 1 drift, 2 when the
// package has no constants or no registry (the old test's t.Fatal).
func TestRunExitCodes(t *testing.T) {
	clean := writePkg(t, map[string]string{"keys.go": cleanKeys, "registry.go": cleanRegistry})
	if code := run([]string{clean}); code != 0 {
		t.Fatalf("clean: exit %d, want 0", code)
	}
	drift := writePkg(t, map[string]string{"keys.go": cleanKeys + "\nconst MetaKeyC = \"c\"\n", "registry.go": cleanRegistry})
	if code := run([]string{drift}); code != 1 {
		t.Fatalf("drift: exit %d, want 1", code)
	}
	noConsts := writePkg(t, map[string]string{"registry.go": cleanRegistry})
	if code := run([]string{noConsts}); code != 2 {
		t.Fatalf("no constants: exit %d, want 2", code)
	}
	noRegistry := writePkg(t, map[string]string{"keys.go": cleanKeys})
	if code := run([]string{noRegistry}); code != 2 {
		t.Fatalf("no registry: exit %d, want 2", code)
	}
}
