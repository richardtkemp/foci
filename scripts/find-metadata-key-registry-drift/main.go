// Command find-metadata-key-registry-drift checks that internal/session's
// session_metadata key registry and its MetaKey* constants agree:
//
//   - every MetaKey* string constant is listed in SessionMetadataKeys;
//   - every SessionMetadataKeys entry's Key is backed by a MetaKey* constant;
//   - no key is listed in SessionMetadataKeys twice;
//   - no two MetaKey* constants share the same value.
//
// Both sides are read from the package's non-test .go source, so a newly
// added constant is checked without maintaining a second list.
//
// Why a lint tool rather than a go test: these checks used to be
// internal/session's TestSessionMetadataKeysComplete and
// TestSessionMetadataKeysNoDuplicates, which parsed the package's own source
// at test time. Go then keyed that package's cached test result on each
// file's mode and mtime and on the directory, so every fresh checkout
// re-ran the whole package (#2353). Runs as part of `make lint`.
// Stdlib-only, so it lives in the main module.
//
// Usage: find-metadata-key-registry-drift [dir] (default "internal/session")
//
// Exit codes: 0 = consistent, 1 = drift found, 2 = tool error (including no
// MetaKey* constants or no SessionMetadataKeys registry in dir).
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	constPrefix  = "MetaKey"
	registryName = "SessionMetadataKeys"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fset := flag.NewFlagSet("find-metadata-key-registry-drift", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: find-metadata-key-registry-drift [dir]  (default internal/session)")
		fmt.Fprintln(os.Stderr, "Checks every MetaKey* constant is in SessionMetadataKeys, every entry is backed by one, and nothing is duplicated.")
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	dir := "internal/session"
	switch fset.NArg() {
	case 0:
	case 1:
		dir = fset.Arg(0)
	default:
		fset.Usage()
		return 2
	}

	pkg, err := parseDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-metadata-key-registry-drift: %v\n", err)
		return 2
	}
	findings := check(pkg)
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Printf("\n%d session_metadata key registry problem(s) in %s\n", len(findings), dir)
		return 1
	}
	return 0
}

// registryEntry is one SessionMetadataKeys element as found in source.
type registryEntry struct {
	pos   string // file:line:col of the element
	value string // resolved storage key ("" when unresolved)
	desc  string // how the Key was written, for messages
	ok    bool   // value resolved (MetaKey* ident or string literal)
}

// pkgInfo is what the checks need from the package source.
type pkgInfo struct {
	consts   map[string]string // MetaKey* name -> value
	registry []registryEntry
	found    bool // a SessionMetadataKeys composite literal was seen
}

// parseDir parses dir's non-test .go files and extracts the MetaKey* string
// constants and the SessionMetadataKeys registry literal.
func parseDir(dir string) (*pkgInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}

	info := &pkgInfo{consts: make(map[string]string)}
	for _, f := range files {
		collectConsts(f, info.consts)
	}
	for _, f := range files {
		collectRegistry(fset, f, info)
	}
	if len(info.consts) == 0 {
		return nil, fmt.Errorf("no %s* string constants found in %s", constPrefix, dir)
	}
	if !info.found {
		return nil, fmt.Errorf("no %s composite literal found in %s", registryName, dir)
	}
	return info, nil
}

// collectConsts records every top-level MetaKey* constant whose value is a
// string literal.
func collectConsts(f *ast.File, out map[string]string) {
	for _, decl := range f.Decls {
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
				if !strings.HasPrefix(id.Name, constPrefix) || i >= len(vs.Values) {
					continue
				}
				if v, ok := stringLit(vs.Values[i]); ok {
					out[id.Name] = v
				}
			}
		}
	}
}

// collectRegistry finds `var SessionMetadataKeys = []T{...}` and records
// each element's Key.
func collectRegistry(fset *token.FileSet, f *ast.File, info *pkgInfo) {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != registryName || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				info.found = true
				for _, elt := range lit.Elts {
					info.registry = append(info.registry, entryFor(fset, elt, info.consts))
				}
			}
		}
	}
}

// entryFor resolves one registry element's Key: a keyed `Key:` field, else
// the first positional field.
func entryFor(fset *token.FileSet, elt ast.Expr, consts map[string]string) registryEntry {
	e := registryEntry{pos: fset.Position(elt.Pos()).String(), desc: "no Key"}
	cl, ok := elt.(*ast.CompositeLit)
	if !ok {
		e.desc = "not a composite literal"
		return e
	}
	var keyExpr ast.Expr
	for i, field := range cl.Elts {
		if kv, ok := field.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Key" {
				keyExpr = kv.Value
			}
		} else if i == 0 {
			keyExpr = field
		}
	}
	if keyExpr == nil {
		return e
	}
	switch x := keyExpr.(type) {
	case *ast.Ident:
		e.desc = x.Name
		if v, ok := consts[x.Name]; ok {
			e.value, e.ok = v, true
		}
	case *ast.BasicLit:
		e.desc = x.Value
		if v, ok := stringLit(x); ok {
			e.value, e.ok = v, true
		}
	default:
		e.desc = "an unresolvable expression"
	}
	return e
}

func stringLit(x ast.Expr) (string, bool) {
	lit, ok := x.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// check returns one message per violation, sorted for stable output.
func check(info *pkgInfo) []string {
	var out []string

	registered := make(map[string]bool, len(info.registry))
	seen := make(map[string]bool, len(info.registry))
	for _, e := range info.registry {
		if !e.ok {
			out = append(out, fmt.Sprintf("%s: %s entry Key is %s, not a %s* constant", e.pos, registryName, e.desc, constPrefix))
			continue
		}
		registered[e.value] = true
		if seen[e.value] {
			out = append(out, fmt.Sprintf("%s: duplicate %s entry for key %q", e.pos, registryName, e.value))
		}
		seen[e.value] = true
	}

	constValues := make(map[string]bool, len(info.consts))
	for _, v := range info.consts {
		constValues[v] = true
	}
	for _, e := range info.registry {
		if e.ok && !constValues[e.value] {
			out = append(out, fmt.Sprintf("%s: %s entry %q has no %s* constant", e.pos, registryName, e.value, constPrefix))
		}
	}

	names := make([]string, 0, len(info.consts))
	for name := range info.consts {
		names = append(names, name)
	}
	sort.Strings(names)
	byValue := make(map[string]string, len(names))
	for _, name := range names {
		v := info.consts[name]
		if !registered[v] {
			out = append(out, fmt.Sprintf("%s = %q is missing from %s", name, v, registryName))
		}
		if other, ok := byValue[v]; ok {
			out = append(out, fmt.Sprintf("%s and %s share the same value %q", other, name, v))
		}
		byValue[v] = name
	}
	sort.Strings(out)
	return out
}
