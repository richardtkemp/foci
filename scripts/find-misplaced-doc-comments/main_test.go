package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixture reproduces the #2915 incidents (#2879: a doc glued onto a newly
// inserted function and const) next to the shapes that must stay clean.
const fixture = `package x

// beginTurn starts a turn.
func emitLiveModel() {}

func beginTurn() {}

// setOutcome records the outcome.
func (s *S) setModel() {}

func (s *S) setOutcome() {}

// S is fine.
type S struct{}

// S.method is fine: a receiver qualifier is dropped.
func (s *S) method() {}

// (*S).other() is fine too.
func (s *S) other() {}

// limit bounds things.
const window = 3

const limit = 2

const (
	// beginTurn names a func, inside a group: flagged.
	grouped = 1
	// grouped2 is fine.
	grouped2 = 2
)

// Deprecated: prose that names no symbol is not flagged.
func old() {}

// A S value is prose too.
var v S

// TestBeginTurn names the function under test: fine for a test.
func TestEmit(t *T) {}

// TestEmit names another test: flagged.
func TestOther(t *T) {}

//go:generate echo directive-only docs are skipped
func gen() {}
`

// TestScanFlagsMisplacedDocs is the fail-arm: each glued doc is caught, and
// the clean shapes are not.
func TestScanFlagsMisplacedDocs(t *testing.T) {
	ds := &dirScan{symbols: map[string]bool{}}
	if err := scanFile("x.go", []byte(fixture), ds); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`x.go:4:6: doc of emitLiveModel opens with "beginTurn", another symbol in this package`,
		`x.go:9:13: doc of setModel opens with "setOutcome", another symbol in this package`,
		`x.go:23:7: doc of window opens with "limit", another symbol in this package`,
		`x.go:29:2: doc of grouped opens with "beginTurn", another symbol in this package`,
		`x.go:44:6: doc of TestOther opens with "TestEmit", another symbol in this package`,
	}
	got := ds.findings()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n got %q\nwant %q", got, want)
	}
}

// TestScanTreeSeesSiblingFilesAndTests proves symbols are pooled per
// directory (a doc naming a function in a sibling file is caught), _test.go
// files are checked, and excluded dirs are skipped.
func TestScanTreeSeesSiblingFilesAndTests(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("p/a.go", "package p\n\nfunc fetchFixture() {}\n")
	write("p/a_test.go", "package p\n\n// fetchFixture loads a fixture.\nfunc testFetchParseTimeout() {}\n")
	write("p/testdata/b.go", "package p\n\n// fetchFixture x\nfunc skipped() {}\n")
	write("q/c.go", "package q\n\n// fetchFixture is another package's name: fine.\nfunc local() {}\n")

	got, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "p/a_test.go") + `:4:6: doc of testFetchParseTimeout opens with "fetchFixture", another symbol in this package`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n got %q\nwant %q", got, want)
	}
}
