package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSessionMetadataKeysComplete proves the registry is the single source of
// truth: every MetaKey* constant declared in this package appears in
// SessionMetadataKeys, and every registry entry is backed by one of those
// constants — so a new key added without registering (or a registry entry with
// no constant) fails the build.
func TestSessionMetadataKeysComplete(t *testing.T) {
	consts := metaKeyConstants(t)
	if len(consts) == 0 {
		t.Fatal("no MetaKey* constants found in package source")
	}
	registered := make(map[string]bool, len(SessionMetadataKeys))
	for _, mk := range SessionMetadataKeys {
		registered[mk.Key] = true
	}
	for name, value := range consts {
		if !registered[value] {
			t.Errorf("%s = %q is missing from SessionMetadataKeys", name, value)
		}
	}
	for _, mk := range SessionMetadataKeys {
		backed := false
		for _, value := range consts {
			if value == mk.Key {
				backed = true
				break
			}
		}
		if !backed {
			t.Errorf("SessionMetadataKeys entry %q has no MetaKey* constant", mk.Key)
		}
	}
}

// TestSessionMetadataKeysNoDuplicates proves no key is registered twice and no
// two MetaKey* constants collide on the same storage string, which would make
// the registry order ambiguous.
func TestSessionMetadataKeysNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(SessionMetadataKeys))
	for _, mk := range SessionMetadataKeys {
		if seen[mk.Key] {
			t.Errorf("duplicate registry entry for key %q", mk.Key)
		}
		seen[mk.Key] = true
	}
	byValue := make(map[string]string, len(metaKeyConstants(t)))
	for name, value := range metaKeyConstants(t) {
		if other, ok := byValue[value]; ok {
			t.Errorf("%s and %s share the same value %q", name, other, value)
		}
		byValue[value] = name
	}
}

// TestSessionMetadataKeysDisplayOrder pins the /sessions info display contract
// at the registry: the Shown entries, in slice order, must equal the exact
// legacy knownSessionMetadataKeys list (same keys, same order, same unset
// renderings, same branch-only flag) so the command's output is unchanged.
func TestSessionMetadataKeysDisplayOrder(t *testing.T) {
	legacy := []struct {
		key        string
		unset      string
		branchOnly bool
	}{
		{"model", "null", false},
		{"model_endpoint", "null", false},
		{"model_format", "null", false},
		{"effort", "null", false},
		{"permission_mode", "null", false},
		{"cc_resume_id", "null", false},
		{"last_activity", "null", false},
		{"no_compact", "false", false},
		{"display_show_thinking", "false", false},
		{"orientation_consumed", "false", true},
	}
	var shown []SessionMetadataKey
	for _, mk := range SessionMetadataKeys {
		if mk.Shown {
			shown = append(shown, mk)
		}
	}
	if len(shown) != len(legacy) {
		t.Fatalf("registry lists %d Shown keys, legacy display contract has %d", len(shown), len(legacy))
	}
	for i, want := range legacy {
		got := shown[i]
		if got.Key != want.key || got.Unset != want.unset || got.BranchOnly != want.branchOnly {
			t.Errorf("Shown key %d = {%q %q branchOnly=%v}, want {%q %q branchOnly=%v}",
				i, got.Key, got.Unset, got.BranchOnly, want.key, want.unset, want.branchOnly)
		}
	}
}

// TestSessionMetadataKeyValuesStable proves the constants are byte-for-byte
// the key strings persisted before the registry existed, so every converted
// call site keeps writing identical data (no renames, no migration).
func TestSessionMetadataKeyValuesStable(t *testing.T) {
	stable := []struct {
		name string
		got  string
		want string
	}{
		{"MetaKeyModel", MetaKeyModel, "model"},
		{"MetaKeyModelEndpoint", MetaKeyModelEndpoint, "model_endpoint"},
		{"MetaKeyModelFormat", MetaKeyModelFormat, "model_format"},
		{"MetaKeyEffort", MetaKeyEffort, "effort"},
		{"MetaKeyThinking", MetaKeyThinking, "thinking"},
		{"MetaKeySpeed", MetaKeySpeed, "speed"},
		{"MetaKeyShowToolCalls", MetaKeyShowToolCalls, "show_tool_calls"},
		{"MetaKeyDisplayShowThinking", MetaKeyDisplayShowThinking, "display_show_thinking"},
		{"MetaKeyStreamOutput", MetaKeyStreamOutput, "stream_output"},
		{"MetaKeyDisplayWidth", MetaKeyDisplayWidth, "display_width"},
		{"MetaKeyPermissionMode", MetaKeyPermissionMode, "permission_mode"},
		{"MetaKeyNoCompact", MetaKeyNoCompact, "no_compact"},
		{"MetaKeyCCResumeID", MetaKeyCCResumeID, "cc_resume_id"},
		{"MetaKeyCCUndelivered", MetaKeyCCUndelivered, "cc_undelivered"},
		{"MetaKeyOrientationConsumed", MetaKeyOrientationConsumed, "orientation_consumed"},
		{"MetaKeyLastActivity", MetaKeyLastActivity, "last_activity"},
	}
	for _, s := range stable {
		if s.got != s.want {
			t.Errorf("%s = %q, must stay %q (storage format — renaming requires a migration)", s.name, s.got, s.want)
		}
	}
}

// TestSessionMetadataCallSitesUseRegistry mechanically enforces requirement 2:
// no non-test call to the session_metadata API may pass a bare string literal
// as the key — keys flow from the MetaKey* registry constants only.
func TestSessionMetadataCallSitesUseRegistry(t *testing.T) {
	if len(SessionMetadataKeys) == 0 {
		t.Fatal("SessionMetadataKeys registry is empty — no keys to enforce")
	}
	// Root at the module root: walk up from the test's working directory (go
	// test runs in the package dir) to go.mod. Not runtime.Caller: tests are
	// built with -trimpath (scripts/seal-test.sh, shared test cache), which
	// turns this file's path into "foci/internal/session/..." — a walk of a
	// directory that does not exist. A RELATIVE root also keeps the files this
	// walk reads portable in go test's cache key across checkouts.
	root := "."
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		if i == 8 {
			t.Fatal("no go.mod above the package directory — cannot locate module root")
		}
		root = filepath.Join(root, "..")
	}
	keyArgIndex := map[string]int{
		"SetSessionMetadata":      1, // (sessionKey, key, value)
		"GetSessionMetadata":      1, // (sessionKey, key)
		"DeleteSessionMetadata":   1, // (sessionKey, key)
		"SessionKeysWithMetadata": 0, // (key)
	}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Never skip the root itself: its base is "." or "..", which the
			// dot-dir rule below would otherwise prune the whole walk with.
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx, ok := keyArgIndex[sel.Sel.Name]
			if !ok || idx >= len(call.Args) {
				return true
			}
			if lit, ok := call.Args[idx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				offenders = append(offenders, fset.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module sources: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s: %s", o, "string-literal session_metadata key — use a session.MetaKey* constant")
	}
}

// metaKeyConstants parses this package's non-test Go files and returns every
// MetaKey* string constant declared in them, keyed by constant name. Parsing
// the source (rather than a hand-maintained list) means a newly added
// constant is automatically subject to the completeness checks.
func metaKeyConstants(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	consts := make(map[string]string)
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "MetaKey") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							consts[id.Name] = v
						}
					}
				}
			}
		}
	}
	return consts
}
