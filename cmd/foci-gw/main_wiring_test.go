package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePath reads the module path from go.mod, so the import paths this test
// derives for scanned packages use the same source of truth as the code.
func modulePath(t *testing.T, repoRoot string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if mod, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(mod)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

// scanGoFiles parses every non-test .go file under root (skipping testdata)
// and calls visit with each parsed file and the import path pkgPathOf derives
// for it.
func scanGoFiles(t *testing.T, root string, pkgPathOf func(fp string) string, visit func(f *ast.File, pkgPath string)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(fp, ".go") || strings.HasSuffix(fp, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, fp, nil, 0)
		if err != nil {
			return err
		}
		visit(f, pkgPathOf(fp))
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
}

// callsRegisterMessagingProvider reports whether f's declarations call
// platform.RegisterMessagingProvider anywhere (init directly, or a helper init
// calls — the registration site is an implementation detail of the transport).
func callsRegisterMessagingProvider(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RegisterMessagingProvider" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "platform" {
				found = true
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// TestMainLinksEveryMessagingProvider pins the gateway's wiring: every
// messaging provider package shipped under internal/ must be imported by a
// non-test file of package main, so its init registers the transport in the
// production binary. A provider linked only by tests (or by nothing) would
// silently vanish from the gateway — config validation would still accept the
// platform id, and nothing would ever connect. Slack's only production import
// is main.go's blank import (#2459 A1).
//
// disconnected-test-ok: asserts package main's source wiring — which import
// paths the package's non-test files declare. The assertion is about the
// imports themselves, so it cannot reference a production identifier.
func TestMainLinksEveryMessagingProvider(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	mod := modulePath(t, repoRoot)
	providers := []string{
		mod + "/internal/app",
		mod + "/internal/discord",
		mod + "/internal/slack",
		mod + "/internal/telegram",
	}

	// The transports the repository ships: every non-test package under
	// internal/ that registers a messaging provider.
	var pkgs []string
	seen := map[string]bool{}
	internalRoot := filepath.Join(repoRoot, "internal")
	scanGoFiles(t, internalRoot, func(fp string) string {
		rel, err := filepath.Rel(repoRoot, filepath.Dir(fp))
		if err != nil {
			t.Fatal(err)
		}
		return path.Join(mod, filepath.ToSlash(rel))
	}, func(f *ast.File, pkgPath string) {
		if !seen[pkgPath] && callsRegisterMessagingProvider(f) {
			seen[pkgPath] = true
			pkgs = append(pkgs, pkgPath)
		}
	})
	sort.Strings(pkgs)
	if !slices.Equal(pkgs, providers) {
		t.Fatalf("messaging provider packages under internal/ = %v, want %v (update this list when a transport is added or removed)", pkgs, providers)
	}

	// Every one of them is linked into package main (the gateway binary):
	// blank or named, in any non-test file of this package.
	imported := map[string]bool{}
	scanGoFiles(t, ".", func(string) string { return "" }, func(f *ast.File, _ string) {
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			imported[p] = true
		}
	})
	for _, p := range providers {
		if !imported[p] {
			t.Errorf("package main does not import %s: the gateway binary would not register this messaging provider", p)
		}
	}
}
