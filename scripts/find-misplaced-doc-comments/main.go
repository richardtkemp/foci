// Command find-misplaced-doc-comments flags a top-level doc comment whose first
// word names a DIFFERENT top-level symbol of the same package than the one it
// documents (#2915).
//
// The defect it catches: a new function, type, const or var is inserted
// between an existing doc comment and the symbol it documented. Go then glues
// the old doc onto the new symbol, and the old symbol silently loses its doc:
//
//	// beginTurn starts a turn ...      <- old doc, now attached to emitLiveModel
//	func emitLiveModel() { ... }
//
//	func beginTurn() { ... }            <- undocumented
//
// gofmt, vet and golangci-lint all pass this. The check:
//
//   - every .go file under the root, _test.go included (golangci runs with
//     tests: false, and one of the incidents behind #2915 was in a test);
//     dot-dirs, vendor, testdata and bin are skipped;
//   - every top-level func or method, type spec, and single-name var/const
//     spec with a doc comment;
//   - the doc's first word, with trailing punctuation and "()" removed and a
//     "T." or "(*T)." qualifier dropped, is compared with the symbol's name;
//   - a mismatch is a finding only when that word is itself the name of a
//     top-level symbol (func, method, type, var or const) in the same
//     directory. A doc that opens with ordinary prose ("Deprecated:", "A ...",
//     "Invariant names ...") is not flagged: that is style, not a misplaced doc.
//
// One exemption: the doc of a Test/Benchmark/Fuzz/Example function usually
// opens with the name of the function under test, so for those only a first
// word that is ANOTHER test entry point is flagged.
//
// Not covered: a misplaced doc whose first word happens to be the new symbol's
// own name, and a doc block that names a different function in a later
// paragraph (measured on 2026-11-08: 67 such lines, all legitimate prose).
//
// Usage: find-misplaced-doc-comments [root] (default ".", the repo root)
//
// Exit codes: 0 = clean, 1 = misplaced docs found, 2 = tool error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fset := flag.NewFlagSet("find-misplaced-doc-comments", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: find-misplaced-doc-comments [root]")
		fmt.Fprintln(os.Stderr, "Flags top-level doc comments whose first word names a different symbol of the same package.")
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	root := "."
	switch fset.NArg() {
	case 0:
	case 1:
		root = fset.Arg(0)
	default:
		fset.Usage()
		return 2
	}

	findings, err := scanTree(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-misplaced-doc-comments: %v\n", err)
		return 2
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Printf("\n%d doc comment(s) open with another symbol's name: the doc is glued to the wrong declaration (a symbol was inserted between a doc and its function?). Move the doc back, or start it with the documented symbol's name.\n", len(findings))
		return 1
	}
	return 0
}

// documented is one top-level declaration that carries a doc comment.
type documented struct {
	pos       token.Position
	name      string
	firstWord string
}

// dirScan collects one directory's top-level symbol names (all files, test
// files and external test packages included) and its documented declarations.
type dirScan struct {
	symbols map[string]bool
	docs    []documented
}

// scanTree walks root and returns one finding line per misplaced doc, sorted.
func scanTree(root string) ([]string, error) {
	dirs := map[string]*dirScan{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Never prune the root itself: its base may be "." or "..".
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		ds := dirs[dir]
		if ds == nil {
			ds = &dirScan{symbols: map[string]bool{}}
			dirs[dir] = ds
		}
		return scanFile(path, src, ds)
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ds := range dirs {
		out = append(out, ds.findings()...)
	}
	sort.Strings(out)
	return out, nil
}

// scanFile parses one Go file and adds its top-level symbols and documented
// declarations to ds.
func scanFile(path string, src []byte, ds *dirScan) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return err
	}
	add := func(id *ast.Ident, doc *ast.CommentGroup) {
		ds.symbols[id.Name] = true
		if doc == nil {
			return
		}
		if w := docFirstWord(doc); w != "" {
			ds.docs = append(ds.docs, documented{pos: fset.Position(id.Pos()), name: id.Name, firstWord: w})
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Name, d.Doc)
		case *ast.GenDecl:
			// An unparenthesised decl's doc sits on the GenDecl; a grouped
			// one's docs sit on each spec. The group's own doc describes the
			// group, not its first member.
			for _, spec := range d.Specs {
				doc := d.Doc
				if d.Lparen.IsValid() {
					doc = nil
				}
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Doc != nil {
						doc = s.Doc
					}
					add(s.Name, doc)
				case *ast.ValueSpec:
					if s.Doc != nil {
						doc = s.Doc
					}
					if len(s.Names) != 1 {
						doc = nil // "a, b = ..." has no single name to compare with
					}
					for _, n := range s.Names {
						if n.Name != "_" {
							add(n, doc)
						}
					}
				}
			}
		}
	}
	return nil
}

// findings returns "file:line:col: <symbol>'s doc opens with <word>" for every
// documented declaration whose first word names another symbol of the dir.
func (ds *dirScan) findings() []string {
	var out []string
	for _, d := range ds.docs {
		if d.firstWord == d.name || !ds.symbols[d.firstWord] {
			continue
		}
		if isTestEntry(d.name) && !isTestEntry(d.firstWord) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: doc of %s opens with %q, another symbol in this package", d.pos, d.name, d.firstWord))
	}
	return out
}

// docFirstWord returns the first word of a doc comment, normalised for
// comparison with a symbol name: trailing punctuation and "()" removed, and a
// receiver qualifier ("T.", "(*T).") dropped. Directive-only docs
// ("//go:generate ...", "//nolint") yield "": CommentGroup.Text strips them.
func docFirstWord(doc *ast.CommentGroup) string {
	fields := strings.Fields(doc.Text())
	if len(fields) == 0 {
		return ""
	}
	w := strings.TrimRight(fields[0], ".,:;!?")
	w = strings.TrimSuffix(w, "()")
	if i := strings.LastIndex(w, "."); i >= 0 {
		w = w[i+1:]
	}
	return w
}

// isTestEntry reports whether name is a go test entry point.
func isTestEntry(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
